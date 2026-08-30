package resticrun

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// BackupSummary is restic's final "summary" message from "backup --json".
//
// Field names follow restic's documented scripting output. Unknown fields
// are ignored: restic states that new message types and fields may be added
// over time, so the parser must tolerate them.
type BackupSummary struct {
	MessageType         string    `json:"message_type"`
	FilesNew            uint64    `json:"files_new"`
	FilesChanged        uint64    `json:"files_changed"`
	FilesUnmodified     uint64    `json:"files_unmodified"`
	DirsNew             uint64    `json:"dirs_new"`
	DirsChanged         uint64    `json:"dirs_changed"`
	DirsUnmodified      uint64    `json:"dirs_unmodified"`
	DataBlobs           int64     `json:"data_blobs"`
	TreeBlobs           int64     `json:"tree_blobs"`
	DataAdded           uint64    `json:"data_added"`
	DataAddedPacked     uint64    `json:"data_added_packed"`
	TotalFilesProcessed uint64    `json:"total_files_processed"`
	TotalBytesProcessed uint64    `json:"total_bytes_processed"`
	TotalDuration       float64   `json:"total_duration"`
	BackupStart         time.Time `json:"backup_start"`
	BackupEnd           time.Time `json:"backup_end"`
	SnapshotID          string    `json:"snapshot_id"`
	DryRun              bool      `json:"dry_run"`
}

// ErrNoSummary means restic produced no summary message. That happens when
// the run failed before completing, and must never be reported as success.
var ErrNoSummary = errors.New("resticrun: no summary message in restic output")

// ParseBackupSummary extracts the summary from a "backup --json" stream.
//
// The stream is newline-delimited JSON carrying many status messages before
// the single summary. Malformed lines are skipped rather than fatal: a
// progress line restic garbled under a narrow terminal must not lose us the
// snapshot ID.
func ParseBackupSummary(stdout []byte) (BackupSummary, error) {
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	// Status lines carry file paths and can be long.
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)

	var summary BackupSummary
	found := false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var probe struct {
			MessageType string `json:"message_type"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		if probe.MessageType != "summary" {
			continue
		}
		if err := json.Unmarshal(line, &summary); err != nil {
			return BackupSummary{}, fmt.Errorf("resticrun: decode summary: %w", err)
		}
		found = true
	}
	if err := scanner.Err(); err != nil {
		return BackupSummary{}, fmt.Errorf("resticrun: read restic output: %w", err)
	}
	if !found {
		return BackupSummary{}, ErrNoSummary
	}
	return summary, nil
}

// Exit codes restic documents. Anything else is treated as a plain failure.
const (
	exitOK = 0
	// exitIncompleteRead means "backup" could not read some source files.
	// A snapshot was still created, so the copy exists but is incomplete.
	//
	// The same code means something quite different for other commands —
	// "forget" uses it for "failed to remove one or more snapshots" — so
	// it is only forgiven where a snapshot really was produced.
	exitIncompleteRead = 3
)

// classifyExit maps a restic exit code to an error.
//
// incompleteOK must only be set for "backup". On a live cPanel server,
// transient unreadable files (a session file deleted mid-run, a socket) are
// routine, and the resulting snapshot is still worth keeping; the caller
// records Incomplete and the job is surfaced as a warning. For every other
// command exit 3 is a failure, and treating it as success would let a
// prune blocked by an append-only destination look like a completed
// maintenance run while the repository grows without bound.
func classifyExit(code int, stderr []byte, incompleteOK bool) error {
	if code == exitOK || (incompleteOK && code == exitIncompleteRead) {
		return nil
	}
	return fmt.Errorf("resticrun: restic exited %d: %s", code, explain(stderr, 5))
}

// explain summarises stderr for an error message: the last few meaningful
// lines, with Go stack frames dropped. restic prints a trace on fatal
// errors, and the frames crowd out the line that says what went wrong.
func explain(b []byte, n int) string {
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	kept := make([][]byte, 0, n)
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || isStackFrame(lines[i]) {
			continue
		}
		kept = append([][]byte{line}, kept...)
	}
	if len(kept) == 0 {
		return "(no output)"
	}
	return string(bytes.Join(kept, []byte("; ")))
}

// isStackFrame recognises the two shapes Go traces take: an indented file
// and line, and the qualified function name above it.
func isStackFrame(line []byte) bool {
	if len(line) > 0 && (line[0] == '\t' || line[0] == ' ') {
		return bytes.Contains(line, []byte(".go:")) || bytes.Contains(line, []byte(".s:"))
	}
	trimmed := bytes.TrimSpace(line)
	return bytes.HasPrefix(trimmed, []byte("runtime.")) ||
		bytes.HasPrefix(trimmed, []byte("main.init")) ||
		bytes.HasPrefix(trimmed, []byte("github.com/restic/restic/"))
}
