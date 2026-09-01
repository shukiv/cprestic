package node_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/nodestore"
)

// TestRetentionRefusesToRunWithoutApproval is the gate the operator asked
// for: nothing is deleted from a repository until somebody has looked at
// a plan and said go.
func TestRetentionRefusesToRunWithoutApproval(t *testing.T) {
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

	repo, err := store.PutRepository(nodestore.Repository{Path: "repo", DestinationID: "d1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutPolicy(nodestore.Policy{
		Name: "Nightly", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{repo.ID},
		Retention:     nodestore.Retention{KeepDaily: 7},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.ApplyRetention(context.Background(), repo.ID); err == nil {
		t.Fatal("backups were deleted from a repository nobody had approved")
	}

	// And approving is itself refused until there is a plan to approve:
	// an operator cannot agree to something they have not been shown.
	if err := engine.ApproveRetention(repo.ID); err == nil {
		t.Fatal("retention was approved for a repository with no plan")
	}
}

// TestAKeepPolicyOfNothingIsRefused covers the argument list that would
// empty a repository. restic deletes every snapshot when told to keep
// none, so a schedule that says nothing must not be read as "keep
// nothing" — and must not quietly get a default nobody chose either.
func TestAKeepPolicyOfNothingIsRefused(t *testing.T) {
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

	repo, err := store.PutRepository(nodestore.Repository{Path: "repo", DestinationID: "d1"})
	if err != nil {
		t.Fatal(err)
	}
	// A schedule that writes here but keeps nothing, and one that keeps
	// something but writes elsewhere.
	if _, err := store.PutPolicy(nodestore.Policy{
		Name: "No keeps", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{repo.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutPolicy(nodestore.Policy{
		Name: "Elsewhere", ScheduleCron: "0 3 * * *", Enabled: true,
		RepositoryIDs: []string{"other"},
		Retention:     nodestore.Retention{KeepDaily: 7},
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	repo.RetentionApprovedAt = &now
	if _, err := store.PutRepository(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ApplyRetention(context.Background(), repo.ID); err == nil {
		t.Fatal("a repository with no keep policy was handed to restic forget")
	}
}
