package resticrun

import (
	"context"
	"strings"
	"testing"

	"github.com/shuki/cprest/internal/destination"
)

func TestHistoricalSnapshotSizeIsMeasuredNotGuessed(t *testing.T) {
	for _, output := range []string{`{"total_size":9000000000}`, `{}`, `{"total_size":0}`} {
		t.Run(output, func(t *testing.T) {
			r := New(Config{RuntimeDir: t.TempDir()}, ExecFunc(func(_ context.Context, cmd Command) (CommandResult, error) {
				if !strings.Contains(strings.Join(cmd.Args, " "), "stats --json --mode restore-size abcdef0123456789") {
					t.Fatalf("unexpected measurement command: %v", cmd.Args)
				}
				return CommandResult{Stdout: []byte(output)}, nil
			}))
			size, err := r.RestoreSize(t.Context(), Repository{Dest: &destination.Local{Root: t.TempDir()}, Path: "repo", Password: "test"}, Snapshot{ID: "abcdef0123456789"})
			if strings.Contains(output, "9000000000") {
				if err != nil || size != 9000000000 {
					t.Fatalf("size=%d err=%v", size, err)
				}
			} else if err == nil {
				t.Fatal("unknown size was accepted")
			}
		})
	}
}
