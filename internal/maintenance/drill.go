package maintenance

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/pkgacct"
	"github.com/shuki/cprest/internal/reassemble"
	"github.com/shuki/cprest/internal/resticrun"
)

// DrillRequest asks for a restore rehearsal.
type DrillRequest struct {
	RepositoryID string
	// Account is the cPanel user to rehearse. Empty picks the account of
	// the repository's most recent snapshot.
	Account string
	// WorkDir is scratch space; it is emptied when the drill finishes.
	WorkDir string
}

// DrillResult summarises a rehearsal.
type DrillResult struct {
	Account       string
	SnapshotID    string
	Mode          pkgacct.Mode
	BytesRestored uint64
	// Checks are the structural assertions that passed, in order, so a
	// recorded drill says what was actually verified.
	Checks []string
}

// Drill restores a snapshot into scratch space and checks that what comes
// out looks like an account, then throws it away.
//
// The checks are structural. Nothing here can tell you cPanel would accept
// the archive — only a real restorepkg on a real host can — but a drill
// that fails means the backup certainly cannot be restored, which is the
// question worth answering nightly. See docs/DESIGN.md §10.
func (r *Runner) Drill(ctx context.Context, req DrillRequest) (DrillResult, error) {
	var result DrillResult

	err := r.withRun(ctx, req.RepositoryID, KindDrill, func(repo resticrun.Repository) (string, error) {
		account := req.Account
		snapshotID := ""

		filter := resticrun.SnapshotFilter{Latest: 1}
		if account != "" {
			filter.Tags = []string{"account:" + account}
		}
		snapshots, err := r.restic.Snapshots(ctx, repo, filter)
		if err != nil {
			return "", err
		}
		if len(snapshots) == 0 {
			return "", fmt.Errorf("maintenance: repository holds no snapshot to rehearse")
		}
		newest := snapshots[len(snapshots)-1]
		snapshotID = newest.ID
		if account == "" {
			account = newest.Account()
		}
		if account == "" {
			return "", fmt.Errorf("maintenance: snapshot %s has no account tag", newest.ShortID)
		}

		workDir := req.WorkDir
		if workDir == "" {
			workDir, err = os.MkdirTemp("", "cprest-drill-")
			if err != nil {
				return "", fmt.Errorf("maintenance: create drill scratch: %w", err)
			}
		}
		// A drill's output is proof, not a deliverable.
		defer func() { _ = os.RemoveAll(workDir) }()

		rebuilt, err := reassemble.Run(ctx, r.restic, reassemble.Request{
			Account:    account,
			SnapshotID: snapshotID,
			WorkDir:    workDir,
			Repo:       repo,
		})
		if err != nil {
			return "", err
		}

		checks, err := reassemble.Verify(rebuilt)
		result = DrillResult{
			Account:       account,
			SnapshotID:    snapshotID,
			Mode:          rebuilt.Mode,
			BytesRestored: rebuilt.BytesRestored,
			Checks:        checks,
		}
		if err != nil {
			return "", err
		}
		r.log.Info("restore drill passed",
			"repository_id", req.RepositoryID, "account", account,
			"snapshot", snapshotID, "checks", strings.Join(checks, ", "))
		_, detail := drillOutcome(result, nil)
		return detail, nil
	})
	return result, err
}

// drillOutcome renders a drill result for the maintenance_runs row.
func drillOutcome(result DrillResult, err error) (string, string) {
	if err != nil {
		return string(job.StatusFailed), err.Error()
	}
	return string(job.StatusSuccess), fmt.Sprintf(
		"account=%s snapshot=%s mode=%s bytes=%d checks=%s",
		result.Account, result.SnapshotID, result.Mode, result.BytesRestored,
		strings.Join(result.Checks, "; "))
}
