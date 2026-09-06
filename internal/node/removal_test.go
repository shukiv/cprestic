package node

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
)

func enableRemovalProtection(t *testing.T, store *nodestore.Store) {
	t.Helper()
	settings, err := store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.ProtectAccountRemoval = true
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
}

func TestAccountRemovalProtectionIsOptIn(t *testing.T) {
	engine, _ := lifecycleEngine(t, map[string][]string{}, map[string]int{})
	decision, err := engine.AccountRemovalSafety("customer1", time.Now())
	if err != nil || !decision.Allowed || decision.Enforced {
		t.Fatalf("default removal decision = %+v, %v", decision, err)
	}
}

func TestAccountRemovalSafetiesEvaluateManyAccounts(t *testing.T) {
	engine, store := lifecycleEngine(t, map[string][]string{}, map[string]int{})
	enableRemovalProtection(t, store)
	policy, _ := store.PutPolicy(nodestore.Policy{
		Name: "daily full", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{"repository-1"},
	})
	finished := time.Now().UTC().Add(-time.Hour)
	_, _ = store.PutJob(nodestore.Job{
		PolicyID: policy.ID, Account: "protected", CompleteAccount: true,
		Status: job.StatusSuccess, FinishedAt: &finished,
		Targets: []nodestore.JobTarget{{RepositoryID: "repository-1", Status: job.TargetSuccess}},
	})

	decisions, err := engine.AccountRemovalSafeties([]string{"protected", "missing"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !decisions["protected"].Enforced || !decisions["protected"].Allowed {
		t.Fatalf("protected = %+v", decisions["protected"])
	}
	if !decisions["missing"].Enforced || decisions["missing"].Allowed {
		t.Fatalf("missing = %+v", decisions["missing"])
	}
}

func TestAccountRemovalRequiresEveryPromisedCompleteCopy(t *testing.T) {
	engine, store := lifecycleEngine(t, map[string][]string{}, map[string]int{})
	enableRemovalProtection(t, store)
	policy, err := store.PutPolicy(nodestore.Policy{
		Name: "daily full", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{"local-copy", "remote-copy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	finished := now.Add(-time.Hour)
	_, err = store.PutJob(nodestore.Job{
		PolicyID: policy.ID, Account: "customer1", CompleteAccount: true,
		Status: job.StatusSuccess, FinishedAt: &finished,
		Targets: []nodestore.JobTarget{
			{RepositoryID: "local-copy", Status: job.TargetSuccess},
			{RepositoryID: "remote-copy", Status: job.TargetSuccess, Incomplete: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	decision, err := engine.AccountRemovalSafety("customer1", now)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Allowed || !strings.Contains(decision.Detail, "remote-c") {
		t.Fatalf("incomplete promised copy allowed removal: %+v", decision)
	}

	stored, _ := store.Jobs(0)
	stored[0].Targets[1].Incomplete = false
	if _, err := store.PutJob(stored[0]); err != nil {
		t.Fatal(err)
	}
	decision, err = engine.AccountRemovalSafety("customer1", now)
	if err != nil || !decision.Allowed {
		t.Fatalf("complete copies did not allow removal: %+v, %v", decision, err)
	}
}

func TestAccountRemovalRejectsStaleAndLegacyJobs(t *testing.T) {
	engine, store := lifecycleEngine(t, map[string][]string{}, map[string]int{})
	enableRemovalProtection(t, store)
	policy, _ := store.PutPolicy(nodestore.Policy{
		Name: "daily full", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{"repository-1"},
	})
	now := time.Now().UTC()
	old := now.Add(-72 * time.Hour) // daily schedules are due after two intervals
	legacy, err := store.PutJob(nodestore.Job{
		PolicyID: policy.ID, Account: "customer1", Status: job.StatusSuccess,
		FinishedAt: &old,
		Targets:    []nodestore.JobTarget{{RepositoryID: "repository-1", Status: job.TargetSuccess}},
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := engine.AccountRemovalSafety("customer1", now)
	if err != nil || decision.Allowed {
		t.Fatalf("legacy job authorized removal: %+v, %v", decision, err)
	}

	legacy.CompleteAccount = true
	if _, err := store.PutJob(legacy); err != nil {
		t.Fatal(err)
	}
	decision, err = engine.AccountRemovalSafety("customer1", now)
	if err != nil || decision.Allowed || !strings.Contains(decision.Detail, "stale") {
		t.Fatalf("stale job authorized removal: %+v, %v", decision, err)
	}
}

func TestAccountRemovalRejectsPartialPolicyAndUnnamedHook(t *testing.T) {
	engine, store := lifecycleEngine(t, map[string][]string{}, map[string]int{})
	enableRemovalProtection(t, store)
	_, _ = store.PutPolicy(nodestore.Policy{
		Name: "files only", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{"repository-1"}, SkipEmail: true,
	})
	decision, err := engine.AccountRemovalSafety("customer1", time.Now())
	if err != nil || decision.Allowed || !strings.Contains(decision.Detail, "full-account") {
		t.Fatalf("partial policy decision = %+v, %v", decision, err)
	}
	decision, err = engine.AccountRemovalSafety("", time.Now())
	if err != nil || decision.Allowed || !strings.Contains(decision.Detail, "did not name") {
		t.Fatalf("unnamed hook decision = %+v, %v", decision, err)
	}
}

func TestMinimumPolicyCoverFindsSmallerPlanThanGreedy(t *testing.T) {
	policies := []nodestore.Policy{
		{ID: "wide", RepositoryIDs: []string{"1", "2", "3", "4"}},
		{ID: "left", RepositoryIDs: []string{"1", "2", "5"}},
		{ID: "right", RepositoryIDs: []string{"3", "4", "6"}},
	}
	selected := minimumPolicyCover([]string{"1", "2", "3", "4", "5", "6"}, policies)
	ids := map[string]bool{}
	for _, policy := range selected {
		ids[policy.ID] = true
	}
	if len(selected) != 2 || !ids["left"] || !ids["right"] {
		t.Fatalf("minimum cover = %+v", selected)
	}
}

func TestQueueRemovalPreparationStoresSequentialPolicyPlan(t *testing.T) {
	engine, store := lifecycleEngine(t, map[string][]string{}, map[string]int{})
	enableRemovalProtection(t, store)
	for _, policy := range []nodestore.Policy{
		{ID: "wide", Name: "wide", ScheduleCron: "0 2 * * *", Enabled: true,
			RepositoryIDs: []string{"1", "2", "3", "4"}},
		{ID: "left", Name: "left", ScheduleCron: "0 2 * * *", Enabled: true,
			RepositoryIDs: []string{"1", "2", "5"}},
		{ID: "right", Name: "right", ScheduleCron: "0 2 * * *", Enabled: true,
			RepositoryIDs: []string{"3", "4", "6"}},
	} {
		if _, err := store.PutPolicy(policy); err != nil {
			t.Fatal(err)
		}
	}
	selected, err := engine.QueueRemovalPreparation("customer1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 {
		t.Fatalf("queued policies = %+v", selected)
	}
	jobs, err := store.Jobs(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].Status != job.StatusPending || jobs[1].Status != job.StatusPending {
		t.Fatalf("queued jobs = %+v", jobs)
	}
	if !jobs[0].QueuedAt.After(jobs[1].QueuedAt) {
		t.Fatalf("jobs were not given a stable sequence: %+v", jobs)
	}
	if _, err := engine.QueueRemovalPreparation("customer1", time.Now()); err == nil {
		t.Fatal("a second preparation plan was queued over pending work")
	}
}

func TestConcurrentQueueRequestsCannotOverlapOneAccount(t *testing.T) {
	engine, _ := lifecycleEngine(t, map[string][]string{}, map[string]int{})
	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			_, err := engine.QueueBackup("policy", "customer1")
			results <- err
		}()
	}
	ready.Wait()
	close(start)
	succeeded := 0
	for range 2 {
		if <-results == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("successful concurrent queues = %d, want exactly one", succeeded)
	}
}
