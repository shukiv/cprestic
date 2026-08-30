// Package testsupport spins up disposable dependencies for tests.
//
// Tests that need PostgreSQL, restic or rest-server call these helpers and
// skip when the binary is absent, so "go test ./..." stays runnable on a
// machine that has none of them.
package testsupport

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// PostgresDSN starts a throwaway PostgreSQL cluster and returns a DSN for a
// fresh database inside it. The cluster is stopped and deleted when the test
// finishes.
//
// The cluster listens on a unix socket in a temporary directory rather than
// a TCP port, so concurrent test runs cannot collide.
func PostgresDSN(t *testing.T) string {
	t.Helper()

	binDir := findPostgresBinDir()
	if binDir == "" {
		t.Skip("no PostgreSQL installation found; skipping")
	}

	// initdb refuses a socket directory whose path is too long for
	// sockaddr_un, so keep the base short.
	base, err := os.MkdirTemp("", "cprest-pg-")
	if err != nil {
		t.Fatalf("testsupport: temp dir: %v", err)
	}
	dataDir := filepath.Join(base, "data")

	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(filepath.Join(binDir, name), args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("testsupport: %s failed: %v\n%s", name, err, output)
		}
	}

	run("initdb", "-D", dataDir, "-U", "cprest", "--auth=trust", "--no-sync")
	// -h '' disables TCP entirely; -k sets the unix socket directory.
	run("pg_ctl", "-D", dataDir, "-o", fmt.Sprintf("-k %s -h ''", base),
		"-l", filepath.Join(base, "postgres.log"), "-w", "start")

	t.Cleanup(func() {
		stop := exec.Command(filepath.Join(binDir, "pg_ctl"), "-D", dataDir, "-m", "immediate", "stop")
		_ = stop.Run()
		_ = os.RemoveAll(base)
	})

	dbName := fmt.Sprintf("cprest_%d", time.Now().UnixNano())
	createDB := exec.Command(filepath.Join(binDir, "createdb"), "-h", base, "-U", "cprest", dbName)
	if output, err := createDB.CombinedOutput(); err != nil {
		t.Fatalf("testsupport: createdb failed: %v\n%s", err, output)
	}

	return fmt.Sprintf("postgres://cprest@/%s?host=%s", dbName, base)
}

// findPostgresBinDir locates initdb, preferring PATH and falling back to the
// versioned directories Debian and Red Hat install into.
func findPostgresBinDir() string {
	if path, err := exec.LookPath("initdb"); err == nil {
		return filepath.Dir(path)
	}
	var candidates []string
	for _, pattern := range []string{
		"/usr/lib/postgresql/*/bin/initdb",
		"/usr/pgsql-*/bin/initdb",
		"/opt/homebrew/opt/postgresql*/bin/initdb",
	} {
		matches, _ := filepath.Glob(pattern)
		candidates = append(candidates, matches...)
	}
	if len(candidates) == 0 {
		return ""
	}
	// Highest version last.
	sort.Strings(candidates)
	return filepath.Dir(candidates[len(candidates)-1])
}

// RequireBinary skips the test unless the named executable is on PATH, and
// returns its absolute path.
func RequireBinary(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		// go install puts binaries in GOBIN or GOPATH/bin, which is often
		// missing from a test runner's PATH.
		for _, dir := range goBinDirs() {
			candidate := filepath.Join(dir, name)
			if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
				return candidate
			}
		}
		t.Skipf("%s not found on PATH; skipping", name)
	}
	return path
}

func goBinDirs() []string {
	var dirs []string
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		dirs = append(dirs, gobin)
	}
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		for _, part := range strings.Split(gopath, string(os.PathListSeparator)) {
			dirs = append(dirs, filepath.Join(part, "bin"))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "go", "bin"))
	}
	return dirs
}

// WaitFor polls until check returns nil or the timeout expires.
func WaitFor(ctx context.Context, timeout time.Duration, check func() error) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = check(); lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out after %s: %w", timeout, lastErr)
}
