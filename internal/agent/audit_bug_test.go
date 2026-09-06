package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/destination"
	"github.com/shuki/cprest/internal/protocol"
	"github.com/shuki/cprest/internal/resticrun"
	"github.com/shuki/cprest/internal/staging"
)

// Positive audit reproduction: Apply returns success but changes nothing;
// an account that existed before is enough for the job to report applied.
func TestAuditNoOpOverwriteIsReportedApplied(t *testing.T) {
	root := t.TempDir()
	executor := resticrun.ExecFunc(func(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
		for i, arg := range cmd.Args {
			if arg == "snapshots" {
				return resticrun.CommandResult{Stdout: []byte(`[{"id":"abcdef0123456789",` +
					`"tags":["account:c1","mode:monolithic"],` +
					`"paths":["/staging/c1/cpmove-c1.tar"]}]`)}, nil
			}
			if arg == "--target" {
				target := cmd.Args[i+1]
				if err := os.MkdirAll(target, 0o700); err != nil {
					return resticrun.CommandResult{}, err
				}
				if err := os.WriteFile(filepath.Join(target, "cpmove-c1.tar"), []byte("archive"), 0o600); err != nil {
					return resticrun.CommandResult{}, err
				}
				return resticrun.CommandResult{Stdout: []byte(
					`{"message_type":"summary","files_restored":1,"bytes_restored":7}`)}, nil
			}
		}
		t.Fatalf("unexpected restic command %v", cmd.Args)
		return resticrun.CommandResult{}, nil
	})
	provider := &cpanel.Fake{Root: filepath.Join(root, "cpanel")}
	stagingRoot := filepath.Join(root, "staging")
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	worker := New(Config{
		Provider: provider, Staging: &staging.Manager{Root: stagingRoot},
		Runner: resticrun.New(resticrun.Config{RuntimeDir: root}, executor),
		Log:    slog.New(slog.DiscardHandler),
	})
	report := worker.RunRestore(context.Background(), protocol.RestoreAssignment{
		JobID: "restore", CPanelUser: "c1", SnapshotID: "abcdef0123456789",
		Kind: protocol.RestoreAccount, Apply: true, SizeEstimate: 1024,
		Source: protocol.Target{Spec: destination.Spec{Type: destination.TypeLocal,
			Config: map[string]string{"root": root}}, RepoPath: "repo", RepoPassword: "password"},
	})
	if report.Status != "success" || !report.Applied {
		t.Fatalf("no-op restore was not reported applied: %+v", report)
	}
	if len(provider.Applied) != 1 {
		t.Fatalf("Apply was not reached: %+v", provider.Applied)
	}
	// Fake.Apply deliberately changes no account state. Account() succeeds
	// before and after, which is the production overwrite check too.
}
