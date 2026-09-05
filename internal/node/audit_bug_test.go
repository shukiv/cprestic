package node_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/node"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/reassemble"
	"github.com/shuki/cprest/internal/resticrun"
)

func TestAuditWholeAccountRestoreChoosesNewerDatabaseExcludedSnapshot(t *testing.T) {
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

	now := time.Now().UTC()
	exec := resticrun.ExecFunc(func(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
		return resticrun.CommandResult{Stdout: []byte(`[
			{"id":"full-old","time":"` + now.Add(-2*time.Hour).Format(time.RFC3339Nano) + `","tags":["account:customer1","mode:split"],"paths":["/stage/metadata","/home/customer1","/stage/databases"]},
			{"id":"partial-new","time":"` + now.Add(-time.Hour).Format(time.RFC3339Nano) + `","tags":["account:customer1","mode:split"],"paths":["/stage/metadata","/home/customer1"]}
		]`)}, nil
	})
	engine := newEngineWithExec(t, store, root, exec)
	_, repo, err := engine.AddDestination(nodestore.Destination{
		Name: "Local", Type: "local", Config: map[string]string{"root": t.TempDir()},
	}, nil, "backups")
	if err != nil {
		t.Fatal(err)
	}

	chosen, err := engine.SnapshotAsOf(context.Background(), repo.ID, "customer1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if chosen != "partial-new" {
		t.Fatalf("chosen = %q, want current vulnerable behavior partial-new", chosen)
	}
	parts, err := reassemble.Classify([]string{"/stage/metadata", "/home/customer1"})
	if err != nil {
		t.Fatalf("database-excluded snapshot is rejected before apply: %v", err)
	}
	if parts.Databases != "" {
		t.Fatalf("database-excluded snapshot unexpectedly contains databases: %+v", parts)
	}
	_ = node.Contents{}
}
