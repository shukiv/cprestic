package node_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/nodestore"
)

// sink collects the notifications a test produced.
type sink struct {
	*httptest.Server
	mu   sync.Mutex
	sent []map[string]any
}

func newSink(t *testing.T) *sink {
	t.Helper()
	s := &sink{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.sent = append(s.sent, body)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *sink) events(name string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found []map[string]any
	for _, message := range s.sent {
		if message["event"] == name {
			found = append(found, message)
		}
	}
	return found
}

// TestAnAccountThatStoppedBeingBackedUpIsReported covers the one failure
// this program cannot report from a run, because there is no run: backups
// of an account quietly stop happening. Nothing fails, nothing is logged,
// and the operator finds out when they need a restore.
//
// It also covers the other half of that promise — that it is said once.
// A server whose backups have been broken for a week must not send a week
// of identical messages, or the next real one is lost among them.
func TestAnAccountThatStoppedBeingBackedUpIsReported(t *testing.T) {
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
	settings.Hostname = "test.example.com"
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}

	engine := newEngine(t, store, root)
	collected := newSink(t)
	if _, err := engine.SaveChannel(nodestore.Channel{
		Name:    "Sink",
		Kind:    "webhook",
		Config:  map[string]string{"url": collected.URL},
		Enabled: true,
	}, nil); err != nil {
		t.Fatal(err)
	}

	// A nightly schedule over two accounts, one of which was backed up
	// this morning and one of which has not been backed up for a month.
	if _, err := store.PutPolicy(nodestore.Policy{
		Name:          "Nightly",
		ScheduleCron:  "0 2 * * *",
		RepositoryIDs: []string{"repo-1"},
		Accounts:      []string{"recent", "forgotten"},
		Enabled:       true,
	}); err != nil {
		t.Fatal(err)
	}
	// PutJob stamps QueuedAt on a job it is giving an id to, so these are
	// stored and then dated.
	now := time.Now().UTC()
	backedUpAt(t, store, "recent", now.Add(-6*time.Hour))
	backedUpAt(t, store, "forgotten", now.Add(-30*24*time.Hour))

	ctx := context.Background()
	if _, err := engine.Schedule(ctx, now); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	overdue := collected.events("overdue")
	if len(overdue) != 1 {
		t.Fatalf("want one overdue message, got %d: %v", len(overdue), collected.sent)
	}
	if overdue[0]["account"] != "forgotten" {
		t.Fatalf("the wrong account was reported: %v", overdue[0])
	}
	if overdue[0]["host"] != "test.example.com" {
		t.Fatalf("the message does not say which server it is about: %v", overdue[0])
	}

	// Said once. The watcher is throttled, so move time on past that as
	// well as running it again.
	if _, err := engine.Schedule(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if again := collected.events("overdue"); len(again) != 1 {
		t.Fatalf("the same problem was reported %d times", len(again))
	}
}

// backedUpAt records a successful backup of an account at a given time.
func backedUpAt(t *testing.T, store *nodestore.Store, account string, when time.Time) {
	t.Helper()
	stored, err := store.PutJob(nodestore.Job{Account: account, Status: job.StatusSuccess})
	if err != nil {
		t.Fatal(err)
	}
	stored.QueuedAt = when
	if _, err := store.PutJob(stored); err != nil {
		t.Fatal(err)
	}
}

// TestARunThatNeverFinishesIsReported covers the other silent failure: a
// job wedged in "running" holds its account's staging space and blocks
// every later backup of it, and nothing else ever says so.
func TestARunThatNeverFinishesIsReported(t *testing.T) {
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
	collected := newSink(t)
	if _, err := engine.SaveChannel(nodestore.Channel{
		Name:    "Sink",
		Kind:    "webhook",
		Config:  map[string]string{"url": collected.URL},
		Enabled: true,
	}, nil); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	started := now.Add(-9 * time.Hour)
	if _, err := store.PutJob(nodestore.Job{
		Account: "wedged", Status: job.StatusRunning,
		QueuedAt: started, StartedAt: &started,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.Schedule(context.Background(), now); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	stuck := collected.events("stuck")
	if len(stuck) != 1 {
		t.Fatalf("want one stuck message, got %d: %v", len(stuck), collected.sent)
	}
	if stuck[0]["account"] != "wedged" {
		t.Fatalf("the wrong account was reported: %v", stuck[0])
	}
	if stuck[0]["severity"] != "error" {
		t.Fatalf("a wedged run is not a passing remark: %v", stuck[0])
	}
}
