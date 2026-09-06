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

// TestAMonolithicDownloadIsWhereItSaysItIs: a full-account restore that is
// only rebuilt to be collected must report the path the archive is
// actually at. A monolithic snapshot keeps it one directory further down,
// and the reported path used to drop that: the job said success and the
// interface had nothing to hand over.
func TestAMonolithicDownloadIsWhereItSaysItIs(t *testing.T) {
	root := t.TempDir()
	executor := resticrun.ExecFunc(func(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
		for i, arg := range cmd.Args {
			if arg == "snapshots" {
				return resticrun.CommandResult{Stdout: []byte(`[{"id":"abcdef0123456789",` +
					`"short_id":"abcdef01","summary":{"total_bytes_processed":1024},"tags":["account:c1","mode:monolithic"],` +
					`"paths":["/staging/c1/cpmove-c1.tar"]}]`)}, nil
			}
			if arg == "--target" {
				target := cmd.Args[i+1]
				if err := os.MkdirAll(target, 0o700); err != nil {
					return resticrun.CommandResult{}, err
				}
				writeAccountArchive(t, filepath.Join(target, "cpmove-c1.tar"), "c1")
				return resticrun.CommandResult{Stdout: []byte(
					`{"message_type":"summary","files_restored":1,"bytes_restored":10}`)}, nil
			}
		}
		t.Fatalf("unexpected restic command %v", cmd.Args)
		return resticrun.CommandResult{}, nil
	})

	worker := New(Config{
		Provider: &cpanel.Fake{},
		Staging:  &staging.Manager{Root: root},
		Runner:   resticrun.New(resticrun.Config{RuntimeDir: root}, executor),
		Log:      slog.New(slog.DiscardHandler),
	})
	result := worker.RunRestore(context.Background(), protocol.RestoreAssignment{
		JobID: "download", CPanelUser: "c1", SnapshotID: "abcdef0123456789",
		Kind: protocol.RestoreAccount, SizeEstimate: 1024,
		Source: protocol.Target{
			Spec: destination.Spec{Type: destination.TypeLocal,
				Config: map[string]string{"root": root}},
			RepoPath: "repo", RepoPassword: "test-password",
		},
	})
	if result.Status != "success" {
		t.Fatalf("the restore failed: %+v", result)
	}
	if _, err := os.Stat(result.ArchivePath); err != nil {
		t.Fatalf("nothing is at the path it reported (%s): %v", result.ArchivePath, err)
	}
}
