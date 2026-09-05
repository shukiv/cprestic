package node

import (
	"context"
	"fmt"
	"time"

	"github.com/shuki/cprest/internal/resticrun"
)

// deletedSweepEvery is how often deleted accounts are looked at. Forgetting
// one takes the repository lock, and nothing here is urgent: an account
// that reached its ninetieth day an hour ago can go this evening.
const deletedSweepEvery = 6 * time.Hour

// ForgetAccountBackups removes a deleted account's backups from every
// destination and then forgets the account here.
//
// What this is for: a customer has gone, and their backups are to go with
// them -- a request an operator has to be able to answer, and one nothing
// else in this program does. Retention thins a series and always keeps
// something; this removes the series.
//
// Only an account cPanel no longer has. A live account's backups are its
// protection, and there is no reading of "forget this" that means "leave
// the account running with nothing behind it".
//
// Snapshots first, records second. A failure removing snapshots leaves the
// account still listed, with its history, and the operator can try again.
// The other order would leave backups in a destination that nothing on this
// server knows the name of.
func (e *Engine) ForgetAccountBackups(ctx context.Context, account string) (int, error) {
	if account == "" {
		return 0, fmt.Errorf("node: no account was named")
	}
	identity, err := e.store.Identity(account)
	if err != nil {
		return 0, fmt.Errorf("node: %s is not an account this server has records for", account)
	}
	if identity.RetiredAt == nil {
		return 0, fmt.Errorf(
			"node: %s is still on this server, so its backups are not forgotten", account)
	}

	repositories, err := e.store.Repositories()
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, stored := range repositories {
		// The endpoint that permits deletes: forgetting is one.
		repo, err := e.OpenRepository(stored.ID, true)
		if err != nil {
			return removed, err
		}
		snapshots, err := e.runner.Snapshots(ctx, repo, resticrun.SnapshotFilter{
			Tags: []string{"account:" + account},
		})
		if err != nil {
			return removed, err
		}
		if len(snapshots) == 0 {
			continue
		}
		ids := make([]string, 0, len(snapshots))
		for _, snapshot := range snapshots {
			ids = append(ids, snapshot.ID)
		}
		if err := e.runner.ForgetSnapshots(ctx, repo, ids); err != nil {
			return removed, err
		}
		e.log.Warn("a deleted account's backups were removed",
			"account", account, "repository_id", stored.ID, "snapshots", len(ids))
		removed += len(ids)
	}

	if err := e.store.ForgetAccount(account); err != nil {
		return removed, err
	}
	e.log.Warn("a deleted account was forgotten", "account", account, "snapshots", removed)
	return removed, nil
}

// sweepDeletedAccounts forgets the backups of accounts that have been gone
// longer than this server keeps them.
//
// Nothing else ever removes them. Retention thins a series and always keeps
// something, so without this a server accumulates the backups of every
// customer who has ever left, on a destination somebody pays for. The
// operator sets the period, including "keep them until I say so".
func (e *Engine) sweepDeletedAccounts(ctx context.Context, now time.Time) {
	life := e.settings.KeepDeletedAccountsFor()
	if life <= 0 {
		return
	}
	if !e.lastDeletedSweep.IsZero() && now.Sub(e.lastDeletedSweep) < deletedSweepEvery {
		return
	}
	busy, err := e.anyJobRunning()
	if err != nil || busy {
		// Forgetting takes the same repository lock a backup does, and a
		// backup is the thing that must not wait.
		return
	}
	e.lastDeletedSweep = now

	identities, err := e.store.Identities()
	if err != nil {
		e.log.Error("read account identities", "error", err)
		return
	}
	live, err := e.provider.Accounts(ctx)
	if err != nil {
		// Without cPanel's own list there is no way to tell a deleted
		// account from one this server simply has no record for, and
		// deleting backups on a guess is not a thing to do.
		e.log.Warn("cannot list accounts, so nothing is forgotten", "error", err)
		return
	}
	here := make(map[string]bool, len(live))
	for _, account := range live {
		here[account.User] = true
	}

	cutoff := now.Add(-life)
	for _, identity := range identities {
		if identity.RetiredAt == nil || identity.RetiredAt.After(cutoff) {
			continue
		}
		if here[identity.Account] {
			// The name is on the server again. Whoever holds it now is
			// not the customer who left, and neither their backups nor
			// this record are the ones to remove.
			continue
		}
		removed, err := e.ForgetAccountBackups(ctx, identity.Account)
		if err != nil {
			e.log.Error("forget a deleted account whose time is up",
				"account", identity.Account, "error", err)
			continue
		}
		e.log.Warn("forgot a deleted account whose backups reached their age",
			"account", identity.Account, "snapshots", removed,
			"deleted_days_ago", int(now.Sub(*identity.RetiredAt).Hours()/24))
	}
}
