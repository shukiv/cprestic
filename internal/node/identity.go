package node

import (
	"context"
	"fmt"
	"os/user"
	"strconv"
	"time"

	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/resticrun"
)

// A cPanel username is a label. The unix account behind it is the
// identity, and the two come apart exactly once: when an account is
// deleted and another is created with the same name. A host does that
// whenever a customer leaves and the next one asks for a name they liked.
//
// Everything in this program that goes by name would then hand the second
// customer the first one's backups. Their own self-service page would
// list them, and let them restore and download them. Nothing would look
// wrong.

// BackfillIdentities records every account that is on the server now
// against the unix account it means.
//
// It runs once, and what it establishes is the thing every later check
// leans on: these accounts are here today, so the backups already stored
// under their names are theirs. Without it, the first branch of
// noteIdentity would be reasoning from an absent record -- and an absent
// record is not evidence that a name has always meant the same customer.
// A name recycled before this program ever recorded it would have looked
// exactly the same.
func (e *Engine) BackfillIdentities(ctx context.Context) error {
	settings := e.Settings()
	if settings.IdentitiesBackfilledAt != nil {
		return nil
	}
	accounts, err := e.provider.Accounts(ctx)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	for _, account := range accounts {
		if _, err := e.store.Identity(account.User); err == nil {
			continue
		}
		uid, err := accountUID(account.User)
		if err != nil {
			// An account cPanel lists that the system does not know is a
			// problem for the backup to report, not for this to guess at.
			e.log.Warn("no unix account behind a cPanel name",
				"account", account.User, "error", err)
			continue
		}
		if _, err := e.store.PutIdentity(nodestore.AccountIdentity{
			Account: account.User, UID: uid, SinceAt: now,
		}); err != nil {
			return err
		}
	}

	settings.IdentitiesBackfilledAt = &now
	if err := e.store.SaveSettings(settings); err != nil {
		return err
	}
	e.settings = settings
	e.log.Info("recorded which unix account each cPanel name means",
		"accounts", len(accounts))
	return nil
}

// noteIdentity records which unix account a name means, and reports
// whether the name has just changed hands.
func (e *Engine) noteIdentity(account string) (nodestore.AccountIdentity, error) {
	uid, err := accountUID(account)
	if err != nil {
		return nodestore.AccountIdentity{}, err
	}

	now := time.Now().UTC()
	stored, err := e.store.Identity(account)
	switch {
	case err != nil:
		// No record. What that means depends on whether every account
		// then present has already been recorded: before the backfill it
		// means nobody has looked yet, and after it, it means this name
		// was not on the server when we looked -- so it is either new or
		// has changed hands, and anything stored under it from before now
		// is not this account's to read.
		fresh := nodestore.AccountIdentity{Account: account, UID: uid, SinceAt: now}
		fresh.Recycled = e.Settings().IdentitiesBackfilledAt != nil
		if fresh.Recycled {
			e.log.Warn("a cPanel name appeared that was not here when accounts were recorded",
				"account", account, "uid", uid,
				"older_backups_hidden_from_the_account", true)
		}
		return e.store.PutIdentity(fresh)
	case stored.UID == uid:
		stored.SinceAt = orZero(stored.SinceAt, now)
		return e.store.PutIdentity(stored)
	}

	// The name means a different unix account than it did. Whoever holds
	// it now is not who took the older backups.
	e.log.Warn("a cPanel name has changed hands",
		"account", account, "was_uid", stored.UID, "now_uid", uid,
		"backups_before", stored.SinceAt)
	return e.store.PutIdentity(nodestore.AccountIdentity{
		Account: account, UID: uid, SinceAt: now, Recycled: true,
		CreatedAt: stored.CreatedAt,
	})
}

// visibleSince is the earliest backup of an account that the person
// holding that name today may see.
//
// Zero means all of them: a name that has never changed hands has one
// owner, and the date this program first noticed it is not a boundary
// between anybody.
func (e *Engine) visibleSince(account string) time.Time {
	stored, err := e.store.Identity(account)
	if err != nil || !stored.Recycled {
		return time.Time{}
	}
	return stored.SinceAt
}

// OwnedByCaller reports whether the unix account on the other end of the
// socket is the one this name currently means, and from when its backups
// are theirs.
//
// It is checked against the running system rather than against what was
// recorded, because the record is what the last backup saw and the socket
// is who is asking now.
func (e *Engine) OwnedByCaller(account string, callerUID uint32) (since time.Time, err error) {
	uid, err := accountUID(account)
	if err != nil {
		return time.Time{}, err
	}
	if uid != int(callerUID) {
		return time.Time{}, fmt.Errorf(
			"node: %s is uid %d on this server and the request came from uid %d",
			account, uid, callerUID)
	}
	return e.visibleSince(account), nil
}

// accountUID is the unix account a cPanel name means right now.
func accountUID(account string) (int, error) {
	found, err := user.Lookup(account)
	if err != nil {
		return 0, fmt.Errorf("node: %s is not an account on this server: %w", account, err)
	}
	uid, err := strconv.Atoi(found.Uid)
	if err != nil {
		return 0, fmt.Errorf("node: %s has an unreadable uid %q", account, found.Uid)
	}
	return uid, nil
}

func orZero(value, fallback time.Time) time.Time {
	if value.IsZero() {
		return fallback
	}
	return value
}

// UserSnapshots is what the person holding an account's name today may
// see of it.
//
// It is the account's snapshots, minus anything taken before the name
// changed hands. A name that has never changed hands loses nothing: the
// filter exists for the one case where "this account's backups" and
// "this customer's backups" are different sets.
func (e *Engine) UserSnapshots(ctx context.Context, repositoryID, account string) ([]resticrun.Snapshot, error) {
	// Checked here and not only when a backup runs. Between an account
	// being deleted and the new one's first backup there can be days, and
	// this is the interface the new customer is looking at during them.
	if _, err := e.noteIdentity(account); err != nil {
		return nil, err
	}
	snapshots, err := e.Snapshots(ctx, repositoryID, account)
	if err != nil {
		return nil, err
	}
	since := e.visibleSince(account)
	if since.IsZero() {
		return snapshots, nil
	}
	theirs := make([]resticrun.Snapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.Time.Before(since) {
			continue
		}
		theirs = append(theirs, snapshot)
	}
	return theirs, nil
}

// OwnsSnapshot reports whether a given backup is one this account's
// current owner may act on. Restores go through it as well as listings:
// hiding a snapshot from a page while still restoring it on request
// would be no protection at all.
func (e *Engine) OwnsSnapshot(ctx context.Context, repositoryID, account, snapshotID string) error {
	snapshots, err := e.UserSnapshots(ctx, repositoryID, account)
	if err != nil {
		return err
	}
	for _, snapshot := range snapshots {
		if snapshot.ID == snapshotID || snapshot.ShortID == snapshotID {
			return nil
		}
	}
	return fmt.Errorf("node: that backup is not one of this account's")
}
