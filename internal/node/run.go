package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/shuki/cprest/internal/destination"
	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/protocol"
	"github.com/shuki/cprest/internal/reassemble"
	"github.com/shuki/cprest/internal/resticrun"
	"github.com/shuki/cprest/internal/vault"
)

// EnsureProvisioned creates any repository that does not exist yet on its
// destination.
//
// The first repository is initialised normally; every later one copies its
// chunker parameters, because those are fixed at creation and cannot be
// changed afterwards. See docs/DESIGN.md §7.
func (e *Engine) EnsureProvisioned(ctx context.Context) (int, error) {
	repos, err := e.store.Repositories()
	if err != nil {
		return 0, err
	}

	var created int
	for _, repo := range repos {
		if repo.InitialisedAt != nil {
			continue
		}
		target, err := e.OpenRepository(repo.ID, true)
		if err != nil {
			return created, err
		}

		var source *resticrun.Repository
		if repo.ChunkerSourceRepoID != "" {
			opened, err := e.OpenRepository(repo.ChunkerSourceRepoID, true)
			if err != nil {
				return created, fmt.Errorf("node: open chunker source: %w", err)
			}
			source = &opened
		}
		if err := e.runner.Init(ctx, target, source); err != nil {
			return created, err
		}
		if err := e.store.MarkRepositoryInitialised(repo.ID); err != nil {
			return created, err
		}
		e.log.Info("repository created",
			"repository_id", repo.ID, "path", repo.Path,
			"chunker_source", repo.ChunkerSourceRepoID)
		created++
	}
	return created, nil
}

// QueueBackup queues a backup of one account under a policy.
func (e *Engine) QueueBackup(policyID, account string) (nodestore.Job, error) {
	running, err := e.store.RunningJobFor(account)
	if err != nil {
		return nodestore.Job{}, err
	}
	if running {
		// Two runs for one account would stage in the same directory.
		return nodestore.Job{}, fmt.Errorf("node: account %s already has work in flight", account)
	}
	return e.store.PutJob(nodestore.Job{
		PolicyID: policyID, Account: account, Status: job.StatusPending,
	})
}

// QueueRestore queues a restore.
func (e *Engine) QueueRestore(restore nodestore.Restore) (nodestore.Restore, error) {
	if restore.SnapshotID == "" {
		return nodestore.Restore{}, errors.New("node: restore needs a snapshot")
	}
	if restore.Kind == "" {
		restore.Kind = protocol.RestoreAccount
	}
	if restore.Kind == protocol.RestoreFiles && len(restore.IncludePaths) == 0 {
		return nodestore.Restore{}, errors.New("node: a files restore needs at least one path")
	}
	if restore.Apply && restore.Kind != protocol.RestoreAccount {
		return nodestore.Restore{}, errors.New("node: only a whole-account restore can be applied")
	}

	running, err := e.store.RunningJobFor(restore.Account)
	if err != nil {
		return nodestore.Restore{}, err
	}
	if running {
		return nodestore.Restore{}, fmt.Errorf(
			"node: account %s already has work in flight", restore.Account)
	}
	return e.store.PutRestore(restore)
}

// RunOnce performs the oldest piece of pending work, if any, and reports
// whether it did anything.
func (e *Engine) RunOnce(ctx context.Context) (bool, error) {
	restore, backup, err := e.store.PendingWork()
	if err != nil {
		return false, err
	}
	switch {
	case restore != nil:
		return true, e.runRestore(ctx, *restore)
	case backup != nil:
		return true, e.runBackup(ctx, *backup)
	default:
		return false, nil
	}
}

func (e *Engine) runBackup(ctx context.Context, stored nodestore.Job) error {
	policy, err := e.store.Policy(stored.PolicyID)
	if err != nil {
		return e.failJob(stored, fmt.Sprintf("policy: %v", err))
	}
	account, err := e.provider.Account(ctx, stored.Account)
	if err != nil {
		return e.failJob(stored, fmt.Sprintf("account: %v", err))
	}
	assignment, err := e.assignmentFor(stored, policy, account)
	if err != nil {
		return e.failJob(stored, err.Error())
	}

	now := time.Now().UTC()
	stored.Status = job.StatusRunning
	stored.StartedAt = &now
	if _, err := e.store.PutJob(stored); err != nil {
		return err
	}

	report := e.worker.RunJob(ctx, assignment)

	stored.StagingErr = report.StagingError
	stored.Targets = nil
	results := make([]job.TargetResult, 0, len(report.Targets))
	for _, target := range report.Targets {
		stored.Targets = append(stored.Targets, nodestore.JobTarget{
			RepositoryID:   target.RepositoryID,
			Status:         job.TargetStatus(target.Status),
			SnapshotID:     target.SnapshotID,
			BytesAdded:     target.BytesAdded,
			BytesProcessed: target.BytesProcessed,
			DurationSecs:   target.DurationSecs,
			Incomplete:     target.Incomplete,
			Error:          target.Error,
			Detail:         target.Detail,
		})
		results = append(results, job.TargetResult{Status: job.TargetStatus(target.Status)})
	}
	if report.StagingError != "" {
		// Nothing was uploaded, so no target holds a copy.
		for i := range stored.Targets {
			stored.Targets[i].Status = job.TargetFailed
			stored.Targets[i].Error = report.StagingError
		}
		results = nil
		for _, repositoryID := range policy.RepositoryIDs {
			_ = repositoryID
			results = append(results, job.TargetResult{Status: job.TargetFailed})
		}
	}

	stored.Status = job.Rollup(results)
	finished := time.Now().UTC()
	stored.FinishedAt = &finished
	if _, err := e.store.PutJob(stored); err != nil {
		return err
	}
	e.log.Info("backup finished",
		"job_id", stored.ID, "account", stored.Account, "status", stored.Status)
	return nil
}

func (e *Engine) failJob(stored nodestore.Job, reason string) error {
	stored.Status = job.StatusFailed
	stored.StagingErr = reason
	finished := time.Now().UTC()
	stored.FinishedAt = &finished
	e.log.Error("backup failed before starting",
		"job_id", stored.ID, "account", stored.Account, "error", reason)
	_, err := e.store.PutJob(stored)
	return err
}

func (e *Engine) runRestore(ctx context.Context, stored nodestore.Restore) error {
	target, err := e.targetFor(stored.RepositoryID)
	if err != nil {
		return e.failRestore(stored, err.Error())
	}
	account, err := e.provider.Account(ctx, stored.Account)
	if err != nil {
		return e.failRestore(stored, fmt.Sprintf("account: %v", err))
	}

	now := time.Now().UTC()
	stored.Status = job.StatusRunning
	stored.StartedAt = &now
	if _, err := e.store.PutRestore(stored); err != nil {
		return err
	}

	report := e.worker.RunRestore(ctx, protocol.RestoreAssignment{
		JobID:        stored.ID,
		AccountID:    stored.Account,
		CPanelUser:   stored.Account,
		SnapshotID:   stored.SnapshotID,
		Kind:         stored.Kind,
		IncludePaths: stored.IncludePaths,
		TargetDir:    stored.TargetDir,
		Apply:        stored.Apply,
		Source:       target,
		SizeEstimate: account.SizeBytes,
	})

	stored.Status = job.Status(report.Status)
	stored.BytesRestored = report.BytesRestored
	stored.ArchivePath = report.ArchivePath
	stored.RestoredTo = report.RestoredTo
	stored.Applied = report.Applied
	stored.Error = report.Error
	finished := time.Now().UTC()
	stored.FinishedAt = &finished
	if _, err := e.store.PutRestore(stored); err != nil {
		return err
	}
	e.log.Info("restore finished",
		"restore_id", stored.ID, "account", stored.Account, "status", stored.Status)
	return nil
}

func (e *Engine) failRestore(stored nodestore.Restore, reason string) error {
	stored.Status = job.StatusFailed
	stored.Error = reason
	finished := time.Now().UTC()
	stored.FinishedAt = &finished
	e.log.Error("restore failed before starting", "restore_id", stored.ID, "error", reason)
	_, err := e.store.PutRestore(stored)
	return err
}

// Schedule queues whatever the policies say is due.
//
// A policy that has never run, or has been dormant longer than the catch-up
// window, starts from the edge of that window: a server that was off for a
// week should run tonight's backup, not seven of them.
func (e *Engine) Schedule(ctx context.Context, now time.Time) (int, error) {
	policies, err := e.store.Policies()
	if err != nil {
		return 0, err
	}

	var queued int
	for _, policy := range policies {
		if !policy.Enabled || len(policy.RepositoryIDs) == 0 {
			continue
		}
		schedule, err := cron.ParseStandard(policy.ScheduleCron)
		if err != nil {
			e.log.Error("policy schedule is not valid",
				"policy", policy.Name, "cron", policy.ScheduleCron, "error", err)
			continue
		}

		last := time.Time{}
		if policy.LastRunAt != nil {
			last = *policy.LastRunAt
		}
		if earliest := now.Add(-catchUpWindow); last.IsZero() || last.Before(earliest) {
			last = earliest
		}
		if schedule.Next(last).After(now) {
			continue
		}

		accounts, err := e.accountsFor(ctx, policy)
		if err != nil {
			e.log.Error("resolve policy accounts", "policy", policy.Name, "error", err)
			continue
		}
		if err := e.store.SetPolicyLastRun(policy.ID, now); err != nil {
			return queued, err
		}
		for _, account := range accounts {
			if _, err := e.QueueBackup(policy.ID, account); err != nil {
				// Usually the previous run for this account is still
				// going, which is a skip rather than a failure.
				e.log.Warn("skipped account", "policy", policy.Name,
					"account", account, "reason", err)
				continue
			}
			queued++
		}
		e.log.Info("policy fired", "policy", policy.Name, "queued", queued)
	}
	return queued, nil
}

// catchUpWindow bounds how far back a restart looks for missed schedules.
const catchUpWindow = 2 * time.Hour

func (e *Engine) accountsFor(ctx context.Context, policy nodestore.Policy) ([]string, error) {
	if !policy.AllAccounts() {
		return policy.Accounts, nil
	}
	// Resolved at run time, so accounts created since the policy was
	// written are backed up without anyone remembering to edit it.
	accounts, err := e.provider.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(accounts))
	for _, account := range accounts {
		names = append(names, account.User)
	}
	return names, nil
}

// Run drives the scheduler and the worker until the context is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		if _, err := e.Schedule(ctx, time.Now()); err != nil {
			e.log.Error("scheduler", "error", err)
		}
		// Drain the queue before sleeping, so a policy that queued ten
		// accounts does not take ten ticks to start the second one.
		for {
			did, err := e.RunOnce(ctx)
			if err != nil {
				e.log.Error("run work", "error", err)
				break
			}
			if !did {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Forget applies a policy's retention to a repository and prunes.
//
// In fleet mode this runs on a separate trusted host. Standalone has no
// such host, so it runs here — which means the credential that can delete
// backups lives on the machine an attacker would compromise. That is the
// trade standalone makes; see docs/adr/0007-standalone-mode.md.
func (e *Engine) Forget(ctx context.Context, repositoryID string, retention nodestore.Retention, prune bool) error {
	repo, err := e.OpenRepository(repositoryID, true)
	if err != nil {
		return err
	}
	return e.runner.Forget(ctx, repo, resticrun.ForgetSpec{
		KeepLast:    retention.KeepLast,
		KeepDaily:   retention.KeepDaily,
		KeepWeekly:  retention.KeepWeekly,
		KeepMonthly: retention.KeepMonthly,
		KeepYearly:  retention.KeepYearly,
		// A repository holds every account on this server, so retention is
		// per account, and snapshot tags are what identify an account.
		GroupBy: "host,tags",
		Prune:   prune,
	})
}

// Check verifies a repository's integrity.
func (e *Engine) Check(ctx context.Context, repositoryID string, readDataSubsetPercent int) error {
	repo, err := e.OpenRepository(repositoryID, true)
	if err != nil {
		return err
	}
	return e.runner.Check(ctx, repo, resticrun.CheckSpec{
		ReadDataSubsetPercent: readDataSubsetPercent,
	})
}

// Drill rehearses a restore into scratch space and throws the result away.
func (e *Engine) Drill(ctx context.Context, repositoryID, account, workDir string) ([]string, error) {
	repo, err := e.OpenRepository(repositoryID, true)
	if err != nil {
		return nil, err
	}
	snapshots, err := e.runner.Snapshots(ctx, repo, resticrun.SnapshotFilter{
		Tags: []string{"account:" + account}, Latest: 1,
	})
	if err != nil {
		return nil, err
	}
	if len(snapshots) == 0 {
		return nil, fmt.Errorf("node: no snapshot of %s to rehearse", account)
	}

	rebuilt, err := reassemble.Run(ctx, e.runner, reassemble.Request{
		Account:    account,
		SnapshotID: snapshots[len(snapshots)-1].ID,
		WorkDir:    workDir,
		Repo:       repo,
	})
	if err != nil {
		return nil, err
	}
	return reassemble.Verify(rebuilt)
}

// AddDestination stores a destination and the repository that will live in
// it, sealing the credentials on the way past.
func (e *Engine) AddDestination(dest nodestore.Destination, secrets map[string]string,
	repositoryPath string) (nodestore.Destination, nodestore.Repository, error) {

	spec := destination.Spec{
		Type: destination.Type(dest.Type), Config: dest.Config, Secrets: secrets,
	}
	// Fail here, with a message an operator can read, rather than at two
	// in the morning on the first scheduled run.
	if _, err := destination.Build(spec); err != nil {
		return nodestore.Destination{}, nodestore.Repository{}, err
	}

	secretID, err := SealCredentials(e.store, e.vault, secrets)
	if err != nil {
		return nodestore.Destination{}, nodestore.Repository{}, err
	}
	dest.CredentialsSecretID = secretID

	stored, err := e.store.PutDestination(dest)
	if err != nil {
		return nodestore.Destination{}, nodestore.Repository{}, err
	}

	repo, err := e.newRepository(stored.ID, repositoryPath)
	if err != nil {
		return nodestore.Destination{}, nodestore.Repository{}, err
	}
	return stored, repo, nil
}

// newRepository creates the repository record for a destination, with its
// own generated password sealed in the vault.
func (e *Engine) newRepository(destinationID, path string) (nodestore.Repository, error) {
	password, err := vault.GenerateMasterKey()
	if err != nil {
		return nodestore.Repository{}, err
	}
	passwordID, err := SealRepositoryPassword(e.store, e.vault, password)
	if err != nil {
		return nodestore.Repository{}, err
	}
	return e.store.PutRepository(nodestore.Repository{
		DestinationID: destinationID, Path: path, PasswordSecretID: passwordID,
	})
}

// RunPolicyNow queues every account a schedule covers, immediately.
//
// It deliberately leaves the schedule's last-fired time alone: running one
// by hand is extra work, not a replacement for tonight's run, and moving
// the marker would skip that.
func (e *Engine) RunPolicyNow(ctx context.Context, policyID string) (queued int, skipped []string, err error) {
	policy, err := e.store.Policy(policyID)
	if err != nil {
		return 0, nil, err
	}
	if len(policy.RepositoryIDs) == 0 {
		return 0, nil, fmt.Errorf(
			"node: %q has no destination, so there is nowhere to send a backup", policy.Name)
	}

	accounts, err := e.accountsFor(ctx, policy)
	if err != nil {
		return 0, nil, err
	}
	if len(accounts) == 0 {
		return 0, nil, fmt.Errorf("node: %q covers no accounts", policy.Name)
	}

	for _, account := range accounts {
		if _, err := e.QueueBackup(policy.ID, account); err != nil {
			// Almost always because that account is already being backed
			// up, which is a skip rather than a failure.
			e.log.Warn("skipped account on a manual run",
				"policy", policy.Name, "account", account, "reason", err)
			skipped = append(skipped, account)
			continue
		}
		queued++
	}
	e.log.Info("schedule run by hand",
		"policy", policy.Name, "queued", queued, "skipped", len(skipped))
	return queued, skipped, nil
}

// QueueDownload asks for an account's newest backup to be rebuilt into an
// archive that can then be downloaded.
//
// It is a restore that is deliberately not applied: the archive is left on
// this server for someone to fetch, and nothing on the live account is
// touched.
func (e *Engine) QueueDownload(ctx context.Context, account string) (nodestore.Restore, error) {
	repositories, err := e.store.Repositories()
	if err != nil {
		return nodestore.Restore{}, err
	}

	var (
		newest     resticrun.Snapshot
		newestRepo string
		lastErr    error
	)
	for _, repository := range repositories {
		if repository.InitialisedAt == nil {
			continue
		}
		snapshots, err := e.Snapshots(ctx, repository.ID, account)
		if err != nil {
			// One unreachable destination should not stop us looking in
			// the others.
			lastErr = err
			continue
		}
		for _, snapshot := range snapshots {
			if snapshot.Time.After(newest.Time) {
				newest, newestRepo = snapshot, repository.ID
			}
		}
	}

	if newestRepo == "" {
		if lastErr != nil {
			return nodestore.Restore{}, fmt.Errorf(
				"node: no backup of %s could be found: %w", account, lastErr)
		}
		return nodestore.Restore{}, fmt.Errorf("node: %s has no backup yet", account)
	}

	return e.QueueRestore(nodestore.Restore{
		Account:      account,
		RepositoryID: newestRepo,
		SnapshotID:   newest.ID,
		Kind:         protocol.RestoreAccount,
	})
}

// ArchiveForDownload resolves a finished restore to the archive it produced,
// after checking the file is where this node put it.
//
// The path comes out of the state file rather than from a request, but it
// becomes an open() either way, so it is checked against the staging root
// before anything is read.
func (e *Engine) ArchiveForDownload(restoreID string) (path, filename string, size int64, err error) {
	restore, err := e.store.Restore(restoreID)
	if err != nil {
		return "", "", 0, err
	}
	if restore.Status != job.StatusSuccess || restore.ArchivePath == "" {
		return "", "", 0, fmt.Errorf("node: that restore produced no archive")
	}

	root, err := filepath.Abs(e.settings.StagingRoot)
	if err != nil {
		return "", "", 0, err
	}
	resolved, err := filepath.Abs(restore.ArchivePath)
	if err != nil {
		return "", "", 0, err
	}
	if !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return "", "", 0, fmt.Errorf("node: that archive is not in the staging area")
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", "", 0, fmt.Errorf(
			"node: the archive is gone — it is removed when the account is restored again "+
				"or the service restarts: %w", err)
	}
	return resolved, filepath.Base(resolved), info.Size(), nil
}
