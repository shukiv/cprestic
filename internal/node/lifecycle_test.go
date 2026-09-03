package node

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/vault"
)

func lifecycleEngine(t *testing.T, users map[string][]string, uids map[string]int) (*Engine, *nodestore.Store) {
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
	done := time.Now().UTC()
	settings.IdentitiesBackfilledAt = &done
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	v, err := vault.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(Config{
		Store: store, Vault: v,
		Provider: &cpanel.Fake{Root: filepath.Join(root, "cpanel"), Databases: users},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		AccountUID: func(user string) (int, error) {
			return uids[user], nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine, store
}

func TestModifyHookCarriesNamedPolicyAcrossRename(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"newname": nil}, map[string]int{"newname": 1042})
	since := time.Now().Add(-24 * time.Hour).UTC()
	if _, err := store.PutIdentity(nodestore.AccountIdentity{
		Account: "oldname", UID: 1042, SinceAt: since,
	}); err != nil {
		t.Fatal(err)
	}
	policy, err := store.PutPolicy(nodestore.Policy{
		Name: "selected", Accounts: []string{"oldname"}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := engine.ReconcileAccountRenames(context.Background()); err != nil {
		t.Fatal(err)
	}
	policy, _ = store.Policy(policy.ID)
	if len(policy.Accounts) != 1 || policy.Accounts[0] != "newname" {
		t.Fatalf("policy accounts = %v, want renamed account", policy.Accounts)
	}
	identity, err := store.Identity("newname")
	if err != nil || identity.UID != 1042 || !identity.SinceAt.Equal(since) {
		t.Fatalf("renamed identity = %+v, %v", identity, err)
	}
}

func TestCreateReconciliationDoesNotMistakeReusedUIDForRename(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"newowner": nil}, map[string]int{"newowner": 1042})
	if _, err := store.PutIdentity(nodestore.AccountIdentity{
		Account: "departed", UID: 1042, SinceAt: time.Now().Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	policy, _ := store.PutPolicy(nodestore.Policy{
		Name: "selected", Accounts: []string{"departed"}, Enabled: true,
	})

	if err := engine.ReconcileAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	policy, _ = store.Policy(policy.ID)
	if policy.Accounts[0] != "departed" {
		t.Fatalf("a reused uid was treated as a rename: %v", policy.Accounts)
	}
	identity, err := store.Identity("newowner")
	if err != nil || !identity.Recycled {
		t.Fatalf("new owner has no privacy boundary: %+v, %v", identity, err)
	}
}

func TestCreateHookSeparatesReusedNameEvenWhenUIDWasAlsoReused(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"webshop": nil}, map[string]int{"webshop": 1042})
	oldSince := time.Now().Add(-30 * 24 * time.Hour).UTC()
	previous, err := store.PutIdentity(nodestore.AccountIdentity{
		Account: "webshop", UID: 1042, SinceAt: oldSince,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.AccountCreated("webshop"); err != nil {
		t.Fatal(err)
	}
	current, err := store.Identity("webshop")
	if err != nil {
		t.Fatal(err)
	}
	if !current.Recycled || !current.SinceAt.After(oldSince) {
		t.Fatalf("reused name and uid inherited the old boundary: %+v", current)
	}
	if !current.CreatedAt.Equal(previous.CreatedAt) {
		t.Fatal("identity history was replaced rather than advanced")
	}
}

func TestNewAccountGetsImmediateBackupFromWidestAllAccountPolicy(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"newsite": nil}, map[string]int{"newsite": 1500})
	_, _ = store.PutPolicy(nodestore.Policy{
		Name: "named only", Accounts: []string{"someoneelse"}, Enabled: true,
		RepositoryIDs: []string{"named-repo"},
	})
	_, _ = store.PutPolicy(nodestore.Policy{
		Name: "one copy", Enabled: true, RepositoryIDs: []string{"repo-1"},
	})
	widest, _ := store.PutPolicy(nodestore.Policy{
		Name: "two copies", Enabled: true,
		RepositoryIDs: []string{"repo-1", "repo-2"},
	})

	queued, err := engine.QueueInitialBackup("newsite")
	if err != nil || !queued {
		t.Fatalf("initial backup = %v, %v", queued, err)
	}
	jobs, err := store.Jobs(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].PolicyID != widest.ID || jobs[0].Account != "newsite" {
		t.Fatalf("initial jobs = %+v", jobs)
	}
}

func TestInitialBackupPrefersCompletePolicyOverWiderPartialPolicy(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"newsite": nil}, map[string]int{"newsite": 1500})
	_, _ = store.PutPolicy(nodestore.Policy{
		Name: "partial", Enabled: true, SkipEmail: true,
		RepositoryIDs: []string{"repo-1", "repo-2"},
	})
	complete, _ := store.PutPolicy(nodestore.Policy{
		Name: "complete", Enabled: true, RepositoryIDs: []string{"repo-1"},
	})
	queued, err := engine.QueueInitialBackup("newsite")
	if err != nil || !queued {
		t.Fatalf("initial backup = %v, %v", queued, err)
	}
	jobs, _ := store.Jobs(0)
	if len(jobs) != 1 || jobs[0].PolicyID != complete.ID {
		t.Fatalf("initial backup used a partial payload: %+v", jobs)
	}
}

func TestSuspensionBackupIsOptInAndCoversAllFullDestinations(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"customer1": nil}, map[string]int{"customer1": 1500})
	result, err := engine.QueueSuspensionBackup("customer1")
	if err != nil || result.Enabled || result.Queued {
		t.Fatalf("default suspension backup = %+v, %v", result, err)
	}
	settings, _ := store.Settings()
	settings.BackupOnSuspension = true
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	_, _ = store.PutPolicy(nodestore.Policy{
		Name: "partial", Enabled: true, SkipEmail: true,
		RepositoryIDs: []string{"repo-1", "repo-2"},
	})
	_, _ = store.PutPolicy(nodestore.Policy{
		Name: "local full", Enabled: true, RepositoryIDs: []string{"repo-1"},
	})
	_, _ = store.PutPolicy(nodestore.Policy{
		Name: "remote full", Enabled: true, Accounts: []string{"customer1"},
		RepositoryIDs: []string{"repo-2"},
	})

	result, err = engine.QueueSuspensionBackup("customer1")
	if err != nil || !result.Enabled || !result.Queued || len(result.Policies) != 2 {
		t.Fatalf("suspension backup = %+v, %v", result, err)
	}
	jobs, _ := store.Jobs(0)
	if len(jobs) != 2 {
		t.Fatalf("suspension jobs = %+v", jobs)
	}
	result, err = engine.QueueSuspensionBackup("customer1")
	if err != nil || !result.Busy || result.Queued {
		t.Fatalf("duplicate suspension backup = %+v, %v", result, err)
	}
}

func TestCoverageRepairRejectsPolicyThatDoesNotCoverAccount(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"customer1": nil}, map[string]int{"customer1": 1500})
	wrong, _ := store.PutPolicy(nodestore.Policy{
		Name: "someone else", Accounts: []string{"customer2"}, Enabled: true,
		RepositoryIDs: []string{"repo-1"},
	})
	if _, err := engine.QueueCoverageRepair(wrong.ID, "customer1"); err == nil {
		t.Fatal("unrelated policy was accepted for coverage repair")
	}
	right, _ := store.PutPolicy(nodestore.Policy{
		Name: "customer one", Accounts: []string{"customer1"}, Enabled: true,
		RepositoryIDs: []string{"repo-1"},
	})
	if _, err := engine.QueueCoverageRepair(right.ID, "customer1"); err != nil {
		t.Fatalf("covered policy was refused: %v", err)
	}
}

func TestRemovingOnlyNamedAccountDisablesRatherThanWidensPolicy(t *testing.T) {
	engine, store := lifecycleEngine(t, map[string][]string{}, map[string]int{})
	policy, _ := store.PutPolicy(nodestore.Policy{
		Name: "one customer", Accounts: []string{"departed"}, Enabled: true,
	})
	if err := engine.AccountRemoved("departed"); err != nil {
		t.Fatal(err)
	}
	policy, _ = store.Policy(policy.ID)
	if policy.Enabled || len(policy.Accounts) != 0 {
		t.Fatalf("terminated account widened policy: %+v", policy)
	}
}

// TestARemovedAccountIsNeverTreatedAsARenameSource covers the way the
// ownership boundary can be handed to the wrong customer.
//
// A rename is inferred from an old name that vanished while its uid stayed.
// Linux reuses a deleted account's uid, so a new account that reached the
// server without a create hook -- restored by restorepkg, or created while
// this service was stopped -- looks exactly like a rename of the departed
// account whose uid it inherited. Taking it as one hands the new customer
// the previous one's SinceAt, and their backups with it.
//
// cPanel told us that account was removed. A removal is not a rename.
func TestARemovedAccountIsNeverTreatedAsARenameSource(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"newowner": nil}, map[string]int{"newowner": 1042})

	departedSince := time.Now().Add(-90 * 24 * time.Hour).UTC()
	retired := time.Now().Add(-24 * time.Hour).UTC()
	if _, err := store.PutIdentity(nodestore.AccountIdentity{
		Account: "departed", UID: 1042, SinceAt: departedSince, RetiredAt: &retired,
	}); err != nil {
		t.Fatal(err)
	}
	policy, err := store.PutPolicy(nodestore.Policy{
		Name: "selected", Accounts: []string{"departed"}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The modify hook is the one path allowed to infer a rename at all.
	if err := engine.ReconcileAccountRenames(context.Background()); err != nil {
		t.Fatal(err)
	}

	identity, err := store.Identity("newowner")
	if err != nil {
		t.Fatalf("the new account has no identity at all: %v", err)
	}
	if !identity.SinceAt.After(departedSince) {
		t.Fatalf("the new owner inherited the departed customer's boundary: %+v", identity)
	}
	if !identity.Recycled {
		t.Fatal("the new owner was recorded without a privacy boundary")
	}
	policy, _ = store.Policy(policy.ID)
	if len(policy.Accounts) != 1 || policy.Accounts[0] != "departed" {
		t.Fatalf("a removal was carried across as a rename: %v", policy.Accounts)
	}
}
