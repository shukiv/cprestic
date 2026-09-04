package nodestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Basket is what somebody has chosen out of one restore point so far.
//
// Choosing runs across several pages -- the databases are one list and
// their users are another -- and the pages are plain requests with nothing
// remembered between them, because the recovery centre has to work with
// scripting switched off. So the choice is kept here rather than in the
// browser, against the restore point it was made from: a basket assembled
// out of Tuesday's backup is not a basket out of Monday's, and quietly
// carrying one over to the other would restore files nobody looked at.
type Basket struct {
	Account      string             `json:"account"`
	RepositoryID string             `json:"repository_id"`
	SnapshotID   string             `json:"snapshot_id"`
	Items        []RestoreSelection `json:"items,omitempty"`
	UpdatedAt    time.Time          `json:"updated_at"`
}

// BasketLife is how long an unfinished basket is kept. Long enough to
// answer the door and come back, short enough that a restore point which
// has since been forgotten is not still being offered.
const BasketLife = 24 * time.Hour

// Empty reports whether there is nothing in the basket.
func (b Basket) Empty() bool { return len(b.Items) == 0 }

// Count is how many individual things are in it. A category chosen whole
// counts as one, because that is what it restores.
func (b Basket) Count() int {
	total := 0
	for _, item := range b.Items {
		if len(item.Names) == 0 {
			total++
			continue
		}
		total += len(item.Names)
	}
	return total
}

func basketKey(account, repository, snapshot string) string {
	return account + "\x00" + repository + "\x00" + snapshot
}

// Basket reads what has been chosen out of one restore point. A basket
// nobody has started is empty rather than missing.
func (s *Store) Basket(account, repository, snapshot string) (Basket, error) {
	empty := Basket{Account: account, RepositoryID: repository, SnapshotID: snapshot}
	if account == "" || repository == "" || snapshot == "" {
		return empty, nil
	}
	var basket Basket
	err := s.get(bucketBaskets, basketKey(account, repository, snapshot), &basket)
	switch {
	case errors.Is(err, ErrNotFound):
		return empty, nil
	case err != nil:
		return empty, err
	}
	if time.Since(basket.UpdatedAt) > BasketLife {
		return empty, nil
	}
	return basket, nil
}

// PutInBasket adds one part of an account to the basket, replacing what
// was there for that same part.
//
// Choosing a category again replaces the earlier choice rather than adding
// to it, because the page it was chosen on shows what is ticked now: the
// two cannot be merged without the result disagreeing with the tick boxes
// that produced it.
func (s *Store) PutInBasket(account, repository, snapshot string,
	selection RestoreSelection) (Basket, error) {

	return s.changeBasket(account, repository, snapshot, func(basket *Basket) {
		basket.Items = withSelection(basket.Items, selection)
	})
}

// TakeFromBasket removes one part of an account from the basket.
func (s *Store) TakeFromBasket(account, repository, snapshot, kind string) (Basket, error) {
	return s.changeBasket(account, repository, snapshot, func(basket *Basket) {
		basket.Items = withoutKind(basket.Items, kind)
	})
}

// EmptyBasket forgets everything chosen out of one restore point.
func (s *Store) EmptyBasket(account, repository, snapshot string) error {
	err := s.delete(bucketBaskets, basketKey(account, repository, snapshot))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func withSelection(items []RestoreSelection, selection RestoreSelection) []RestoreSelection {
	kept := withoutKind(items, selection.Kind)
	sort.Strings(selection.Names)
	return append(kept, selection)
}

func withoutKind(items []RestoreSelection, kind string) []RestoreSelection {
	kept := make([]RestoreSelection, 0, len(items))
	for _, item := range items {
		if item.Kind != kind {
			kept = append(kept, item)
		}
	}
	return kept
}

// changeBasket applies one change under a single transaction, so two
// requests arriving together cannot each read the basket, add their own
// part and write the other's away.
func (s *Store) changeBasket(account, repository, snapshot string,
	change func(*Basket)) (Basket, error) {

	if account == "" || repository == "" || snapshot == "" {
		return Basket{}, fmt.Errorf("nodestore: a basket belongs to one restore point")
	}
	key := []byte(basketKey(account, repository, snapshot))
	basket := Basket{Account: account, RepositoryID: repository, SnapshotID: snapshot}

	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketBaskets)
		if raw := bucket.Get(key); raw != nil {
			var stored Basket
			if err := json.Unmarshal(raw, &stored); err == nil &&
				time.Since(stored.UpdatedAt) <= BasketLife {
				basket = stored
			}
		}
		change(&basket)
		basket.UpdatedAt = time.Now().UTC()

		// Baskets nobody finished. They are few and small, so they are
		// swept where they are written rather than on a timer.
		sweepBaskets(bucket, key)

		if basket.Empty() {
			return bucket.Delete(key)
		}
		encoded, err := json.Marshal(basket)
		if err != nil {
			return fmt.Errorf("nodestore: encode basket: %w", err)
		}
		return bucket.Put(key, encoded)
	})
	if err != nil {
		return Basket{}, err
	}
	return basket, nil
}

// sweepBaskets drops the ones nobody came back to.
func sweepBaskets(bucket *bolt.Bucket, except []byte) {
	var stale [][]byte
	_ = bucket.ForEach(func(key, raw []byte) error {
		if string(key) == string(except) {
			return nil
		}
		var basket Basket
		if err := json.Unmarshal(raw, &basket); err != nil ||
			time.Since(basket.UpdatedAt) > BasketLife {
			stale = append(stale, append([]byte(nil), key...))
		}
		return nil
	})
	for _, key := range stale {
		_ = bucket.Delete(key)
	}
}

// ForgetBaskets forgets every basket belonging to one account. A name that
// has changed hands must not hand the new owner a basket the last one
// left behind.
func (s *Store) ForgetBaskets(account string) error {
	prefix := account + "\x00"
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketBaskets)
		var gone [][]byte
		if err := bucket.ForEach(func(key, _ []byte) error {
			if strings.HasPrefix(string(key), prefix) {
				gone = append(gone, append([]byte(nil), key...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, key := range gone {
			if err := bucket.Delete(key); err != nil {
				return err
			}
		}
		return nil
	})
}
