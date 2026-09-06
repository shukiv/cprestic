package resticrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	NeedsCompletionTag   = "cprest:read-status-v1"
	CompletionReceiptTag = "cprest:completion-v1"
	completedIDPrefix    = "completed:"
)

func hasTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}

// recordCompletion adds a tiny independent snapshot after a successful read.
// Retagging a payload would require deleting/replacing its snapshot, which
// cannot work on append-only storage and would invalidate its recorded ID.
// The initial payload is unverified even if the agent dies before reporting.
func (r *Runner) recordCompletion(ctx context.Context, repo Repository, host, id string) error {
	if err := validateSnapshotID(id); err != nil {
		return err
	}
	if id == "latest" {
		return fmt.Errorf("resticrun: completion needs an exact snapshot")
	}
	dir, err := os.MkdirTemp(r.cfg.RuntimeDir, "gniza-completion-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	filename := filepath.Join(dir, "completed")
	if err := os.WriteFile(filename, []byte(id+"\n"), 0600); err != nil {
		return err
	}
	args, err := BackupArgs(BackupSpec{Paths: []string{filename}, Host: host,
		Tags: []string{CompletionReceiptTag, completedIDPrefix + id}})
	if err != nil {
		return err
	}
	result, err := r.run(ctx, repo, args, secondary{}, nil)
	if err != nil {
		return err
	}
	if err := classifyExit(result.ExitCode, result.Stderr, false); err != nil {
		return err
	}
	summary, err := ParseBackupSummary(result.Stdout)
	if err != nil {
		return err
	}
	if summary.SnapshotID == "" {
		return fmt.Errorf("resticrun: completion receipt has no snapshot id")
	}
	return nil
}

func (r *Runner) completionReceipts(ctx context.Context, repo Repository) (map[string]bool, error) {
	snapshots, err := r.rawSnapshots(ctx, repo, SnapshotFilter{Tags: []string{CompletionReceiptTag}})
	if err != nil {
		return nil, err
	}
	completed := map[string]bool{}
	for _, snapshot := range snapshots {
		if !hasTag(snapshot.Tags, CompletionReceiptTag) {
			continue
		}
		for _, tag := range snapshot.Tags {
			if id, ok := strings.CutPrefix(tag, completedIDPrefix); ok && validateSnapshotID(id) == nil && id != "latest" {
				completed[id] = true
			}
		}
	}
	return completed, nil
}
