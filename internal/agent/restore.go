package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/shuki/cprest/internal/destination"
	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/protocol"
	"github.com/shuki/cprest/internal/reassemble"
	"github.com/shuki/cprest/internal/resticrun"
	"github.com/shuki/cprest/internal/staging"
)

// RunRestore brings an account, or named files from it, back from a
// snapshot.
//
// It always returns a report. Nothing is handed to cPanel unless the
// operator asked for it: materialising files is safe, overwriting a live
// account is not.
func (a *Agent) RunRestore(ctx context.Context, assignment protocol.RestoreAssignment) protocol.RestoreReport {
	report := protocol.RestoreReport{
		JobID:  assignment.JobID,
		Status: string(job.StatusFailed),
	}
	log := a.log.With("restore_job_id", assignment.JobID,
		"account", assignment.CPanelUser, "snapshot", assignment.SnapshotID)

	started := time.Now()
	defer func() { report.DurationSecs = time.Since(started).Seconds() }()

	repo, err := a.restoreSource(assignment)
	if err != nil {
		log.Error("build restore source", "error", err)
		report.Error = err.Error()
		return report
	}

	// A restore writes roughly what the account occupies, so it goes
	// through the same space preflight as a backup. The staging key is
	// distinct so a restore cannot collide with a backup's directory.
	estimate := assignment.SizeEstimate
	if estimate == 0 {
		estimate = 1 << 20
	}
	stagingKey := "restore-" + assignment.CPanelUser
	// A previous restore of this account may have left its rebuilt archive
	// here. This restore supersedes it, so the space is reclaimed rather
	// than allowed to wedge every future restore of the account.
	if reclaimed, err := a.staging.Reclaim(stagingKey); err != nil {
		log.Error("reclaim previous restore staging", "error", err)
		report.Error = err.Error()
		return report
	} else if reclaimed {
		log.Warn("removed a previous restore's staging directory", "key", stagingKey)
	}

	dir, err := a.staging.Allocate(stagingKey, estimate)
	if err != nil {
		log.Error("allocate restore staging", "error", err)
		report.Error = err.Error()
		return report
	}
	keepStaging := false
	defer func() {
		if keepStaging {
			// The rebuilt archive lives here and an operator still needs
			// it. Releasing would delete the thing we just produced.
			//
			// It survives until the next restore of this account or the
			// next agent restart, whichever comes first: the startup sweep
			// cannot tell a finished restore's output from a crashed one's
			// debris. The path is recorded on the restore job so an
			// operator knows what was there.
			return
		}
		if err := a.staging.Release(dir); err != nil {
			log.Error("release restore staging", "error", err)
		}
	}()

	restoreCtx, cancel := context.WithTimeout(ctx, a.TargetTimeout)
	defer cancel()

	switch assignment.Kind {
	case protocol.RestoreFiles:
		return a.restoreFiles(restoreCtx, log, assignment, repo, dir, report)
	case protocol.RestoreAccount, "":
		result, err := a.restoreAccount(restoreCtx, log, assignment, repo, dir)
		if err != nil {
			report.Error = err.Error()
			return report
		}
		keepStaging = !assignment.Apply
		report.Status = string(job.StatusSuccess)
		report.BytesRestored = result.BytesRestored
		report.ArchivePath = result.ArchivePath
		report.Applied = assignment.Apply
		return report
	default:
		report.Error = fmt.Sprintf("agent: unknown restore kind %q", assignment.Kind)
		return report
	}
}

// restoreAccount rebuilds the account archive and, when asked, hands it to
// cPanel.
func (a *Agent) restoreAccount(ctx context.Context, log *slog.Logger,
	assignment protocol.RestoreAssignment, repo resticrun.Repository,
	dir *staging.Dir) (reassemble.Result, error) {

	result, err := reassemble.Run(ctx, a.runner, reassemble.Request{
		Account:    assignment.CPanelUser,
		SnapshotID: assignment.SnapshotID,
		WorkDir:    dir.Path,
		Repo:       repo,
	})
	if err != nil {
		log.Error("rebuild account archive", "error", err)
		return reassemble.Result{}, err
	}
	log.Info("account archive rebuilt",
		"archive", result.ArchivePath, "mode", result.Mode,
		"bytes_restored", result.BytesRestored)

	if !assignment.Apply {
		// The default. An operator inspects the archive and applies it
		// themselves, or asks for a job with apply set.
		log.Info("archive left in place; restorepkg not run", "archive", result.ArchivePath)
		return result, nil
	}

	log.Warn("applying restore to the live account", "archive", result.ArchivePath)
	if err := a.provider.Apply(ctx, result.ArchivePath); err != nil {
		log.Error("restorepkg", "error", err)
		return reassemble.Result{}, err
	}
	log.Info("account restored")
	return result, nil
}

// restoreFiles pulls named paths out of a snapshot, keeping the paths they
// had, and leaves them where the operator asked.
func (a *Agent) restoreFiles(ctx context.Context, log *slog.Logger,
	assignment protocol.RestoreAssignment, repo resticrun.Repository,
	dir *staging.Dir, report protocol.RestoreReport) protocol.RestoreReport {

	if len(assignment.IncludePaths) == 0 {
		report.Error = "agent: a files restore needs at least one path"
		return report
	}

	target := assignment.TargetDir
	if target == "" {
		target = filepath.Join(dir.Path, "files")
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		report.Error = fmt.Sprintf("agent: create restore target: %v", err)
		return report
	}

	restored, err := a.runner.Restore(ctx, repo, resticrun.RestoreSpec{
		SnapshotID: assignment.SnapshotID,
		Target:     target,
		// Include keeps the original directory structure, so the operator
		// can see where each file came from.
		Include: assignment.IncludePaths,
	})
	if err != nil {
		log.Error("restore files", "error", err)
		report.Error = err.Error()
		return report
	}
	if restored.FilesRestored == 0 {
		report.Error = "agent: no files in the snapshot matched the requested paths"
		return report
	}

	log.Info("files restored", "target", target,
		"files", restored.FilesRestored, "bytes", restored.BytesRestored)
	report.Status = string(job.StatusSuccess)
	report.BytesRestored = restored.BytesRestored
	report.RestoredTo = target
	return report
}

func (a *Agent) restoreSource(assignment protocol.RestoreAssignment) (resticrun.Repository, error) {
	dest, err := destination.Build(assignment.Source.Spec)
	if err != nil {
		return resticrun.Repository{}, err
	}
	return resticrun.Repository{
		Dest:     dest,
		Path:     assignment.Source.RepoPath,
		Password: assignment.Source.RepoPassword,
	}, nil
}
