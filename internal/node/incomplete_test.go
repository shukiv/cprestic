package node_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/resticrun"
)

func TestLegacyIncompleteBackupCannotBeSelectedForRecovery(t *testing.T) {
	engine, repo := recoveryFixture(t, `[{"id":"aaaaaaaaaaaaaaaa","time":"2026-09-06T01:00:00Z","tags":["account:customer1"]}]`)
	if _, err := engine.Store().PutJob(nodestore.Job{Account: "customer1", Status: job.StatusSuccess,
		Targets: []nodestore.JobTarget{{RepositoryID: repo.ID, SnapshotID: "aaaaaaaaaaaaaaaa", Status: job.TargetSuccess, Incomplete: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.SnapshotAsOf(t.Context(), repo.ID, "customer1", time.Time{}); err == nil {
		t.Fatal("legacy incomplete backup was selected for full recovery")
	}
	if _, _, err := engine.Drill(t.Context(), repo.ID, "customer1"); err == nil || !strings.Contains(err.Error(), "unverified") {
		t.Fatalf("legacy incomplete backup was rehearsed as a full backup: %v", err)
	}
}

func TestDirectRetentionProtectsLegacyIncompleteBackups(t *testing.T) {
	root := t.TempDir()
	db, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	settings := nodestore.DefaultSettings()
	settings.StagingRoot, settings.ResticCache, settings.ConfigDir = filepath.Join(root, "staging"), filepath.Join(root, "cache"), filepath.Join(root, "config")
	if err := db.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	executor := resticrun.ExecFunc(func(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
		if cmd.Args[0] == "forget" && !strings.Contains(strings.Join(cmd.Args, " "), "--dry-run") {
			t.Fatalf("legacy incomplete backup caused deletion: %v", cmd.Args)
		}
		return resticrun.CommandResult{Stdout: []byte(`[{"tags":["account:customer1"],"keep":[{"id":"aaaaaaaaaaaaaaaa"}],"remove":[{"id":"bbbbbbbbbbbbbbbb"}]}]`)}, nil
	})
	engine := newEngineWithExec(t, db, root, executor)
	repo := attachedRepository(t, db, engine)
	if _, err := db.PutJob(nodestore.Job{Account: "customer1", Status: job.StatusSuccess,
		Targets: []nodestore.JobTarget{{RepositoryID: repo.ID, SnapshotID: "aaaaaaaaaaaaaaaa", Incomplete: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Forget(t.Context(), repo.ID, nodestore.Retention{KeepLast: 1}, false); err != nil {
		t.Fatal(err)
	}
}
