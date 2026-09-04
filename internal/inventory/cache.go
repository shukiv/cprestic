package inventory

import (
	"context"
	"path"
	"sync"
	"time"

	"github.com/shuki/cprest/internal/granular"
)

// Cache reads a snapshot's containers once and remembers what was in them.
//
// The account's metadata archive runs to tens of megabytes and lives in the
// repository, which may be across a network. Somebody looking through a
// backup clicks between its parts, and reading that archive again for every
// click would make the page slower the more of it they used. What is
// remembered is a list of names, so it stays small; it is remembered only
// briefly, because a repository can gain a snapshot at any time and a
// stale answer about a backup is worse than a slow one.
type Cache struct {
	// TTL is how long a reading is kept. Zero means two minutes.
	TTL time.Duration

	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	read  time.Time
	held  *contents
	err   error
	ready chan struct{}
}

// Items says what one part of an account this backup holds.
func (c *Cache) Items(ctx context.Context, reader Reader, src Source,
	kind granular.Kind) ([]Item, error) {

	if !kind.ListsItems() || kind.NeedsNames() {
		// Files, mail and the databases are listed by restic itself,
		// from the snapshot's own paths. Nothing here reads them.
		return nil, nil
	}
	held, err := c.contents(ctx, reader, src)
	if err != nil {
		return nil, err
	}
	return itemsOf(kind, held)
}

// Forget drops what was read for one snapshot.
func (c *Cache) Forget(src Source) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, src.Key+"\x00"+src.SnapshotID)
}

func (c *Cache) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return 2 * time.Minute
}

// contents returns what this snapshot's containers hold, reading them if
// the last reading has expired.
//
// One reader at a time per snapshot: two pages opened together would
// otherwise stream the same archive out of the repository twice.
func (c *Cache) contents(ctx context.Context, reader Reader, src Source) (*contents, error) {
	key := src.Key + "\x00" + src.SnapshotID

	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*entry{}
	}
	if found, ok := c.entries[key]; ok {
		fresh := found.ready == nil && time.Since(found.read) < c.ttl()
		if fresh || found.ready != nil {
			c.mu.Unlock()
			if found.ready != nil {
				select {
				case <-found.ready:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return found.held, found.err
		}
	}
	pending := &entry{ready: make(chan struct{})}
	c.entries[key] = pending
	c.mu.Unlock()

	held, err := read(ctx, reader, src)

	c.mu.Lock()
	pending.held, pending.err, pending.read = held, err, time.Now()
	close(pending.ready)
	pending.ready = nil
	if err != nil {
		// A failure is not kept: the next click should try again rather
		// than repeat an error for two minutes.
		delete(c.entries, key)
	}
	c.mu.Unlock()
	return held, err
}

// read opens this snapshot's containers.
func read(ctx context.Context, reader Reader, src Source) (*contents, error) {
	held := &contents{}
	if src.Parts.Metadata != "" {
		members, bodies, err := readArchive(ctx, reader, src)
		if err != nil {
			return nil, err
		}
		held.members, held.bodies = members, bodies
	}
	if src.Parts.Databases != "" {
		// A backup made before cprest staged the stored passwords has the
		// grants and not the file beside them. The users are still worth
		// listing, so a missing file is not a failure here -- the restore
		// itself is where that becomes one.
		auth, err := readFile(ctx, reader, src.Repo, src.SnapshotID,
			path.Join(src.Parts.Databases, granular.DatabaseUsersAuthFile))
		if err == nil {
			held.auth, _ = ParseAuth(auth)
		}
		grants, err := readFile(ctx, reader, src.Repo, src.SnapshotID,
			path.Join(src.Parts.Databases, granular.RunnableDatabaseUsersFile))
		if err == nil {
			held.grants, _ = ParseGrants(grants)
		}
	}
	return held, nil
}
