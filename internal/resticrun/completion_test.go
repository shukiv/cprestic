package resticrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/destination"
)

func completionRepo(t *testing.T) Repository {
	return Repository{Dest: &destination.Local{Root: t.TempDir()}, Path: "repo", Password: "test"}
}

func TestOnlySuccessfulReadsPublishCompletionReceipts(t *testing.T) {
	for _, exit := range []int{0, 3} {
		calls := 0
		r := New(Config{RuntimeDir: t.TempDir()}, ExecFunc(func(_ context.Context, cmd Command) (CommandResult, error) {
			calls++
			args := strings.Join(cmd.Args, " ")
			if calls == 1 {
				if !strings.Contains(args, NeedsCompletionTag) {
					t.Fatal("payload was not marked unverified before backup")
				}
				return CommandResult{Stdout: []byte(sampleBackupJSON), ExitCode: exit}, nil
			}
			if !strings.Contains(args, CompletionReceiptTag) || !strings.Contains(args, completedIDPrefix+"40dc15203b1cf9") {
				t.Fatalf("not a receipt for the exact successful snapshot: %v", cmd.Args)
			}
			return CommandResult{Stdout: []byte(sampleBackupJSON)}, nil
		}))
		result, err := r.Backup(t.Context(), completionRepo(t), BackupSpec{Paths: []string{"/account"}, RecordCompletion: true})
		if err != nil {
			t.Fatal(err)
		}
		if exit == 3 && (calls != 1 || !result.Incomplete) {
			t.Fatal("incomplete read published a completion receipt")
		}
		if exit == 0 && (calls != 2 || result.Incomplete) {
			t.Fatal("successful read did not publish its receipt")
		}
	}
}

func TestRecoveryReconstructsReadStatusWithoutLocalJobHistory(t *testing.T) {
	const good, bad = "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"
	payloads, _ := json.Marshal([]Snapshot{
		{ID: good, Tags: []string{"account:c1", NeedsCompletionTag}},
		{ID: bad, Tags: []string{"account:c1", NeedsCompletionTag}},
	})
	receipts, _ := json.Marshal([]Snapshot{{ID: "cccccccccccccccc", Tags: []string{CompletionReceiptTag, completedIDPrefix + good}}})
	r := New(Config{RuntimeDir: t.TempDir()}, ExecFunc(func(_ context.Context, cmd Command) (CommandResult, error) {
		if hasTag(cmd.Args, CompletionReceiptTag) {
			return CommandResult{Stdout: receipts}, nil
		}
		return CommandResult{Stdout: payloads}, nil
	}))
	snapshots, err := r.Snapshots(t.Context(), completionRepo(t), SnapshotFilter{Tags: []string{"account:c1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 || !snapshots[0].Complete() || snapshots[1].Complete() {
		t.Fatalf("source-read status was lost with the local history: %+v", snapshots)
	}
}

func TestIncompleteReadsCannotEvictACompleteSnapshot(t *testing.T) {
	for _, dry := range []bool{true, false} {
		deletes := 0
		group := retentionGroupJSON{Host: "cp01", Tags: []string{"account:c1", NeedsCompletionTag},
			Keep:   []Snapshot{{ID: "bbbbbbbbbbbbbbbb", Tags: []string{"account:c1", NeedsCompletionTag}}},
			Remove: []Snapshot{{ID: "aaaaaaaaaaaaaaaa", Tags: []string{"account:c1", NeedsCompletionTag}}}}
		planJSON, _ := json.Marshal([]retentionGroupJSON{group})
		receipts, _ := json.Marshal([]Snapshot{{ID: "cccccccccccccccc", Tags: []string{CompletionReceiptTag, completedIDPrefix + "aaaaaaaaaaaaaaaa"}}})
		r := New(Config{RuntimeDir: t.TempDir()}, ExecFunc(func(_ context.Context, cmd Command) (CommandResult, error) {
			if hasTag(cmd.Args, "snapshots") {
				return CommandResult{Stdout: receipts}, nil
			}
			if hasTag(cmd.Args, "--dry-run") {
				return CommandResult{Stdout: planJSON}, nil
			}
			deletes++
			return CommandResult{}, nil
		}))
		plan, err := r.ForgetPlanned(t.Context(), completionRepo(t), ForgetSpec{KeepLast: 1, DryRun: dry})
		if err != nil {
			t.Fatal(err)
		}
		if deletes != 0 || plan.Removed != 0 || plan.Kept != 2 || !plan.Groups[0].Protected {
			t.Fatalf("incomplete newest snapshot evicted the only complete one: %+v, deletes=%d", plan, deletes)
		}
	}
}

func TestRetentionAppliesOnlyTheExactReviewedIDs(t *testing.T) {
	var commands [][]string
	group := retentionGroupJSON{Host: "cp01", Tags: []string{"account:c1", NeedsCompletionTag},
		Keep:   []Snapshot{{ID: "bbbbbbbbbbbbbbbb", Tags: []string{NeedsCompletionTag}}},
		Remove: []Snapshot{{ID: "aaaaaaaaaaaaaaaa", Tags: []string{NeedsCompletionTag}}}}
	planJSON, _ := json.Marshal([]retentionGroupJSON{group})
	receipts, _ := json.Marshal([]Snapshot{
		{Tags: []string{CompletionReceiptTag, completedIDPrefix + "aaaaaaaaaaaaaaaa"}},
		{Tags: []string{CompletionReceiptTag, completedIDPrefix + "bbbbbbbbbbbbbbbb"}},
	})
	r := New(Config{RuntimeDir: t.TempDir()}, ExecFunc(func(_ context.Context, cmd Command) (CommandResult, error) {
		commands = append(commands, cmd.Args)
		if hasTag(cmd.Args, "--dry-run") {
			return CommandResult{Stdout: planJSON}, nil
		}
		if hasTag(cmd.Args, "snapshots") {
			return CommandResult{Stdout: receipts}, nil
		}
		return CommandResult{}, nil
	}))
	plan, err := r.ForgetPlanned(t.Context(), completionRepo(t), ForgetSpec{KeepLast: 1})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Removed != 1 || len(commands) != 3 {
		t.Fatalf("plan=%+v commands=%v", plan, commands)
	}
	assertArgs(t, commands[2], []string{"forget", "aaaaaaaaaaaaaaaa"})
}
