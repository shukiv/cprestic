// Package job models a backup job and its per-repository outcomes.
//
// A job that reached two of three repositories is neither a success nor a
// failure, so job status is not a boolean. See docs/DESIGN.md §9.
package job

import "time"

// Status is the rolled-up state of a whole backup job.
type Status string

const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	StatusSuccess Status = "success"
	// StatusPartialSuccess means at least one repository holds a good
	// copy and at least one target failed. It warns; it does not page.
	StatusPartialSuccess Status = "partial_success"
	StatusFailed         Status = "failed"
	StatusCancelled      Status = "cancelled"
)

// TargetStatus is the state of one repository target within a job.
type TargetStatus string

const (
	TargetPending TargetStatus = "pending"
	TargetRunning TargetStatus = "running"
	TargetSuccess TargetStatus = "success"
	TargetFailed  TargetStatus = "failed"
	// TargetSkipped covers targets deliberately not attempted, for
	// instance a repository under maintenance. A skipped target neither
	// contributes a copy nor counts as a failure.
	TargetSkipped TargetStatus = "skipped"
)

// TargetResult records what happened for one repository.
type TargetResult struct {
	RepositoryID string
	Status       TargetStatus
	SnapshotID   string
	// BytesAdded is restic's data_added: what this backup actually cost in
	// new storage. BytesProcessed is total_bytes_processed, the size of
	// the payload that was read. Recording both makes deduplication
	// effectiveness visible per account.
	BytesAdded     uint64
	BytesProcessed uint64
	Duration       time.Duration
	Attempt        int
	// Incomplete marks a snapshot created while some source files could
	// not be read.
	Incomplete bool
	Err        string
}

// Rollup derives job status from its targets.
//
// A job with no targets is a failure, not a silent pass: a policy that
// resolved to zero repositories is a misconfiguration, and reporting it as
// success would hide accounts that are not being backed up at all.
func Rollup(targets []TargetResult) Status {
	if len(targets) == 0 {
		return StatusFailed
	}

	var succeeded, failed, unfinished int
	for _, target := range targets {
		switch target.Status {
		case TargetSuccess:
			if target.Incomplete {
				failed++
			} else {
				succeeded++
			}
		case TargetFailed:
			failed++
		case TargetPending, TargetRunning:
			unfinished++
		case TargetSkipped:
			// Neither a copy nor a failure.
		}
	}

	switch {
	case unfinished > 0:
		return StatusRunning
	case succeeded > 0 && failed > 0:
		return StatusPartialSuccess
	case succeeded > 0:
		return StatusSuccess
	default:
		// Everything failed, or every target was skipped and no copy was
		// made. Both mean no backup exists for this run.
		return StatusFailed
	}
}

// Terminal reports whether a status will not change without a new attempt.
func (s Status) Terminal() bool {
	switch s {
	case StatusSuccess, StatusPartialSuccess, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}
