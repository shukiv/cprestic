package node

import (
	"context"
	"fmt"
	"time"

	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/resticrun"
)

// Retention is what keeps a repository from growing forever.
//
// It is the only thing this program does that destroys data, so it is
// built to be looked at before it is agreed to: every repository is
// planned with "restic forget --dry-run" first, the plan is stored where
// the interface can show it, and nothing is deleted from a repository
// until an operator has approved it once.

const (
	// retentionEvery is how often a repository is looked at. Forget takes
	// an exclusive lock on the repository, so this is deliberately far
	// slower than the scheduler tick and slower than the nightly backup.
	retentionEvery = 20 * time.Hour
)

// MergedRetention is the keep policy for one repository: the most
// generous value each schedule that writes there asks for.
//
// One repository, one forget. Running it per schedule would take the lock
// twenty times over, and would leave anything no live schedule mentions
// untouched forever -- the server's own settings, and the backups of an
// account that has since been deleted, which are exactly the snapshots
// that make a repository grow without bound.
//
// The trade-off is that a repository shared by a schedule keeping thirty
// days and one keeping seven keeps thirty for both. Keeping too much is
// the right way for this to be wrong.
func MergedRetention(policies []nodestore.Policy, repositoryID string) nodestore.Retention {
	var merged nodestore.Retention
	for _, policy := range policies {
		if !policy.Enabled {
			continue
		}
		writesHere := false
		for _, id := range policy.RepositoryIDs {
			if id == repositoryID {
				writesHere = true
				break
			}
		}
		if !writesHere {
			continue
		}
		merged.KeepLast = max(merged.KeepLast, policy.Retention.KeepLast)
		merged.KeepDaily = max(merged.KeepDaily, policy.Retention.KeepDaily)
		merged.KeepWeekly = max(merged.KeepWeekly, policy.Retention.KeepWeekly)
		merged.KeepMonthly = max(merged.KeepMonthly, policy.Retention.KeepMonthly)
		merged.KeepYearly = max(merged.KeepYearly, policy.Retention.KeepYearly)
	}
	return merged
}

// empty reports a keep policy that would tell restic to delete everything.
func emptyRetention(r nodestore.Retention) bool {
	return r.KeepLast == 0 && r.KeepDaily == 0 && r.KeepWeekly == 0 &&
		r.KeepMonthly == 0 && r.KeepYearly == 0
}

func forgetSpec(keeps nodestore.Retention, dryRun, prune bool) resticrun.ForgetSpec {
	return resticrun.ForgetSpec{
		KeepLast:    keeps.KeepLast,
		KeepDaily:   keeps.KeepDaily,
		KeepWeekly:  keeps.KeepWeekly,
		KeepMonthly: keeps.KeepMonthly,
		KeepYearly:  keeps.KeepYearly,
		DryRun:      dryRun,
		Prune:       prune,
	}
}

// PlanRetention asks what retention would remove from one repository,
// without removing anything, and records the answer.
func (e *Engine) PlanRetention(ctx context.Context, repositoryID string) (nodestore.RetentionState, error) {
	keeps, err := e.retentionFor(repositoryID)
	if err != nil {
		return nodestore.RetentionState{}, err
	}
	// Maintenance credentials: an append-only destination refuses a
	// delete from the credentials a backup runs under, by design.
	repo, err := e.OpenRepository(repositoryID, true)
	if err != nil {
		return nodestore.RetentionState{}, err
	}

	plan, err := e.runner.ForgetPlanned(ctx, repo, forgetSpec(keeps, true, false))
	if err != nil {
		return nodestore.RetentionState{}, e.recordRetention(repositoryID, func(state *nodestore.RetentionState) {
			state.LastError = err.Error()
		})
	}

	now := time.Now().UTC()
	var recorded nodestore.RetentionState
	err = e.recordRetention(repositoryID, func(state *nodestore.RetentionState) {
		state.PlannedAt = &now
		state.Plan = planGroups(plan)
		state.WouldKeep, state.WouldDrop = plan.Kept, plan.Removed
		state.LastError = ""
		recorded = *state
	})
	return recorded, err
}

// ApplyRetention removes what the policy says to remove, and prunes the
// space back if anything went.
//
// The plan is computed again here rather than replayed from what was
// stored: a backup can land between the two, and applying a stored list
// of snapshot ids would delete what the operator saw rather than what the
// policy says now. The stored plan is a record of what they were shown.
func (e *Engine) ApplyRetention(ctx context.Context, repositoryID string) (int, error) {
	stored, err := e.store.Repository(repositoryID)
	if err != nil {
		return 0, err
	}
	if stored.RetentionApprovedAt == nil {
		return 0, fmt.Errorf(
			"node: retention has not been approved for this destination yet")
	}
	keeps, err := e.retentionFor(repositoryID)
	if err != nil {
		return 0, err
	}
	repo, err := e.OpenRepository(repositoryID, true)
	if err != nil {
		return 0, err
	}

	// Forget first, without pruning: forget is quick and prune walks the
	// whole repository, so there is no point paying for the walk when
	// nothing was removed.
	plan, err := e.runner.ForgetPlanned(ctx, repo, forgetSpec(keeps, false, false))
	if err != nil {
		return 0, e.recordRetention(repositoryID, func(state *nodestore.RetentionState) {
			state.LastError = err.Error()
		})
	}
	if plan.Removed > 0 {
		if err := e.runner.Prune(ctx, repo); err != nil {
			return plan.Removed, e.recordRetention(repositoryID, func(state *nodestore.RetentionState) {
				state.LastError = "removed the backups but could not reclaim the space: " + err.Error()
			})
		}
	}

	now := time.Now().UTC()
	return plan.Removed, e.recordRetention(repositoryID, func(state *nodestore.RetentionState) {
		state.AppliedAt = &now
		state.Dropped = plan.Removed
		state.PlannedAt = &now
		state.Plan = planGroups(plan)
		state.WouldKeep, state.WouldDrop = plan.Kept, 0
		state.LastError = ""
	})
}

// ApproveRetention records that an operator has read a plan and agreed
// that this repository may have backups deleted from it.
func (e *Engine) ApproveRetention(repositoryID string) error {
	stored, err := e.store.Repository(repositoryID)
	if err != nil {
		return err
	}
	if stored.Retention.PlannedAt == nil {
		return fmt.Errorf(
			"node: nothing has been planned for this destination yet, so there is " +
				"nothing to approve")
	}
	now := time.Now().UTC()
	stored.RetentionApprovedAt = &now
	_, err = e.store.PutRepository(stored)
	return err
}

// WithdrawRetention stops retention deleting anything from a repository
// again until it is approved afresh.
func (e *Engine) WithdrawRetention(repositoryID string) error {
	stored, err := e.store.Repository(repositoryID)
	if err != nil {
		return err
	}
	stored.RetentionApprovedAt = nil
	_, err = e.store.PutRepository(stored)
	return err
}

// retentionFor is the merged keep policy for a repository, refused if it
// would delete everything.
func (e *Engine) retentionFor(repositoryID string) (nodestore.Retention, error) {
	policies, err := e.store.Policies()
	if err != nil {
		return nodestore.Retention{}, err
	}
	keeps := MergedRetention(policies, repositoryID)
	if emptyRetention(keeps) {
		// restic deletes every snapshot when told to keep none. Never
		// substitute a default nobody chose.
		return nodestore.Retention{}, fmt.Errorf(
			"node: no enabled schedule writing to this destination says how much to keep, " +
				"so there is nothing to apply")
	}
	return keeps, nil
}

func planGroups(plan resticrun.ForgetPlan) []nodestore.RetentionGroup {
	groups := make([]nodestore.RetentionGroup, 0, len(plan.Groups))
	for _, group := range plan.Groups {
		row := nodestore.RetentionGroup{
			Account: group.Account, Host: group.Host,
			Keep: group.Kept, Drop: group.Removed,
		}
		if !group.Oldest.IsZero() {
			oldest := group.Oldest
			row.Oldest = &oldest
		}
		if !group.Newest.IsZero() {
			newest := group.Newest
			row.Newest = &newest
		}
		groups = append(groups, row)
	}
	return groups
}

// recordRetention updates one repository's retention state.
func (e *Engine) recordRetention(repositoryID string, update func(*nodestore.RetentionState)) error {
	stored, err := e.store.Repository(repositoryID)
	if err != nil {
		return err
	}
	update(&stored.Retention)
	if _, err := e.store.PutRepository(stored); err != nil {
		return err
	}
	return nil
}

// sweepRetention looks at one repository that is due, on the scheduler
// tick.
//
// Deliberately one at a time and rarely. Forget and prune both take an
// exclusive lock on the repository, so a retention run and a backup
// cannot overlap: this waits for the queue to be empty, and leaves a
// repository alone for the best part of a day after it has been looked
// at.
func (e *Engine) sweepRetention(ctx context.Context, now time.Time) {
	busy, err := e.anyJobRunning()
	if err != nil || busy {
		// A backup holds the same lock. Retention can wait; a backup
		// cannot.
		return
	}

	repositories, err := e.store.Repositories()
	if err != nil {
		e.log.Error("read repositories", "error", err)
		return
	}
	for _, repo := range repositories {
		if repo.InitialisedAt == nil {
			continue
		}
		if last := lastRetentionAttempt(repo.Retention); !last.IsZero() &&
			now.Sub(last) < retentionEvery {
			continue
		}

		if repo.RetentionApprovedAt == nil {
			// Never approved: take a plan so the operator has something
			// to read, and stop there. Nothing is deleted from a
			// repository until somebody has seen what would go.
			if _, err := e.PlanRetention(ctx, repo.ID); err != nil {
				e.log.Warn("plan retention", "repository", repo.ID, "error", err)
			}
			return
		}

		removed, err := e.ApplyRetention(ctx, repo.ID)
		if err != nil {
			e.log.Error("apply retention", "repository", repo.ID, "error", err)
			return
		}
		if removed > 0 {
			e.log.Info("retention applied", "repository", repo.ID, "removed", removed)
		}
		// One repository per pass: each one takes a lock and a prune
		// walks everything.
		return
	}
}

// lastRetentionAttempt is when this repository was last looked at, either
// way. A failed attempt counts: a repository whose lock is held by
// something stale must not be retried every fifteen seconds.
func lastRetentionAttempt(state nodestore.RetentionState) time.Time {
	latest := time.Time{}
	for _, at := range []*time.Time{state.PlannedAt, state.AppliedAt} {
		if at != nil && at.After(latest) {
			latest = *at
		}
	}
	return latest
}

// anyJobRunning reports whether the server has work in flight.
func (e *Engine) anyJobRunning() (bool, error) {
	jobs, err := e.store.Jobs(0)
	if err != nil {
		return false, err
	}
	for _, stored := range jobs {
		if !stored.Status.Terminal() {
			return true, nil
		}
	}
	restores, err := e.store.Restores(0)
	if err != nil {
		return false, err
	}
	for _, stored := range restores {
		if !stored.Status.Terminal() {
			return true, nil
		}
	}
	return false, nil
}
