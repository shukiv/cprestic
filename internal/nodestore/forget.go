package nodestore

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// ForgetAccount drops everything this server remembers about one account:
// the record that it ever existed, its backup and restore history, its
// lifecycle events and any half-made basket.
//
// This is what an operator asks for when a customer has gone and their
// backups have been removed from the destination as well. It is deliberate
// and irreversible: the history is the only place a finished job is
// recorded, and no other record can rebuild it.
//
// The backups themselves are not touched here. Removing snapshots is the
// destination's business and takes a repository lock; this is the local
// record, and the caller does both in the order that leaves nothing
// dangling -- snapshots first, so a failure there leaves the account still
// listed rather than a name with backups nobody can find.
func (s *Store) ForgetAccount(account string) error {
	if account == "" {
		return fmt.Errorf("nodestore: no account was named")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketIdentities).Delete([]byte(account)); err != nil {
			return err
		}
		if err := tx.Bucket(bucketAccounts).Delete([]byte(account)); err != nil {
			return err
		}
		for _, bucket := range [][]byte{bucketJobs, bucketRestores, bucketLifecycle} {
			if err := deleteWhereAccount(tx.Bucket(bucket), account); err != nil {
				return err
			}
		}
		return deleteBasketsOf(tx.Bucket(bucketBaskets), account)
	})
}

// deleteWhereAccount removes every document in a bucket that names this
// account. The documents differ; the field does not.
func deleteWhereAccount(bucket *bolt.Bucket, account string) error {
	var gone [][]byte
	err := bucket.ForEach(func(key, raw []byte) error {
		var named struct {
			Account string `json:"account"`
		}
		if err := json.Unmarshal(raw, &named); err != nil {
			// A document this cannot read is left alone. Deleting what
			// cannot be identified would take somebody else's history
			// with it.
			return nil
		}
		if named.Account == account {
			gone = append(gone, append([]byte(nil), key...))
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, key := range gone {
		if err := bucket.Delete(key); err != nil {
			return err
		}
	}
	return nil
}
