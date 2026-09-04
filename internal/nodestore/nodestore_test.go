package nodestore_test

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/nodestore"
)

func newStore(t *testing.T) *nodestore.Store {
	t.Helper()
	store, err := nodestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestDefaultSettingsMatchFleetMode(t *testing.T) {
	// Snapshot paths embed the staging root and restic groups retention by
	// path, so a standalone server that later joins a fleet must have been
	// using the fleet default all along.
	settings := nodestore.DefaultSettings()
	if settings.StagingRoot != "/var/lib/cprest/staging" {
		t.Errorf("staging root = %q, does not match fleet mode", settings.StagingRoot)
	}
	if settings.MaxConcurrent != 1 {
		t.Errorf("max concurrent = %d", settings.MaxConcurrent)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	store := newStore(t)

	settings, err := store.Settings()
	if err != nil {
		t.Fatalf("Settings on a fresh store: %v", err)
	}
	if settings.StagingRoot == "" {
		t.Fatal("a fresh store should return defaults, not zero values")
	}

	settings.Hostname = "cp01.example.com"
	settings.MaxConcurrent = 3
	if err := store.SaveSettings(settings); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	reloaded, err := store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Hostname != "cp01.example.com" || reloaded.MaxConcurrent != 3 {
		t.Errorf("reloaded = %+v", reloaded)
	}
}

func TestRepositoriesCopyTheFirstChunkerSource(t *testing.T) {
	store := newStore(t)

	add := func(path string) nodestore.Repository {
		t.Helper()
		repo, err := store.PutRepository(nodestore.Repository{
			DestinationID: "dest-" + path, Path: path, PasswordSecretID: "secret",
		})
		if err != nil {
			t.Fatalf("PutRepository: %v", err)
		}
		// bbolt keys are ordered, but chunker selection is by creation
		// time; keep them distinct.
		time.Sleep(time.Millisecond)
		return repo
	}

	first := add("a")
	second := add("b")
	third := add("c")

	if first.ChunkerSourceRepoID != "" {
		t.Errorf("the first repository has a chunker source: %q", first.ChunkerSourceRepoID)
	}
	// Chunker parameters are fixed at creation and cannot be changed, so
	// every later repository copies the first one's — and the third
	// follows the chain to its root rather than pointing at the second.
	if second.ChunkerSourceRepoID != first.ID {
		t.Errorf("second source = %q, want %q", second.ChunkerSourceRepoID, first.ID)
	}
	if third.ChunkerSourceRepoID != first.ID {
		t.Errorf("third source = %q, want %q", third.ChunkerSourceRepoID, first.ID)
	}
}

func TestDeleteDestinationTakesItsRepositoryWithIt(t *testing.T) {
	store := newStore(t)

	dest, err := store.PutDestination(nodestore.Destination{Name: "Backup disk", Type: "local"})
	if err != nil {
		t.Fatalf("PutDestination: %v", err)
	}
	repo, err := store.PutRepository(nodestore.Repository{
		DestinationID: dest.ID, Path: "cp01", PasswordSecretID: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A destination typed in wrongly has to be removable, or the operator
	// is stuck with it.
	if err := store.DeleteDestination(dest.ID); err != nil {
		t.Fatalf("DeleteDestination: %v", err)
	}
	if _, err := store.Repository(repo.ID); !errors.Is(err, nodestore.ErrNotFound) {
		t.Error("the repository record outlived its destination")
	}
}

func TestDeleteDestinationRefusesWhileAScheduleUsesIt(t *testing.T) {
	store := newStore(t)

	dest, err := store.PutDestination(nodestore.Destination{Name: "Backup disk", Type: "local"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := store.PutRepository(nodestore.Repository{
		DestinationID: dest.ID, Path: "cp01", PasswordSecretID: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutPolicy(nodestore.Policy{
		Name: "Nightly", ScheduleCron: "0 2 * * *", RepositoryIDs: []string{repo.ID},
	}); err != nil {
		t.Fatal(err)
	}

	// Otherwise the schedule would quietly stop making one of the copies
	// it promises.
	err = store.DeleteDestination(dest.ID)
	if err == nil {
		t.Fatal("a destination a schedule still uses should not be removable")
	}
	if !strings.Contains(err.Error(), "Nightly") {
		t.Errorf("err = %v, want it to name the schedule", err)
	}
}

func TestRunningJobForCoversBackupsAndRestores(t *testing.T) {
	store := newStore(t)

	running, err := store.RunningJobFor("customer1")
	if err != nil || running {
		t.Fatalf("clean store reported running=%v (%v)", running, err)
	}

	stored, err := store.PutJob(nodestore.Job{Account: "customer1", Status: job.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	if running, _ := store.RunningJobFor("customer1"); !running {
		t.Error("a running backup was not reported")
	}
	if running, _ := store.RunningJobFor("customer2"); running {
		t.Error("another account was reported as busy")
	}

	stored.Status = job.StatusSuccess
	if _, err := store.PutJob(stored); err != nil {
		t.Fatal(err)
	}

	// A restore blocks a backup of the same account too: both stage in the
	// same place.
	if _, err := store.PutRestore(nodestore.Restore{
		Account: "customer1", Status: job.StatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if running, _ := store.RunningJobFor("customer1"); !running {
		t.Error("a running restore was not reported")
	}
}

func TestPendingWorkPrefersRestores(t *testing.T) {
	store := newStore(t)

	if _, err := store.PutJob(nodestore.Job{Account: "c1", Status: job.StatusPending}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutRestore(nodestore.Restore{
		Account: "c2", SnapshotID: "abc", Status: job.StatusPending,
	}); err != nil {
		t.Fatal(err)
	}

	// Someone is usually waiting for a restore; a backup can start a
	// minute later without anyone noticing.
	restore, backup, err := store.PendingWork()
	if err != nil {
		t.Fatalf("PendingWork: %v", err)
	}
	if restore == nil || backup != nil {
		t.Fatalf("got restore=%v backup=%v, want the restore first", restore, backup)
	}
}

func TestMissingRecordsReportNotFound(t *testing.T) {
	store := newStore(t)
	for name, err := range map[string]error{
		"destination": errOf(func() error { _, err := store.Destination("nope"); return err }),
		"repository":  errOf(func() error { _, err := store.Repository("nope"); return err }),
		"policy":      errOf(func() error { _, err := store.Policy("nope"); return err }),
		"job":         errOf(func() error { _, err := store.Job("nope"); return err }),
		"restore":     errOf(func() error { _, err := store.Restore("nope"); return err }),
		"secret":      errOf(func() error { _, err := store.Secret("nope"); return err }),
	} {
		if !errors.Is(err, nodestore.ErrNotFound) {
			t.Errorf("missing %s gave %v, want ErrNotFound", name, err)
		}
	}
}

func errOf(fn func() error) error { return fn() }

// PutJobs spaces a batch a nanosecond apart, so a nightly run's rows already
// have a fixed order. This covers the other way jobs arrive: a caller that
// supplies its own timestamp, where sorting on the timestamp alone would
// leave the order to bbolt's iteration and let the page move under whoever
// is watching it.
func TestJobsQueuedTogetherKeepAFixedOrder(t *testing.T) {
	store, err := nodestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	queued := time.Now().UTC().Truncate(time.Second)
	for _, account := range []string{"studio", "arkady", "rtflow", "cloud"} {
		if _, err := store.PutJob(nodestore.Job{
			Account: account, Status: job.StatusSuccess, QueuedAt: queued,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Something newer, which must still come first.
	if _, err := store.PutJob(nodestore.Job{
		Account: "later", Status: job.StatusSuccess, QueuedAt: queued.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	var first []string
	for run := 0; run < 5; run++ {
		jobs, err := store.Jobs(0)
		if err != nil {
			t.Fatal(err)
		}
		order := make([]string, 0, len(jobs))
		for _, one := range jobs {
			order = append(order, one.Account)
		}
		if run == 0 {
			first = order
			if order[0] != "later" {
				t.Errorf("newest is not first: %v", order)
			}
			continue
		}
		if !slices.Equal(order, first) {
			t.Errorf("the order moved between reads: %v then %v", first, order)
		}
	}
}

func TestJobsAndRestoresSurviveAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	store, err := nodestore.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	stored, err := store.PutJob(nodestore.Job{Account: "customer1", Status: job.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// A job left running is exactly what a crash leaves behind; the engine
	// has to be able to find it again to close it out.
	reopened, err := nodestore.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	found, err := reopened.Job(stored.ID)
	if err != nil {
		t.Fatalf("Job after reopen: %v", err)
	}
	if found.Status != job.StatusRunning || found.Account != "customer1" {
		t.Errorf("job = %+v", found)
	}
}

func TestLifecycleHistoryIsNewestFirstAndBounded(t *testing.T) {
	store := newStore(t)
	start := time.Now().Add(-time.Hour).UTC()
	for i := 0; i < 105; i++ {
		if _, err := store.PutLifecycleEvent(nodestore.LifecycleEvent{
			Event: "create", Account: "customer1", OK: true,
			At: start.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := store.LifecycleEvents(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 100 {
		t.Fatalf("retained %d lifecycle events, want 100", len(events))
	}
	if !events[0].At.Equal(start.Add(104*time.Second)) ||
		!events[len(events)-1].At.Equal(start.Add(5*time.Second)) {
		t.Fatalf("lifecycle ordering/retention is wrong: first=%v last=%v",
			events[0].At, events[len(events)-1].At)
	}
}
