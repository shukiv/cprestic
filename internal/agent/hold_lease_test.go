package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/protocol"
)

// A lease is a fixed span; the work is not. A restore of a large account
// over a slow link outlasts it, and the job is then given to another
// attempt while this one is still writing into a live account. So the
// agent says it is still working, and keeps the claim.
func TestWorkThatOutlivesItsLeaseKeepsIt(t *testing.T) {
	var renewals atomic.Int64
	var sawRestore atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != protocol.PathRenewLease {
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req protocol.LeaseRenewal
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.JobID != "restore-1" || req.ClaimToken != "token-1" {
			t.Errorf("renewal names %+v", req)
		}
		sawRestore.Store(req.Restore)
		renewals.Add(1)
		_ = json.NewEncoder(w).Encode(protocol.LeaseRenewed{
			LeaseExpiresAt: time.Now().Add(time.Hour)})
	}))
	defer server.Close()

	worker := &Agent{
		client:          NewClientWithHTTP(server.URL, server.Client()),
		log:             slog.New(slog.DiscardHandler),
		LeaseRenewEvery: 10 * time.Millisecond,
	}
	held, release := worker.holdLease(context.Background(), "restore-1", "token-1", true,
		time.Now().Add(time.Hour))
	deadline := time.After(2 * time.Second)
	for renewals.Load() == 0 {
		select {
		case <-deadline:
			release()
			t.Fatal("the lease was never renewed, so long work would be taken back")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if held.Err() != nil {
		t.Errorf("the work was cancelled while its lease was being renewed: %v", held.Err())
	}
	release()
	if !sawRestore.Load() {
		t.Error("the renewal did not say which queue the job is in")
	}
	// And it stops when the work does.
	settled := renewals.Load()
	time.Sleep(120 * time.Millisecond)
	if renewals.Load() != settled {
		t.Error("the heartbeat kept going after the work finished")
	}
}

// Losing the lease stops the work. Continuing to write into a live account
// with no claim on it is the thing the claim token forbids, and another
// attempt may already be doing exactly that.
func TestWorkStopsWhenItsLeaseIsGone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(protocol.ErrorResponse{
			Error: "this job is not leased to you"})
	}))
	defer server.Close()

	worker := &Agent{
		client:          NewClientWithHTTP(server.URL, server.Client()),
		log:             slog.New(slog.DiscardHandler),
		LeaseRenewEvery: 10 * time.Millisecond,
	}
	held, release := worker.holdLease(context.Background(), "restore-1", "token-1", true,
		time.Now().Add(time.Hour))
	defer release()

	select {
	case <-held.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a restore whose lease was taken back kept writing into the live account")
	}
}

// A controller that cannot be reached has not taken the lease away. The
// work carries on: stopping a restore halfway because of a network blip
// would leave the account half-written for no reason.
func TestAnUnreachableControllerDoesNotStopTheWork(t *testing.T) {
	var tries atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tries.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	worker := &Agent{
		client:          NewClientWithHTTP(server.URL, server.Client()),
		log:             slog.New(slog.DiscardHandler),
		LeaseRenewEvery: 10 * time.Millisecond,
	}
	held, release := worker.holdLease(context.Background(), "restore-1", "token-1", true,
		time.Now().Add(time.Hour))
	defer release()

	deadline := time.After(2 * time.Second)
	for tries.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("the heartbeat gave up after one failure")
		case <-held.Done():
			t.Fatal("a controller that was merely unwell stopped a live restore")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if held.Err() != nil {
		t.Errorf("the work was cancelled: %v", held.Err())
	}
}

// Standalone has no controller and no lease, and must not grow a
// heartbeat goroutine that has nowhere to beat.
func TestStandaloneHoldsNoLease(t *testing.T) {
	worker := &Agent{log: slog.New(slog.DiscardHandler)}
	ctx := context.Background()
	held, release := worker.holdLease(ctx, "job-1", "token-1", false, time.Time{})
	release()
	if held != ctx {
		t.Error("standalone work runs under a context it did not ask for")
	}
}
