package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/destination"
	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/pkgacct"
	"github.com/shuki/cprest/internal/protocol"
	"github.com/shuki/cprest/internal/resticrun"
	"github.com/shuki/cprest/internal/staging"
)

// Version identifies the agent build to the controller.
const Version = "0.1.0"

// Agent executes backup jobs on one cPanel server.
type Agent struct {
	client   *Client
	provider cpanel.Provider
	staging  *staging.Manager
	runner   *resticrun.Runner
	log      *slog.Logger

	// PollInterval is the pause after an empty poll. The controller holds
	// the request open, so this is a backstop, not the polling rate.
	PollInterval time.Duration
	// Hostname is reported at enrolment.
	Hostname string
	// ResticVersion is reported at enrolment for fleet-wide version
	// tracking: repository features depend on it.
	ResticVersion string
	// TargetTimeout bounds one repository upload. restic retries a
	// failing backend for a long time; without a bound, one unreachable
	// destination consumes the window the reachable ones needed.
	TargetTimeout time.Duration
	// OnProgress, when set, is called as a backup runs. It is called from
	// the goroutine reading restic's output, roughly once a second per
	// repository, so it must not block.
	OnProgress func(jobID, repositoryID string, progress resticrun.Progress)
}

// Config assembles an Agent.
type Config struct {
	Client        *Client
	Provider      cpanel.Provider
	Staging       *staging.Manager
	Runner        *resticrun.Runner
	Log           *slog.Logger
	Hostname      string
	ResticVersion string
	PollInterval  time.Duration
	TargetTimeout time.Duration
}

// New builds an Agent.
func New(cfg Config) *Agent {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	interval := cfg.PollInterval
	if interval == 0 {
		interval = 5 * time.Second
	}
	targetTimeout := cfg.TargetTimeout
	if targetTimeout == 0 {
		targetTimeout = 4 * time.Hour
	}
	return &Agent{
		client:        cfg.Client,
		provider:      cfg.Provider,
		staging:       cfg.Staging,
		runner:        cfg.Runner,
		log:           log,
		Hostname:      cfg.Hostname,
		ResticVersion: cfg.ResticVersion,
		PollInterval:  interval,
		TargetTimeout: targetTimeout,
	}
}

// Enrol reports this host's capabilities to the controller.
func (a *Agent) Enrol(ctx context.Context) error {
	caps, err := a.provider.Capabilities(ctx)
	if err != nil {
		return err
	}
	flags := map[string]string{}
	if caps.NoCompressFlag != "" {
		flags["nocompress"] = caps.NoCompressFlag
	}
	if caps.SkipHomedirFlag != "" {
		flags["skiphomedir"] = caps.SkipHomedirFlag
	}
	if caps.SkipDBFlag != "" {
		flags["skipdb"] = caps.SkipDBFlag
	}
	if caps.NoCompressFlag == "" {
		// Worth saying out loud at startup: without it, every monolithic
		// backup stores a full copy.
		a.log.Warn("pkgacct on this host cannot disable compression; " +
			"monolithic payloads will deduplicate poorly")
	}

	_, err = a.client.Enrol(ctx, protocol.EnrolRequest{
		Hostname:     a.Hostname,
		AgentVersion: Version,
		ResticVer:    a.ResticVersion,
		PkgacctFlags: flags,
		StagingRoot:  a.staging.Root,
	})
	return err
}

// CleanStaleStaging removes staging directories left by a previous process.
//
// Cleaning up only on the success path is not enough: the failure path is
// exactly when the volume is already under pressure.
func (a *Agent) CleanStaleStaging() error {
	dirs, err := a.staging.List()
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if err := a.staging.Release(&dir); err != nil {
			a.log.Error("remove stale staging", "path", dir.Path, "error", err)
			continue
		}
		a.log.Warn("removed stale staging directory", "key", dir.Key, "path", dir.Path)
	}
	return nil
}

// Run polls for jobs until the context is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		assignment, err := a.client.NextWork(ctx)
		switch {
		case errors.Is(err, ErrNoWork):
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return err
		case err != nil:
			a.log.Error("poll for work", "error", err)
		default:
			a.execute(ctx, assignment)
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.PollInterval):
		}
	}
}

// execute performs one assignment and reports it.
//
// Reporting gets its own deadline. Work that consumed its whole budget must
// still be able to say what happened, otherwise the lease expires and it is
// all repeated.
func (a *Agent) execute(ctx context.Context, assignment protocol.Assignment) {
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()

	switch assignment.Kind {
	case protocol.KindBackup:
		report := a.RunJob(ctx, *assignment.Backup)
		if err := a.client.Report(reportCtx, report); err != nil {
			// The lease will expire and the controller will re-queue it;
			// restic tolerates the partial write.
			a.log.Error("report backup", "job_id", report.JobID, "error", err)
		}
	case protocol.KindRestore:
		report := a.RunRestore(ctx, *assignment.Restore)
		if err := a.client.ReportRestore(reportCtx, report); err != nil {
			a.log.Error("report restore", "job_id", report.JobID, "error", err)
		}
	default:
		a.log.Error("unknown work kind", "kind", assignment.Kind)
	}
}

// RunJob stages one account and uploads it to every target.
//
// It always returns a report. A failure to stage fails the whole job; a
// failure to reach one repository fails only that target, because the
// copies that did land are still good.
func (a *Agent) RunJob(ctx context.Context, assignment protocol.JobAssignment) protocol.JobReport {
	report := protocol.JobReport{JobID: assignment.JobID}
	log := a.log.With("job_id", assignment.JobID, "account", assignment.CPanelUser)

	account, err := a.provider.Account(ctx, assignment.CPanelUser)
	if err != nil {
		log.Error("read account", "error", err)
		report.StagingError = err.Error()
		return report
	}
	// Prefer what the host reports now over the controller's stored
	// estimate, which may predate the account growing.
	size := account.SizeBytes
	if assignment.SizeEstimate > size {
		size = assignment.SizeEstimate
	}
	estimate := stagingEstimate(size, pkgacct.Mode(assignment.PayloadMode))

	// Staged under the account, not the job: the paths restic records
	// must be identical every night, or each run becomes its own
	// retention group and nothing is ever pruned.
	dir, err := a.staging.Allocate(assignment.CPanelUser, estimate)
	if err != nil {
		log.Error("allocate staging", "error", err)
		report.StagingError = err.Error()
		return report
	}
	defer func() {
		if err := a.staging.Release(dir); err != nil {
			log.Error("release staging", "error", err)
		}
	}()

	mode := pkgacct.Mode(assignment.PayloadMode)
	payload, err := a.provider.Stage(ctx, cpanel.StageRequest{
		Account: account, StagingDir: dir.Path, Mode: mode,
		SkipHomedir:   assignment.SkipHomedir,
		SkipDatabases: assignment.SkipDatabases,
	})
	if err != nil {
		log.Error("stage payload", "error", err)
		report.StagingError = err.Error()
		return report
	}
	if payload.Degraded {
		log.Warn("payload will deduplicate poorly", "reason", payload.Reason)
	}
	// restic treats a path it cannot read as a warning and carries on, so
	// a missing part would become a snapshot that looks fine and restores
	// an incomplete account. It has to stop the job instead.
	if err := payload.Verify(); err != nil {
		log.Error("staged payload is incomplete", "error", err)
		report.StagingError = err.Error()
		return report
	}

	// pkgacct runs once; the staged payload is uploaded to every target.
	for _, target := range assignment.Targets {
		report.Targets = append(report.Targets,
			a.backupTarget(ctx, log, assignment, target, payload))
	}

	// A destination that failed gets one more attempt while the payload is
	// still staged: an upload rather than another pkgacct. Most of what
	// fails here is a network that was briefly not there.
	if assignment.RetryFailed {
		for i, result := range report.Targets {
			if result.Status != string(job.TargetFailed) {
				continue
			}
			target, found := targetFor(assignment, result.RepositoryID)
			if !found {
				continue
			}
			log.Warn("retrying a destination that failed",
				"repository_id", result.RepositoryID, "first_error", result.Error)
			retried := a.backupTarget(ctx, log, assignment, target, payload)
			if retried.Status == string(job.TargetSuccess) {
				report.Targets[i] = retried
				continue
			}
			// Keep the second error, and say that both attempts failed:
			// "it failed twice" is worth more than either message alone.
			report.Targets[i].Error = fmt.Sprintf(
				"failed twice: %s (and on retry: %s)", result.Error, retried.Error)
		}
	}
	return report
}

// progressFor adapts the hook to what the runner expects, and returns nil
// when nobody is watching so restic's status lines are not even parsed.
func (a *Agent) progressFor(jobID, repositoryID string) func(resticrun.Progress) {
	if a.OnProgress == nil {
		return nil
	}
	return func(progress resticrun.Progress) {
		a.OnProgress(jobID, repositoryID, progress)
	}
}

// Staging holds what pkgacct writes and what mysqldump writes. In split
// mode that is the account's configuration and its database dumps — the
// home directory is backed up where it lies and is never copied — so
// reserving the whole account refuses backups that would have fitted
// easily. It refused a 6.8 GB account on a volume with 6.4 GB free, for a
// job that would have staged a few hundred megabytes.
//
// A fraction is still a guess. It is a deliberate one: too small a
// reservation costs a run that fails partway, which is the same outcome as
// refusing it, while too large costs backups that never run at all.
const (
	splitStagingShare = 0.20
	splitStagingFloor = 512 << 20
)

// stagingEstimate is how much room a payload of this shape needs.
func stagingEstimate(size uint64, mode pkgacct.Mode) uint64 {
	if mode != pkgacct.ModeSplit {
		// One archive of the whole account, written into staging.
		return size
	}
	share := uint64(float64(size) * splitStagingShare)
	if share < splitStagingFloor {
		return splitStagingFloor
	}
	return share
}

// targetFor finds the destination a report line came from.
func targetFor(assignment protocol.JobAssignment, repositoryID string) (protocol.Target, bool) {
	for _, target := range assignment.Targets {
		if target.RepositoryID == repositoryID {
			return target, true
		}
	}
	return protocol.Target{}, false
}

func (a *Agent) backupTarget(ctx context.Context, log *slog.Logger,
	assignment protocol.JobAssignment, target protocol.Target,
	payload pkgacct.Payload) protocol.TargetReport {

	result := protocol.TargetReport{
		RepositoryID: target.RepositoryID,
		Status:       string(job.TargetFailed),
	}

	dest, err := destination.Build(target.Spec)
	if err != nil {
		result.Error = err.Error()
		log.Error("build destination", "repository_id", target.RepositoryID, "error", err)
		return result
	}

	targetCtx, cancel := context.WithTimeout(ctx, a.TargetTimeout)
	defer cancel()

	started := time.Now()
	backup, err := a.runner.Backup(targetCtx, resticrun.Repository{
		Dest:     dest,
		Path:     target.RepoPath,
		Password: target.RepoPassword,
	}, resticrun.BackupSpec{
		Paths:      payload.Paths(),
		Host:       a.Hostname,
		OnProgress: a.progressFor(assignment.JobID, target.RepositoryID),
		// Tags identify the account, and must be stable: retention groups
		// by them, so a per-job tag would put every run in a group of its
		// own and exempt it from pruning. The job a snapshot came from is
		// recorded in the database against its snapshot id.
		Tags: []string{
			"account:" + assignment.CPanelUser,
			"mode:" + string(payload.Mode),
		},
		Exclude:        assignment.Excludes,
		LimitUploadKiB: assignment.LimitUploadKiB,
	})
	result.DurationSecs = time.Since(started).Seconds()

	if err != nil {
		result.Error = err.Error()
		// One unreachable destination must not invalidate the copies that
		// succeeded, so this is recorded and the loop continues.
		log.Error("backup target", "repository_id", target.RepositoryID, "error", err)
		return result
	}

	result.Status = string(job.TargetSuccess)
	result.SnapshotID = backup.Summary.SnapshotID
	result.BytesAdded = backup.Summary.DataAdded
	result.BytesProcessed = backup.Summary.TotalBytesProcessed
	result.Incomplete = backup.Incomplete
	result.Detail = trimDetail(backup.Stderr)
	if backup.Incomplete {
		log.Warn("snapshot is incomplete: some source files could not be read",
			"repository_id", target.RepositoryID, "snapshot_id", result.SnapshotID)
	}
	log.Info("target complete",
		"repository_id", target.RepositoryID, "snapshot_id", result.SnapshotID,
		"bytes_added", result.BytesAdded, "bytes_processed", result.BytesProcessed)
	return result
}

// maxDetail bounds what is kept from restic's error stream. A backup of a
// busy account can name thousands of transient files, and the last of them
// are the ones worth reading.
const maxDetail = 16 << 10

// trimDetail keeps the tail of restic's error output, which is where its
// summary of what it could not read appears.
func trimDetail(stderr string) string {
	trimmed := strings.TrimSpace(stderr)
	if len(trimmed) <= maxDetail {
		return trimmed
	}
	cut := trimmed[len(trimmed)-maxDetail:]
	if newline := strings.IndexByte(cut, '\n'); newline >= 0 {
		cut = cut[newline+1:]
	}
	return "… earlier output omitted …\n" + cut
}
