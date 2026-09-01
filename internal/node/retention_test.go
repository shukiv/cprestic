package node_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/resticrun"
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

// TestARetentionFailureIsReportedAndBacksOff covers what happens when
// restic will not run — a stale lock is the everyday case, and one turned
// up on the live server during development.
//
// Two things must hold, and neither did. The error has to reach the
// caller: recording it and returning nil told an operator who had just
// hit a locked repository that there was nothing to remove. And the
// attempt has to count towards the throttle, or the sweep retries the
// same locked repository on every fifteen-second tick, silently, for
// ever.
func TestARetentionFailureIsReportedAndBacksOff(t *testing.T) {
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

	// A restic that answers the way a locked repository does.
	locked := resticrun.ExecFunc(func(context.Context, resticrun.Command) (resticrun.CommandResult, error) {
		return resticrun.CommandResult{
			ExitCode: 11,
			Stdout: []byte(`{"message_type":"exit_error","code":11,` +
				`"message":"unable to create lock in backend: repository is already locked"}`),
		}, nil
	})
	engine := newEngineWithExec(t, store, root, locked)

	repo := attachedRepository(t, store, engine)
	if _, err := store.PutPolicy(nodestore.Policy{
		Name: "Nightly", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{repo.ID},
		Retention:     nodestore.Retention{KeepDaily: 7},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.PlanRetention(context.Background(), repo.ID); err == nil {
		t.Fatal("a locked repository was reported as having nothing to remove")
	}

	after, err := store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Retention.LastError == "" {
		t.Error("the reason was not recorded where the page can show it")
	}
	if after.Retention.AttemptedAt == nil {
		t.Fatal("a failed attempt left no timestamp, so the sweep would retry it every tick")
	}
	if !engine.RetentionIsThrottledForTest(after) {
		t.Error("a repository that has just failed is due again immediately")
	}
}
