package resticrun

import (
	"context"
	"encoding/json"
	"fmt"
)

// RestoreSize resolves historical snapshots without a summary by reading
// their tree statistics. Missing metadata is not evidence of an empty backup.
func (r *Runner) RestoreSize(ctx context.Context, repo Repository, snapshot Snapshot) (uint64, error) {
	if snapshot.Summary.TotalBytesProcessed > 0 {
		return snapshot.Summary.TotalBytesProcessed, nil
	}
	if err := validateSnapshotID(snapshot.ID); err != nil {
		return 0, err
	}
	if snapshot.ID == "latest" {
		return 0, fmt.Errorf("resticrun: restore size requires an exact snapshot")
	}
	result, err := r.run(ctx, repo, []string{"stats", "--json", "--mode", "restore-size", snapshot.ID}, secondary{}, nil)
	if err != nil {
		return 0, err
	}
	if err := classifyExit(result.ExitCode, result.Stderr, false); err != nil {
		return 0, err
	}
	var stats struct {
		TotalSize uint64 `json:"total_size"`
	}
	if result.Truncated || json.Unmarshal(result.Stdout, &stats) != nil || stats.TotalSize == 0 {
		return 0, fmt.Errorf("resticrun: cannot establish restore size for snapshot %s; refusing an unbounded restore", snapshot.ID)
	}
	return stats.TotalSize, nil
}
