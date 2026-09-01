package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/destination"
	"github.com/shuki/cprest/internal/human"
	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/notify"
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
// QueueSystemBackup queues the server's own configuration, under the name
// it is stored as. It is queued like any account, so it goes to the same
// destinations, with the same retention and the same reporting.
func (e *Engine) QueueSystemBackup(policyID string) (nodestore.Job, error) {
	return e.QueueBackup(policyID, cpanel.SystemAccount)
}

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
	if restore.Kind == KindVerify {
		// A rehearsal keeps nothing and applies nothing, so the checks
		// that follow do not apply to it.
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
	var account cpanel.AccountInfo
	if stored.Account == cpanel.SystemAccount {
		// Not an account: cPanel has never heard of it, and the worker
		// knows to stage the server's configuration instead.
		account = cpanel.AccountInfo{User: cpanel.SystemAccount}
	} else {
		account, err = e.provider.Account(ctx, stored.Account)
		if err != nil {
			return e.failJob(stored, fmt.Sprintf("account: %v", err))
		}
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
	// A percentage on a finished job says nothing, and "100%" beside a
	// failure would be a lie.
	stored.Progress = nil
	e.forgetProgress(stored.ID)
	finished := time.Now().UTC()
	stored.FinishedAt = &finished
	if _, err := e.store.PutJob(stored); err != nil {
		return err
	}
	e.log.Info("backup finished",
		"job_id", stored.ID, "account", stored.Account, "status", stored.Status)
	e.notifyBackup(ctx, stored)
	return nil
}

// notifyBackup says what a finished backup came to, for whoever asked to
// be told. A backup that fails silently is the same as no backup.
func (e *Engine) notifyBackup(ctx context.Context, stored nodestore.Job) {
	message := notify.Message{Account: stored.Account}
	switch stored.Status {
	case job.StatusSuccess:
		message.Event = notify.EventBackupSucceeded
		message.Subject = fmt.Sprintf("Backed up %s", stored.Account)
	case job.StatusPartialSuccess:
		message.Event = notify.EventBackupPartial
		message.Subject = fmt.Sprintf("Backed up %s, with files it could not read", stored.Account)
	case job.StatusFailed:
		message.Event = notify.EventBackupFailed
		message.Subject = fmt.Sprintf("The backup of %s failed", stored.Account)
	default:
		return
	}
	message.Body = e.backupDetail(stored)
	e.Notify(ctx, message)
}

// destinationNames maps each repository to the destination an operator
// named it, so a message says "Backup server" rather than the first eight
// characters of an identifier nobody has seen before.
func (e *Engine) destinationNames() map[string]string {
	names := map[string]string{}
	destinations, err := e.store.Destinations()
	if err != nil {
		e.log.Error("read destinations", "error", err)
		return names
	}
	byID := map[string]string{}
	for _, dest := range destinations {
		byID[dest.ID] = dest.Name
	}
	repos, err := e.store.Repositories()
	if err != nil {
		e.log.Error("read repositories", "error", err)
		return names
	}
	for _, repo := range repos {
		names[repo.ID] = byID[repo.DestinationID]
	}
	return names
}

// backupDetail is what the message says under its first line: what went
// wrong, or what it cost — per destination, because a backup that reached
// one of two is a different situation from one that reached both.
func (e *Engine) backupDetail(stored nodestore.Job) string {
	if stored.StagingErr != "" {
		return stored.StagingErr
	}
	names := e.destinationNames()
	var lines []string
	for _, target := range stored.Targets {
		where := names[target.RepositoryID]
		if where == "" {
			where = short(target.RepositoryID)
		}
		switch {
		case target.Error != "":
			lines = append(lines, fmt.Sprintf("%s: %s", where, target.Error))
		case target.Incomplete:
			lines = append(lines, fmt.Sprintf(
				"%s: stored, but some files could not be read", where))
		default:
			lines = append(lines, fmt.Sprintf("%s: %s stored, out of %s read",
				where, human.Bytes(target.BytesAdded), human.Bytes(target.BytesProcessed)))
		}
	}
	if stored.StartedAt != nil && stored.FinishedAt != nil {
		lines = append(lines, fmt.Sprintf("It took %s.",
			roughly(stored.FinishedAt.Sub(*stored.StartedAt))))
	}
	return strings.Join(lines, "\n")
}

// short is an id an operator can recognise without being a whole line.
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (e *Engine) failJob(stored nodestore.Job, reason string) error {
	stored.Status = job.StatusFailed
	stored.Progress = nil
	e.forgetProgress(stored.ID)
	stored.StagingErr = reason
	finished := time.Now().UTC()
	stored.FinishedAt = &finished
	e.log.Error("backup failed before starting",
		"job_id", stored.ID, "account", stored.Account, "error", reason)
	_, err := e.store.PutJob(stored)
	return err
}

func (e *Engine) runRestore(ctx context.Context, stored nodestore.Restore) error {
	if stored.Kind == KindVerify {
		return e.runDrill(ctx, stored)
	}

	target, err := e.targetFor(stored.RepositoryID)
	if err != nil {
		return e.failRestore(stored, err.Error())
	}
	var account cpanel.AccountInfo
	if stored.Account == cpanel.SystemAccount {
		// The server's own settings are not an account, and asking cPanel
		// about them fails on the name alone.
		account = cpanel.AccountInfo{User: cpanel.SystemAccount}
	} else {
		account, err = e.provider.Account(ctx, stored.Account)
		if err != nil {
			return e.failRestore(stored, fmt.Sprintf("account: %v", err))
		}
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
		ItemKind:     stored.ItemKind,
		ItemNames:    stored.ItemNames,
		Apply:        stored.Apply,
		Source:       target,
		SizeEstimate: account.SizeBytes,
	})

	stored.Status = job.Status(report.Status)
	stored.BytesRestored = report.BytesRestored
	stored.ArchivePath = report.ArchivePath
	stored.RestoredTo = report.RestoredTo
	if report.Detail != "" {
		stored.Detail = report.Detail
	}
	stored.Applied = report.Applied
	stored.Error = report.Error
	finished := time.Now().UTC()
	stored.FinishedAt = &finished
	if _, err := e.store.PutRestore(stored); err != nil {
		return err
	}
	e.log.Info("restore finished",
		"restore_id", stored.ID, "account", stored.Account, "status", stored.Status)
	subject := fmt.Sprintf("Restored %s", stored.Account)
	if stored.Status == job.StatusFailed {
		subject = fmt.Sprintf("The restore of %s failed", stored.Account)
	}
	body := stored.Detail
	if stored.Error != "" {
		body = stored.Error
	}
	e.Notify(ctx, notify.Message{
		Event: notify.EventRestore, Account: stored.Account,
		Subject: subject, Body: body,
	})
	return nil
}

// runDrill rehearses a restore and records what it proved.
func (e *Engine) runDrill(ctx context.Context, stored nodestore.Restore) error {
	now := time.Now().UTC()
	stored.Status = job.StatusRunning
	stored.StartedAt = &now
	if _, err := e.store.PutRestore(stored); err != nil {
		return err
	}

	checks, err := e.Drill(ctx, stored.RepositoryID, stored.Account)
	finished := time.Now().UTC()
	stored.FinishedAt = &finished
	if err != nil {
		stored.Status = job.StatusFailed
		stored.Error = err.Error()
		if len(checks) > 0 {
			stored.Detail = "passed before failing: " + strings.Join(checks, "; ")
		}
		e.log.Error("restore rehearsal failed",
			"account", stored.Account, "error", err, "checks", checks)
	} else {
		stored.Status = job.StatusSuccess
		stored.Detail = strings.Join(checks, "; ")
		e.log.Info("restore rehearsal passed",
			"account", stored.Account, "checks", checks)
	}
	_, storeErr := e.store.PutRestore(stored)
	return storeErr
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
	// Cheap — one readdir and a stat per finished output — and it is the
	// only thing that runs regularly on a server that is never restarted.
	if err := e.SweepWorkdir(); err != nil {
		e.log.Error("sweep the work directory", "error", err)
	}
	e.watchForTrouble(ctx, now)

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
		if policy.IncludeSystem {
			// The server's own configuration goes first: it is small, and
			// a replacement machine needs it before the accounts on it
			// mean anything.
			accounts = append([]string{cpanel.SystemAccount}, accounts...)
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

// watchForTrouble raises the two things no run reports, because no run
// happens: an account whose backups have stopped, and a run that started
// and never finished. Each is said once, when it becomes true.
func (e *Engine) watchForTrouble(ctx context.Context, now time.Time) {
	// The scheduler ticks every 15 seconds; nothing here changes that
	// fast, and accountsFor reads the account registry per policy.
	e.alertedMu.Lock()
	if now.Sub(e.lastWatch) < watchEvery {
		e.alertedMu.Unlock()
		return
	}
	e.lastWatch = now
	e.alertedMu.Unlock()

	// Nothing is listening yet. Say nothing rather than marking every
	// current problem as already told, which would leave the operator's
	// first channel silent about the very thing they added it for.
	channels, err := e.store.Channels()
	if err != nil {
		e.log.Error("read notification channels", "error", err)
		return
	}
	listening := false
	for _, channel := range channels {
		if channel.Enabled {
			listening = true
			break
		}
	}
	if !listening {
		return
	}

	e.probeDestinations(ctx, now)

	jobs, err := e.store.Jobs(0)
	if err != nil {
		e.log.Error("read jobs", "error", err)
		return
	}
	policies, err := e.store.Policies()
	if err != nil {
		e.log.Error("read policies", "error", err)
		return
	}

	stuckAfter := 6 * time.Hour
	for _, policy := range policies {
		if policy.AlertRunHours > 0 {
			if asked := time.Duration(policy.AlertRunHours) * time.Hour; asked < stuckAfter {
				stuckAfter = asked
			}
		}
	}

	e.alertedMu.Lock()
	defer e.alertedMu.Unlock()
	if e.alerted == nil {
		e.alerted = map[string]bool{}
	}

	for _, stored := range jobs {
		if stored.Status.Terminal() || stored.StartedAt == nil {
			continue
		}
		key := "stuck:" + stored.ID
		if now.Sub(*stored.StartedAt) < stuckAfter || e.alerted[key] {
			continue
		}
		e.alerted[key] = true
		e.Notify(ctx, notify.Message{
			Event:   notify.EventStuck,
			Account: stored.Account,
			Subject: fmt.Sprintf("The backup of %s has been running for %s",
				stored.Account, roughly(now.Sub(*stored.StartedAt))),
			Body: "A run that long is usually stuck rather than slow. It is holding the " +
				"staging space and the queue behind it.",
		})
	}

	// An account is overdue when its own schedule has come and gone
	// without a good backup, twice over.
	latest := map[string]nodestore.Job{}
	for _, stored := range jobs {
		if stored.Status != job.StatusSuccess {
			continue
		}
		if previous, seen := latest[stored.Account]; !seen || stored.QueuedAt.After(previous.QueuedAt) {
			latest[stored.Account] = stored
		}
	}
	for _, policy := range policies {
		if !policy.Enabled || len(policy.RepositoryIDs) == 0 {
			continue
		}
		schedule, err := cron.ParseStandard(policy.ScheduleCron)
		if err != nil {
			continue
		}
		first := schedule.Next(now)
		overdue := 2 * schedule.Next(first).Sub(first)
		if policy.AlertNoBackupDays > 0 {
			overdue = time.Duration(policy.AlertNoBackupDays) * 24 * time.Hour
		}
		if overdue <= 0 {
			continue
		}

		accounts, err := e.accountsFor(ctx, policy)
		if err != nil {
			continue
		}
		for _, account := range accounts {
			last, backedUp := latest[account]
			if backedUp && now.Sub(last.QueuedAt) < overdue {
				delete(e.alerted, "overdue:"+account)
				continue
			}
			key := "overdue:" + account
			if e.alerted[key] {
				continue
			}
			e.alerted[key] = true
			body := fmt.Sprintf("Nothing has backed it up in the last %s, and %q says it should have.",
				roughly(overdue), policy.Name)
			if !backedUp {
				body = fmt.Sprintf("It has never been backed up successfully, and %q says it should have been.",
					policy.Name)
			}
			e.Notify(ctx, notify.Message{
				Event:   notify.EventOverdue,
				Account: account,
				Subject: fmt.Sprintf("%s has no recent backup", account),
				Body:    body,
			})
		}
	}
}

// probeDestinations reaches each destination on a slow cadence, so that a
// backup server which went away is reported when it goes rather than at
// the next backup — which is the night the operator finds out they have
// none.
func (e *Engine) probeDestinations(ctx context.Context, now time.Time) {
	e.alertedMu.Lock()
	if now.Sub(e.lastProbe) < probeEvery {
		e.alertedMu.Unlock()
		return
	}
	e.lastProbe = now
	e.alertedMu.Unlock()

	destinations, err := e.store.Destinations()
	if err != nil {
		e.log.Error("read destinations", "error", err)
		return
	}
	for _, dest := range destinations {
		// TestDestination records the result and says so if it changed.
		if err := e.TestDestination(ctx, dest.ID); err != nil {
			e.log.Warn("destination unreachable", "destination", dest.Name, "error", err)
		}
	}
}

// roughly says a duration the way a person would, for a sentence rather
// than a table.
func roughly(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

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
//
// It never applies anything: the live account is not touched, and nothing
// is kept. What it answers is whether the backup can be turned back into an
// account at all, which is the only question worth asking of a backup
// nightly.
//
// Scratch space is allocated through the staging manager so the same disk
// check that guards a backup guards this too. A rehearsal that filled the
// volume would be worse than no rehearsal.
func (e *Engine) Drill(ctx context.Context, repositoryID, account string) (checks []string, err error) {
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
		return nil, fmt.Errorf("node: no backup of %s to rehearse", account)
	}
	newest := snapshots[len(snapshots)-1]

	// The rehearsal writes about what the backup read.
	estimate := newest.Summary.TotalBytesProcessed
	if estimate == 0 {
		estimate = 1 << 20
	}
	dir, err := e.staging.Allocate("drill-"+account, estimate)
	if err != nil {
		return nil, err
	}
	defer func() {
		if releaseErr := e.staging.Release(dir); releaseErr != nil {
			e.log.Error("release drill scratch", "path", dir.Path, "error", releaseErr)
		}
	}()

	rebuilt, err := reassemble.Run(ctx, e.runner, reassemble.Request{
		Account:    account,
		SnapshotID: newest.ID,
		WorkDir:    dir.Path,
		Repo:       repo,
	})
	if err != nil {
		return nil, err
	}
	return reassemble.Verify(rebuilt)
}

// QueueDrill asks for a rehearsal of an account's newest backup.
//
// It goes through the same queue as everything else, so it cannot run
// alongside a backup or restore of the same account and shows up in
// history with what it checked.
func (e *Engine) QueueDrill(ctx context.Context, account string) (nodestore.Restore, error) {
	repositories, err := e.store.Repositories()
	if err != nil {
		return nodestore.Restore{}, err
	}

	var (
		newest     resticrun.Snapshot
		newestRepo string
	)
	for _, repository := range repositories {
		if repository.InitialisedAt == nil {
			continue
		}
		snapshots, err := e.Snapshots(ctx, repository.ID, account)
		if err != nil {
			continue
		}
		for _, snapshot := range snapshots {
			if snapshot.Time.After(newest.Time) {
				newest, newestRepo = snapshot, repository.ID
			}
		}
	}
	if newestRepo == "" {
		return nodestore.Restore{}, fmt.Errorf("node: %s has no backup to rehearse", account)
	}

	return e.QueueRestore(nodestore.Restore{
		Account:      account,
		RepositoryID: newestRepo,
		SnapshotID:   newest.ID,
		Kind:         KindVerify,
	})
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

// ReadyDownload finds an archive already rebuilt for an account and still
// on disk, so pressing Download can hand it over rather than rebuilding
// something that is right there.
func (e *Engine) ReadyDownload(account string) (nodestore.Restore, bool) {
	restores, err := e.store.Restores(0)
	if err != nil {
		return nodestore.Restore{}, false
	}
	for _, restore := range restores {
		if restore.Account != account || restore.Status != job.StatusSuccess {
			continue
		}
		if restore.ArchivePath == "" || restore.Applied {
			continue
		}
		if _, _, _, err := e.statArchive(restore); err != nil {
			continue
		}
		// Restores come back newest first.
		return restore, true
	}
	return nodestore.Restore{}, false
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

// OpenArchiveForDownload opens the archive a finished restore produced.
//
// The path comes from this node's own state file rather than from a
// request, but it still becomes an open() as root on a server whose other
// users are not trusted, so it is checked rather than believed:
//
//   - symlinks are resolved on both the staging root and the target before
//     containment is decided, because a lexical prefix check is satisfied
//     by a symlink pointing anywhere;
//   - containment is decided with filepath.Rel, so a sibling directory
//     whose name merely starts with the root's cannot pass;
//   - the file is opened with O_NOFOLLOW, so a symlink swapped in at the
//     leaf between the check and the open is refused rather than followed.
//
// The caller closes the file.
func (e *Engine) OpenArchiveForDownload(restoreID string) (file *os.File, filename string, size int64, err error) {
	restore, err := e.store.Restore(restoreID)
	if err != nil {
		return nil, "", 0, err
	}
	resolved, filename, size, err := e.statArchive(restore)
	if err != nil {
		return nil, "", 0, err
	}

	file, err = os.OpenFile(resolved, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", 0, fmt.Errorf("node: open the archive: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, "", 0, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, "", 0, fmt.Errorf("node: that archive is not a regular file")
	}
	return file, filename, info.Size(), nil
}

// statArchive resolves a restore's archive and checks it is where this node
// puts them, without opening it.
func (e *Engine) statArchive(restore nodestore.Restore) (path, filename string, size int64, err error) {
	if restore.Status != job.StatusSuccess || restore.ArchivePath == "" {
		return "", "", 0, fmt.Errorf("node: that restore produced no archive")
	}

	// Both sides are resolved before containment is decided: a lexical
	// prefix check is satisfied by a symlink pointing anywhere.
	root, err := filepath.EvalSymlinks(e.settings.StagingRoot)
	if err != nil {
		return "", "", 0, fmt.Errorf("node: resolve the staging area: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(restore.ArchivePath)
	if err != nil {
		return "", "", 0, fmt.Errorf(
			"node: the archive is gone — it is replaced when the account is rebuilt "+
				"again: %w", err)
	}

	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		filepath.IsAbs(relative) {
		return "", "", 0, fmt.Errorf("node: that archive is not in the staging area")
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		return "", "", 0, fmt.Errorf("node: the archive is gone: %w", err)
	}
	return resolved, filepath.Base(resolved), info.Size(), nil
}

const (
	// watchEvery is how often the overdue and stuck checks run. Long
	// enough that reading the account registry costs nothing, short
	// enough that a stuck run is reported the same hour it wedges.
	watchEvery = 5 * time.Minute
	// probeEvery is how often each destination is reached for. A
	// destination that is down is down for hours, not seconds.
	probeEvery = time.Hour
)
