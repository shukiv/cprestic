package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/store"
)

// TestOneServerCannotReportOnAnothersWork: an agent's certificate says
// which server it is, and a job id says nothing about who is entitled to
// finish it. A second registered server -- one that has been compromised,
// or is simply wrong -- must not be able to mark somebody else's backup
// failed or somebody else's restore done.
func TestOneServerCannotReportOnAnothersWork(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	intruder, err := f.db.CreateServer(ctx, "cp02.example.com", "fingerprint-cp02")
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}

	t.Run("a backup", func(t *testing.T) {
		jobID, err := f.db.CreateJob(ctx, f.accountID, f.policyID)
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		if _, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute); err != nil {
			t.Fatalf("ClaimNextJob: %v", err)
		}

		if _, err := f.db.ApplyReport(ctx, intruder, jobID, nil,
			"staging: need 8192 bytes free, have 512"); err == nil {
			t.Error("another server failed this server's backup")
		}
		status, err := f.db.JobStatus(ctx, jobID)
		if err != nil {
			t.Fatal(err)
		}
		if status != job.StatusRunning {
			t.Errorf("the job is %q, want it still running", status)
		}

		// The server that has it can still finish it.
		if _, err := f.db.ApplyReport(ctx, f.serverID, jobID, []store.TargetReport{
			{RepositoryID: f.repoA.ID, Status: job.TargetSuccess, SnapshotID: "40dc1520"},
			{RepositoryID: f.repoB.ID, Status: job.TargetSuccess, SnapshotID: "40dc1521"},
		}, ""); err != nil {
			t.Fatalf("the owning server could not report: %v", err)
		}
	})

	t.Run("a restore", func(t *testing.T) {
		jobID, err := f.db.CreateRestore(ctx, store.RestoreRequest{
			AccountID: f.accountID, RepositoryID: f.repoA.ID, SnapshotID: "40dc1520",
		})
		if err != nil {
			t.Fatalf("CreateRestore: %v", err)
		}

		// Not even claimed yet: reporting it successful would take it off
		// the queue, and nobody would ever perform it.
		if err := f.db.ApplyRestoreReport(ctx, intruder, jobID, store.RestoreOutcome{
			Status: job.StatusSuccess, BytesRestored: 4096,
		}); err == nil {
			t.Error("another server reported an unclaimed restore as done")
		}
		if _, err := f.db.ClaimNextRestore(ctx, f.serverID, time.Minute); err != nil {
			t.Fatalf("the restore was taken off the queue: %v", err)
		}
		if err := f.db.ApplyRestoreReport(ctx, intruder, jobID, store.RestoreOutcome{
			Status: job.StatusSuccess, BytesRestored: 4096,
		}); err == nil {
			t.Error("another server finished this server's restore")
		}
		stored, err := f.db.RestoreByID(ctx, jobID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != job.StatusRunning {
			t.Errorf("the restore is %q, want it still running", stored.Status)
		}
		if err := f.db.ApplyRestoreReport(ctx, f.serverID, jobID, store.RestoreOutcome{
			Status: job.StatusSuccess, BytesRestored: 4096,
		}); err != nil {
			t.Fatalf("the owning server could not report: %v", err)
		}
	})
}
