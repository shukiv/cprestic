package node

import (
	"context"
	"fmt"

	"github.com/shuki/cprest/internal/resticrun"
)

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
