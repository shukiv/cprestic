package staging

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAllocateRefusesWithoutEstimate(t *testing.T) {
	manager := &Manager{Root: t.TempDir()}
	if _, err := manager.Allocate("customer1", 0); err == nil {
		t.Error("a zero estimate must not bypass the space check")
	}
}

func TestAllocateKeyIsStableAcrossRuns(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{Root: root}

	// The same account must stage to the same path every night: restic
	// records those paths in the snapshot, and paths that change per run
	// give every run its own retention group, which is then never pruned.
	first, err := manager.Allocate("customer1", 1024)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if err := manager.Release(first); err != nil {
		t.Fatalf("Release: %v", err)
	}
	second, err := manager.Allocate("customer1", 1024)
	if err != nil {
		t.Fatalf("second Allocate: %v", err)
	}
	if first.Path != second.Path {
		t.Errorf("paths differ between runs: %q then %q", first.Path, second.Path)
	}
}

func TestAllocateRefusesADirectoryAlreadyInUse(t *testing.T) {
	manager := &Manager{Root: t.TempDir(), MaxConcurrent: 4}
	if _, err := manager.Allocate("customer1", 1024); err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	// Either a concurrent run, which the controller prevents, or crash
	// debris the startup sweep missed. Both deserve a loud failure.
	if _, err := manager.Allocate("customer1", 1024); err == nil {
		t.Error("staging the same account twice should be refused")
	}
}

func TestAllocateRejectsUnsafeKey(t *testing.T) {
	manager := &Manager{Root: t.TempDir()}
	for _, key := range []string{"", "../escape", "a/b", "a b", "a;rm"} {
		if _, err := manager.Allocate(key, 1024); err == nil {
			t.Errorf("key %q should be rejected", key)
		}
	}
}

func TestAllocateInsufficientSpace(t *testing.T) {
	manager := &Manager{Root: t.TempDir(), SafetyMarginRatio: 0.2}
	// Ask for more than any test filesystem can offer.
	_, err := manager.Allocate("job1", 1<<62)
	var spaceErr *ErrInsufficientSpace
	if !errors.As(err, &spaceErr) {
		t.Fatalf("err = %v, want ErrInsufficientSpace", err)
	}
	if spaceErr.Required <= 1<<62 {
		t.Errorf("Required = %d, safety margin was not applied", spaceErr.Required)
	}
}

func TestAllocateListRelease(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{Root: root}

	dir, err := manager.Allocate("customer1", 1024)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if want := filepath.Join(root, "stage-customer1"); dir.Path != want {
		t.Errorf("Path = %q, want %q", dir.Path, want)
	}
	info, err := os.Stat(dir.Path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("mode = %04o, want 0700", perm)
	}

	// List must find directories left behind by a crashed agent, not just
	// ones this process allocated.
	listed, err := manager.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].Key != "customer1" {
		t.Fatalf("List = %+v, want one entry for customer1", listed)
	}

	if err := manager.Release(dir); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(dir.Path); !os.IsNotExist(err) {
		t.Error("staging directory should be gone after Release")
	}
}

func TestConcurrencyCap(t *testing.T) {
	manager := &Manager{Root: t.TempDir(), MaxConcurrent: 2}
	for i, jobID := range []string{"a", "b"} {
		if _, err := manager.Allocate(jobID, 1024); err != nil {
			t.Fatalf("Allocate %d: %v", i, err)
		}
	}
	if _, err := manager.Allocate("c", 1024); err == nil {
		t.Error("allocation beyond MaxConcurrent should be rejected")
	}
}

func TestReleaseRefusesOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	manager := &Manager{Root: root}
	if err := manager.Release(&Dir{Path: outside, Key: "x"}); err == nil {
		t.Error("Release outside the staging root should be refused")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("Release must not have removed the outside directory")
	}
}
