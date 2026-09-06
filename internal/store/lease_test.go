package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/store"
)

// A restore that writes into a live account and outlives its lease must
// not be handed to another attempt.
//
// The lease was a fixed six hours with no way to extend it, so a large
// account on a slow link ran past it. The controller then took the work
// back, refused the successful report when it came, and put the same
// destructive restore on the queue -- to be run a second time over an
// account the first attempt had already half-rewritten, possibly while it
// was still writing.
func TestARestoreThatWritesIntoTheAccountIsNotRequeuedWhenItsLeaseRunsOut(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if err := f.db.MarkRepositoryInitialised(ctx, f.repoA.ID); err != nil {
		t.Fatal(err)
	}
	jobID, err := f.db.CreateRestore(ctx, store.RestoreRequest{
		AccountID: f.accountID, RepositoryID: f.repoA.ID,
		SnapshotID: "40dc15203b1cf9", Apply: true,
	})
	if err != nil {
		t.Fatalf("CreateRestore: %v", err)
	}
	if _, err := f.db.ClaimNextRestore(ctx, f.serverID, 10*time.Millisecond); err != nil {
		t.Fatalf("ClaimNextRestore: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	reclaimed, err := f.db.ReclaimExpiredRestoreLeases(ctx)
	if err != nil {
		t.Fatalf("ReclaimExpiredRestoreLeases: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed %d restores, want the one whose lease expired", reclaimed)
	}

	stored, err := f.db.RestoreByID(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != job.StatusFailed {
		t.Fatalf("the restore is %q; a destructive restore whose lease ran out "+
			"must stop rather than run again unattended", stored.Status)
	}
	if stored.Error == "" {
		t.Error("nothing says why it stopped, so nobody knows to check the account")
	}

	// And it is not offered to another attempt.
	if _, err := f.db.ClaimNextRestore(ctx, f.serverID, time.Minute); !errors.Is(err, store.ErrNoWork) {
		t.Fatalf("the destructive restore was claimed again: %v", err)
	}
}

// A restore that only produces a copy costs nothing to repeat, so it goes
// back on the queue as before.
func TestARestoreThatOnlyMakesACopyIsStillRequeued(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if err := f.db.MarkRepositoryInitialised(ctx, f.repoA.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateRestore(ctx, store.RestoreRequest{
		AccountID: f.accountID, RepositoryID: f.repoA.ID,
		SnapshotID: "40dc15203b1cf9",
	}); err != nil {
		t.Fatalf("CreateRestore: %v", err)
	}
	if _, err := f.db.ClaimNextRestore(ctx, f.serverID, 10*time.Millisecond); err != nil {
		t.Fatalf("ClaimNextRestore: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := f.db.ReclaimExpiredRestoreLeases(ctx); err != nil {
		t.Fatalf("ReclaimExpiredRestoreLeases: %v", err)
	}
	if _, err := f.db.ClaimNextRestore(ctx, f.serverID, time.Minute); err != nil {
		t.Fatalf("a restore that changes nothing was not offered again: %v", err)
	}
}

// Work that says it is still going keeps its lease, which is what stops
// the reclaim from ever reaching it.
func TestRenewingALeaseKeepsTheWork(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if err := f.db.MarkRepositoryInitialised(ctx, f.repoA.ID); err != nil {
		t.Fatal(err)
	}
	jobID, err := f.db.CreateRestore(ctx, store.RestoreRequest{
		AccountID: f.accountID, RepositoryID: f.repoA.ID,
		SnapshotID: "40dc15203b1cf9", Apply: true,
	})
	if err != nil {
		t.Fatalf("CreateRestore: %v", err)
	}
	claimed, err := f.db.ClaimNextRestore(ctx, f.serverID, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("ClaimNextRestore: %v", err)
	}

	expires, err := f.db.RenewRestoreLease(ctx, f.serverID, jobID, claimed.ClaimToken, time.Hour)
	if err != nil {
		t.Fatalf("RenewRestoreLease: %v", err)
	}
	if !expires.After(claimed.LeaseExpiresAt) {
		t.Errorf("the lease was not extended: %v is not after %v",
			expires, claimed.LeaseExpiresAt)
	}
	time.Sleep(60 * time.Millisecond)
	if reclaimed, err := f.db.ReclaimExpiredRestoreLeases(ctx); err != nil || reclaimed != 0 {
		t.Fatalf("a renewed restore was taken back: reclaimed %d, %v", reclaimed, err)
	}

	// The attempt that holds the job can still report, which is the whole
	// point: without renewal its successful report was refused.
	if err := f.db.ApplyRestoreReport(ctx, f.serverID, jobID, claimed.ClaimToken,
		store.RestoreOutcome{Status: job.StatusSuccess, BytesRestored: 4096},
	); err != nil {
		t.Fatalf("the attempt that held the restore could not report: %v", err)
	}
}

// Only the attempt that holds a job may extend it. An agent whose lease
// was taken back must not be able to take it from whoever has it now, and
// no server may extend another server's work.
func TestOnlyTheAttemptHoldingAJobCanExtendIt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	jobID, err := f.db.CreateJob(ctx, f.accountID, f.policyID)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	claimed, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute)
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}

	other, err := f.db.CreateServer(ctx, "cp02.example.com", "fingerprint-cp02")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		what     string
		serverID string
		token    string
	}{
		{"another server", other, claimed.ClaimToken},
		{"an abandoned attempt", f.serverID, "8a1f0c62-0b4a-4c1e-9a53-0f6b7c2d5e41"},
	} {
		if _, err := f.db.RenewBackupLease(ctx, tc.serverID, jobID, tc.token, time.Hour); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s extended the lease and got %v, want ErrNotFound", tc.what, err)
		}
	}
	if _, err := f.db.RenewBackupLease(ctx, f.serverID, jobID, claimed.ClaimToken, time.Hour); err != nil {
		t.Errorf("the attempt that holds the job could not extend it: %v", err)
	}
}
