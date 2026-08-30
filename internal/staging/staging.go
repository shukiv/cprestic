// Package staging manages the scratch space pkgacct writes into.
//
// pkgacct needs free disk roughly equal to the account's size, on the
// server we are trying not to disrupt. Filling that volume takes the
// customer's sites down, so space is checked before a job starts rather
// than discovered when a write fails. See docs/DESIGN.md §12.
package staging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Manager allocates and reclaims staging directories under one root.
type Manager struct {
	// Root is the staging parent directory. Operators size this volume
	// deliberately; it should not be /tmp, and it should not share a
	// filesystem with the accounts being backed up.
	Root string
	// SafetyMarginRatio is extra free space required beyond the estimated
	// payload size, as a fraction. 0.2 means require 120% of the estimate.
	SafetyMarginRatio float64
	// MaxConcurrent caps simultaneously staged accounts so several large
	// accounts cannot collectively exhaust the volume.
	MaxConcurrent int
}

// Dir is an allocated staging directory.
type Dir struct {
	Path string
	// Key identifies what is staged here. It is stable across runs — the
	// account name, not the job id — so the paths restic records in a
	// snapshot are the same every night. Paths that changed per job would
	// give every run its own "restic forget" group, and a per-run group
	// of one is never pruned.
	Key string
}

// ErrInsufficientSpace is returned when the staging volume cannot safely
// hold the estimated payload.
type ErrInsufficientSpace struct {
	Required  uint64
	Available uint64
}

func (e *ErrInsufficientSpace) Error() string {
	return fmt.Sprintf("staging: need %d bytes free, have %d", e.Required, e.Available)
}

// Allocate reserves a staging directory after checking space.
//
// key must be stable for the thing being staged, normally the account
// name. estimatedBytes should come from the account's recorded size.
// Callers that have no estimate must not pass zero to bypass the check;
// they should refuse the job instead, because an unbounded pkgacct on a
// nearly full volume is exactly the failure this guards against.
func (m *Manager) Allocate(key string, estimatedBytes uint64) (*Dir, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if m.Root == "" {
		return nil, fmt.Errorf("staging: root is not configured")
	}
	if estimatedBytes == 0 {
		return nil, fmt.Errorf("staging: refusing to stage %s without a size estimate", key)
	}

	required := estimatedBytes
	if m.SafetyMarginRatio > 0 {
		required = uint64(float64(estimatedBytes) * (1 + m.SafetyMarginRatio))
	}
	available, err := AvailableBytes(m.Root)
	if err != nil {
		return nil, err
	}
	if available < required {
		return nil, &ErrInsufficientSpace{Required: required, Available: available}
	}

	if m.MaxConcurrent > 0 {
		active, err := m.List()
		if err != nil {
			return nil, err
		}
		if len(active) >= m.MaxConcurrent {
			return nil, fmt.Errorf("staging: %d accounts already staged, limit is %d",
				len(active), m.MaxConcurrent)
		}
	}

	path := filepath.Join(m.Root, dirPrefix+key)
	if err := os.Mkdir(path, 0o700); err != nil {
		// An existing directory means crash debris the startup sweep has
		// not cleaned up, since the controller leases only one running
		// job per account. Failing loudly beats staging into a
		// half-written tree.
		return nil, fmt.Errorf("staging: create %s: %w", path, err)
	}
	return &Dir{Path: path, Key: key}, nil
}

// List returns the staging directories currently on disk.
//
// The agent calls this at startup to clear what a previous process left
// behind. Cleaning up only on the success path is not enough: the failure
// path is precisely when the volume is already under pressure.
func (m *Manager) List() ([]Dir, error) {
	entries, err := os.ReadDir(m.Root)
	if err != nil {
		return nil, fmt.Errorf("staging: read root: %w", err)
	}
	var dirs []Dir
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), dirPrefix) {
			continue
		}
		dirs = append(dirs, Dir{
			Path: filepath.Join(m.Root, entry.Name()),
			Key:  strings.TrimPrefix(entry.Name(), dirPrefix),
		})
	}
	return dirs, nil
}

// Reclaim removes a leftover staging directory for a key, if one exists,
// and reports whether it did.
//
// Allocate deliberately refuses a directory that is already there, so
// anything that legitimately supersedes a previous run — a second restore
// of the same account, whose rebuilt archive replaces the last one — calls
// this first rather than failing.
func (m *Manager) Reclaim(key string) (bool, error) {
	if err := validateKey(key); err != nil {
		return false, err
	}
	path := filepath.Join(m.Root, dirPrefix+key)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("staging: stat %s: %w", path, err)
	}
	if err := m.Release(&Dir{Path: path, Key: key}); err != nil {
		return false, err
	}
	return true, nil
}

// Release removes a staging directory. It is called only once every target
// of the job has reached a terminal state, so that a retry against a slow
// destination does not have to re-run pkgacct.
func (m *Manager) Release(dir *Dir) error {
	if dir == nil {
		return nil
	}
	if !strings.HasPrefix(filepath.Clean(dir.Path), filepath.Clean(m.Root)+string(os.PathSeparator)) {
		return fmt.Errorf("staging: refusing to remove %q outside root %q", dir.Path, m.Root)
	}
	if err := os.RemoveAll(dir.Path); err != nil {
		return fmt.Errorf("staging: remove %s: %w", dir.Path, err)
	}
	return nil
}

// AvailableBytes reports free space on the filesystem holding path,
// counting only space available to unprivileged writes.
func AvailableBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("staging: statfs %s: %w", path, err)
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

const dirPrefix = "stage-"

// validateKey rejects identifiers that could escape the staging root once
// joined into a path.
func validateKey(key string) error {
	if key == "" {
		return fmt.Errorf("staging: key is empty")
	}
	for _, r := range key {
		isAllowed := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !isAllowed {
			return fmt.Errorf("staging: key %q contains an unsupported character", key)
		}
	}
	return nil
}
