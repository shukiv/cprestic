package node_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/node"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/vault"
)

func newEngine(t *testing.T, store *nodestore.Store, root string) *node.Engine {
	t.Helper()

	keyHex, err := vault.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "master.key")
	if err := os.WriteFile(keyPath, []byte(keyHex), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := vault.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.New(key)
	if err != nil {
		t.Fatal(err)
	}

	engine, err := node.New(node.Config{
		Store: store, Vault: v,
		Provider: &cpanel.Fake{Root: filepath.Join(root, "cpanel")},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	return engine
}

// TestRecoverFromRestartUnwedgesAnAccount covers the failure a standalone
// server has no lease expiry to catch: the process dies mid-backup, the job
// stays "running" forever, and every later backup of that account is
// skipped as busy — silently, every night.
func TestRecoverFromRestartUnwedgesAnAccount(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}

	// What a crash leaves: a job that never finished, a restore that never
	// finished, and their staging directories.
	crashed, err := store.PutJob(nodestore.Job{Account: "customer1", Status: job.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutRestore(nodestore.Restore{
		Account: "customer2", SnapshotID: "abc", Status: job.StatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"stage-customer1", "stage-restore-customer2"} {
		if err := os.MkdirAll(filepath.Join(settings.StagingRoot, key), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	engine := newEngine(t, store, root)

	recovered, err := store.Job(crashed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != job.StatusFailed {
		t.Errorf("interrupted job status = %q, want failed", recovered.Status)
	}
	if recovered.StagingErr == "" {
		t.Error("the interrupted job does not say why it failed")
	}
	if recovered.FinishedAt == nil {
		t.Error("the interrupted job has no finish time")
	}

	restores, err := store.Restores(0)
	if err != nil {
		t.Fatal(err)
	}
	if restores[0].Status != job.StatusFailed {
		t.Errorf("interrupted restore status = %q, want failed", restores[0].Status)
	}

	// The debris is gone, so the next attempt can allocate its staging.
	entries, err := os.ReadDir(settings.StagingRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("staging still holds %d directories from the previous run", len(entries))
	}

	// And the account is no longer considered busy.
	if _, err := engine.QueueBackup("policy", "customer1"); err != nil {
		t.Errorf("the account is still wedged after recovery: %v", err)
	}
}

func TestQueueRestoreValidates(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, store, root)

	if _, err := engine.QueueRestore(nodestore.Restore{Account: "c1"}); err == nil {
		t.Error("a restore with no snapshot should be refused")
	}
	// A files restore with no paths would restore nothing at all.
	if _, err := engine.QueueRestore(nodestore.Restore{
		Account: "c1", SnapshotID: "abc", Kind: "files",
	}); err == nil {
		t.Error("a files restore with no paths should be refused")
	}
	// restorepkg takes a whole account archive, so this combination is
	// meaningless rather than merely unusual.
	if _, err := engine.QueueRestore(nodestore.Restore{
		Account: "c1", SnapshotID: "abc", Kind: "files",
		IncludePaths: []string{"/home/c1/x"}, Apply: true,
	}); err == nil {
		t.Error("applying a files restore should be refused")
	}

	queued, err := engine.QueueRestore(nodestore.Restore{Account: "c1", SnapshotID: "abc"})
	if err != nil {
		t.Fatalf("QueueRestore: %v", err)
	}
	if queued.Status != job.StatusPending || queued.Apply {
		t.Errorf("queued = %+v", queued)
	}

	// One account, one piece of work: a backup and a restore would stage
	// on top of each other.
	if _, err := engine.QueueBackup("policy", "c1"); err == nil {
		t.Error("a backup should not start while a restore of that account is queued")
	}
}
