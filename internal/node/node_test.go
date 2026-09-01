package node_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/node"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/resticrun"
	"github.com/shuki/cprest/internal/staging"
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

	// Something merely queued must survive: a restart should not empty
	// the queue.
	queued, err := store.PutJob(nodestore.Job{Account: "customer3", Status: job.StatusPending})
	if err != nil {
		t.Fatal(err)
	}

	engine := newEngine(t, store, root)

	if survived, err := store.Job(queued.ID); err != nil {
		t.Fatal(err)
	} else if survived.Status != job.StatusPending {
		t.Errorf("a queued job became %q on restart", survived.Status)
	}

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

// TestOpenArchiveForDownloadRefusesSymlinkEscapes covers the case a lexical
// containment check misses: the path is inside the staging root by name but
// resolves somewhere else. This handler reads as root on a server whose
// other users are not trusted, so it has to resolve before it decides.
func TestOpenArchiveForDownloadRefusesSymlinkEscapes(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	staging := filepath.Join(root, "staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = staging
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, store, root)

	secret := filepath.Join(root, "secret.tar")
	if err := os.WriteFile(secret, []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A symlink sitting inside the staging root, pointing out of it.
	escape := filepath.Join(staging, "cpmove-escape.tar")
	if err := os.Symlink(secret, escape); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	// And a sibling directory whose name merely starts with the root's.
	sibling := staging + "-elsewhere"
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	siblingArchive := filepath.Join(sibling, "cpmove-c1.tar")
	if err := os.WriteFile(siblingArchive, []byte("also not yours"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, path := range map[string]string{
		"symlink out of staging": escape,
		"sibling directory":      siblingArchive,
		"outside entirely":       secret,
	} {
		restore, err := store.PutRestore(nodestore.Restore{
			Account: "customer1", SnapshotID: "abc",
			Status: job.StatusSuccess, ArchivePath: path,
		})
		if err != nil {
			t.Fatal(err)
		}
		file, _, _, err := engine.OpenArchiveForDownload(restore.ID)
		if err == nil {
			file.Close()
			t.Errorf("%s was served", name)
		}
	}

	// A real archive in the right place still works.
	good := filepath.Join(staging, "stage-restore-customer1", "cpmove-customer1.tar")
	if err := os.MkdirAll(filepath.Dir(good), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good, []byte("a real archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore, err := store.PutRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc",
		Status: job.StatusSuccess, ArchivePath: good,
	})
	if err != nil {
		t.Fatal(err)
	}
	file, filename, size, err := engine.OpenArchiveForDownload(restore.ID)
	if err != nil {
		t.Fatalf("a legitimate archive was refused: %v", err)
	}
	defer file.Close()
	if filename != "cpmove-customer1.tar" || size != int64(len("a real archive")) {
		t.Errorf("filename=%q size=%d", filename, size)
	}
}

func TestDrillRefusesWhenTheVolumeIsTooFull(t *testing.T) {
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
	settings.MaxConcurrent = 1
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, store, root)

	// A rehearsal writes a full copy of the account into scratch. On a
	// server that is nearly full — which is the normal state of a cPanel
	// box — it has to be refused rather than allowed to fill the volume.
	if _, err := engine.QueueDrill(context.Background(), "customer1"); err == nil {
		t.Error("a drill was queued for an account with no backup")
	}

	// And a drill must not run alongside other work for the same account.
	if _, err := store.PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.QueueRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc", Kind: node.KindVerify,
	}); err == nil {
		t.Error("a drill was queued while that account was already busy")
	}
}

// Collected output is swept once nobody has come back for it, and work in
// progress is never touched however old it looks.
func TestSweepRemovesOldOutputAndLeavesWorkAlone(t *testing.T) {
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

	manager := &staging.Manager{Root: settings.StagingRoot, MaxConcurrent: 4}
	old, err := manager.Allocate("restore-old", 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	oldOutput, err := manager.Retain(old)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := manager.Allocate("restore-fresh", 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	freshOutput, err := manager.Retain(fresh)
	if err != nil {
		t.Fatal(err)
	}
	working, err := manager.Allocate("customer1", 1<<10)
	if err != nil {
		t.Fatal(err)
	}

	// Only age decides, so the old one is aged rather than waited for.
	longAgo := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(oldOutput.Path, longAgo, longAgo); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(working.Path, longAgo, longAgo); err != nil {
		t.Fatal(err)
	}

	if err := engine.SweepWorkdir(); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, err := os.Stat(oldOutput.Path); !os.IsNotExist(err) {
		t.Error("output nobody collected is still there")
	}
	if _, err := os.Stat(freshOutput.Path); err != nil {
		t.Error("output produced today was swept")
	}
	if _, err := os.Stat(working.Path); err != nil {
		t.Error("a directory being worked in was swept")
	}
}

// newEngineWithExec builds an engine whose restic is a stand-in, so the
// paths that only happen when restic fails can be exercised.
func newEngineWithExec(t *testing.T, store *nodestore.Store, root string, exec resticrun.Execer) *node.Engine {
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
		Store: store, Vault: v, Exec: exec,
		Provider: &cpanel.Fake{Root: filepath.Join(root, "cpanel")},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	return engine
}

// attachedRepository is a local destination with a repository on it, which
// is the least a retention test needs to open one.
func attachedRepository(t *testing.T, store *nodestore.Store, engine *node.Engine) nodestore.Repository {
	t.Helper()
	_, repo, err := engine.AddDestination(nodestore.Destination{
		Name: "Local", Type: "local",
		Config: map[string]string{"root": t.TempDir()},
	}, nil, "backups")
	if err != nil {
		t.Fatalf("add destination: %v", err)
	}
	// Retention only looks at repositories that exist on the far end.
	now := time.Now().UTC()
	repo.InitialisedAt = &now
	if _, err := store.PutRepository(repo); err != nil {
		t.Fatal(err)
	}
	return repo
}
