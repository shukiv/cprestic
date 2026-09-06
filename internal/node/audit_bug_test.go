package node_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/node"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/resticrun"
)

// A schedule may back up less than the whole account: the databases, the
// home directory or the mail can be left out. Such a snapshot is not
// interchangeable with a full one, and whole-account recovery picks a
// snapshot by account and date rather than by hand -- so before this it
// took the newest, and an operator rebuilding a customer got an account
// with no databases and a restore that reported success.
//
// The backup now records what it was taken without, and recovery uses the
// newest backup that holds the whole account.
func TestWholeAccountRecoveryWillNotUseAPartialBackup(t *testing.T) {
	now := time.Now().UTC()
	engine, repo := recoveryFixture(t, `[
		{"id":"full-old","time":"`+now.Add(-2*time.Hour).Format(time.RFC3339Nano)+`","tags":["account:customer1","mode:split"],"paths":["/stage/metadata","/home/customer1","/stage/databases"]},
		{"id":"partial-new","time":"`+now.Add(-time.Hour).Format(time.RFC3339Nano)+`","tags":["account:customer1","mode:split","skip:databases"],"paths":["/stage/metadata","/home/customer1"]}
	]`)

	chosen, err := engine.SnapshotAsOf(context.Background(), repo.ID, "customer1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if chosen != "full-old" {
		t.Fatalf("recovery chose %q; a backup taken without the databases would "+
			"give the customer back an account with none, and report success",
			chosen)
	}
}

// When every backup is a partial one there is nothing safe to choose, and
// the refusal has to say what is missing: silently restoring the newest is
// the whole defect, and refusing without saying why leaves an operator in
// a disaster with no way forward.
func TestWholeAccountRecoverySaysWhatEveryBackupWasTakenWithout(t *testing.T) {
	now := time.Now().UTC()
	engine, repo := recoveryFixture(t, `[
		{"id":"partial-old","time":"`+now.Add(-2*time.Hour).Format(time.RFC3339Nano)+`","tags":["account:customer1","mode:split","skip:databases"],"paths":["/stage/metadata","/home/customer1"]},
		{"id":"partial-new","time":"`+now.Add(-time.Hour).Format(time.RFC3339Nano)+`","tags":["account:customer1","mode:split","skip:databases","skip:email"],"paths":["/stage/metadata","/home/customer1"]}
	]`)

	_, err := engine.SnapshotAsOf(context.Background(), repo.ID, "customer1", time.Time{})
	if err == nil {
		t.Fatal("a whole-account recovery was made from a backup that holds " +
			"less than the whole account")
	}
	for _, said := range []string{"databases", "email", "partial-new"} {
		if !strings.Contains(err.Error(), said) {
			t.Errorf("the refusal does not mention %q: %v", said, err)
		}
	}
}

// recoveryFixture is an engine whose restic answers every snapshot listing
// with the given JSON, and a repository to ask about.
func recoveryFixture(t *testing.T, snapshots string) (*node.Engine, nodestore.Repository) {
	t.Helper()
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	exec := resticrun.ExecFunc(func(_ context.Context, _ resticrun.Command) (resticrun.CommandResult, error) {
		return resticrun.CommandResult{Stdout: []byte(snapshots)}, nil
	})
	engine := newEngineWithExec(t, store, root, exec)
	_, repo, err := engine.AddDestination(nodestore.Destination{
		Name: "Local", Type: "local", Config: map[string]string{"root": t.TempDir()},
	}, nil, "backups")
	if err != nil {
		t.Fatal(err)
	}
	return engine, repo
}
