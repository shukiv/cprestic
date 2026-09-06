package agent

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/destination"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/staging"
)

func TestRestorePreflightDoesNotSubstituteLiveSizeForUnknownHistory(t *testing.T) {
	for _, stats := range []string{`{}`, `{"total_size":9000000000000000000}`} {
		t.Run(stats, func(t *testing.T) {
			root := t.TempDir()
			executor := resticrun.ExecFunc(func(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
				switch cmd.Args[0] {
				case "snapshots":
					return resticrun.CommandResult{Stdout: []byte(`[{"id":"aaaaaaaaaaaaaaaa","tags":["account:c1"],"paths":["/stage/metadata","/home/c1"]}]`)}, nil
				case "stats":
					return resticrun.CommandResult{Stdout: []byte(stats)}, nil
				default:
					t.Fatalf("restore started without sufficient verified space: %v", cmd.Args)
					return resticrun.CommandResult{}, nil
				}
			})
			worker := New(Config{Provider: &cpanel.Fake{}, Staging: &staging.Manager{Root: root},
				Runner: resticrun.New(resticrun.Config{RuntimeDir: root}, executor), Log: slog.New(slog.DiscardHandler)})
			report := worker.RunRestore(t.Context(), protocol.RestoreAssignment{JobID: "restore", CPanelUser: "c1", SnapshotID: "aaaaaaaaaaaaaaaa",
				Kind: protocol.RestoreAccount, SizeEstimate: 1024,
				Source: protocol.Target{Spec: destination.Spec{Type: destination.TypeLocal, Config: map[string]string{"root": root}}, RepoPath: "repo", RepoPassword: "test"}})
			if report.Status != "failed" || (!strings.Contains(report.Error, "restore size") && !strings.Contains(report.Error, "not enough room")) {
				t.Fatalf("unexpected preflight result: %+v", report)
			}
		})
	}
}
