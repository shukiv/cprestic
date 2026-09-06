package agent

import (
	"archive/tar"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/destination"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/staging"
)

// TestASecondRestoreDoesNotOverwriteTheFirstOnesArchive covers what a
// stored restore's archive path means.
//
// Rebuilt archives were keyed by account, so every restore of an account
// wrote to the same path. The second one replaced the first one's file
// while the first one's record went on pointing at it, and a download
// asked for by the older restore's id handed over the newer snapshot,
// under the older one's date. An operator rebuilding a customer from
// before a bad change would get the bad change back and be told it was
// the older backup.
//
// Each restore now owns its output. The superseded one is removed, so the
// older record's path stops resolving and the page says the archive is
// gone -- which is true -- rather than handing over a different backup.
func TestASecondRestoreDoesNotOverwriteTheFirstOnesArchive(t *testing.T) {
	root := t.TempDir()
	stagingRoot := filepath.Join(root, "staging")
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	worker := New(Config{
		Provider: &cpanel.Fake{Root: filepath.Join(root, "cpanel")},
		Staging:  &staging.Manager{Root: stagingRoot},
		Runner: resticrun.New(resticrun.Config{RuntimeDir: root},
			archiveRestorer(t, "c1")),
		Log: slog.New(slog.DiscardHandler),
	})

	rebuild := func(jobID, snapshotID string) protocol.RestoreReport {
		t.Helper()
		report := worker.RunRestore(context.Background(), protocol.RestoreAssignment{
			JobID: jobID, CPanelUser: "c1", SnapshotID: snapshotID,
			Kind: protocol.RestoreAccount, SizeEstimate: 1024,
			Source: protocol.Target{Spec: destination.Spec{Type: destination.TypeLocal,
				Config: map[string]string{"root": root}}, RepoPath: "repo",
				RepoPassword: "password"},
		})
		if report.Status != "success" {
			t.Fatalf("restore %s: %+v", jobID, report)
		}
		if report.ArchivePath == "" {
			t.Fatalf("restore %s produced no archive", jobID)
		}
		return report
	}

	first := rebuild("restore-january", "aaaaaaaaaaaaaaaa")
	if _, err := os.Stat(first.ArchivePath); err != nil {
		t.Fatalf("the first archive is not where it was reported: %v", err)
	}
	second := rebuild("restore-march", "bbbbbbbbbbbbbbbb")

	if first.ArchivePath == second.ArchivePath {
		t.Fatalf("both restores were written to %s, so the older record now "+
			"points at the newer backup", first.ArchivePath)
	}
	if _, err := os.Stat(first.ArchivePath); err == nil {
		t.Errorf("the superseded archive is still at %s, taking up the disk",
			first.ArchivePath)
	}
	if _, err := os.Stat(second.ArchivePath); err != nil {
		t.Errorf("the archive that was just rebuilt is not there: %v", err)
	}
}

// archiveRestorer answers the way restic does for a whole-account restore
// of one snapshot, writing the archive the reassembler expects to find.
func archiveRestorer(t *testing.T, account string) resticrun.Execer {
	t.Helper()
	return resticrun.ExecFunc(func(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
		for i, arg := range cmd.Args {
			if arg == "snapshots" {
				// Two snapshots of the same account, months apart: the
				// pair an operator has to be able to tell apart.
				var listed string
				for _, id := range []string{"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"} {
					if listed != "" {
						listed += ","
					}
					listed += `{"id":"` + id + `","summary":{"total_bytes_processed":1024},` +
						`"tags":["account:` + account + `","mode:monolithic"],` +
						`"paths":["/staging/` + account + `/cpmove-` + account + `.tar"]}`
				}
				return resticrun.CommandResult{Stdout: []byte("[" + listed + "]")}, nil
			}
			if arg == "--target" {
				target := cmd.Args[i+1]
				if err := os.MkdirAll(target, 0o700); err != nil {
					return resticrun.CommandResult{}, err
				}
				archive := filepath.Join(target, "cpmove-"+account+".tar")
				writeAccountArchive(t, archive, account)
				return resticrun.CommandResult{Stdout: []byte(
					`{"message_type":"summary","files_restored":1,"bytes_restored":7}`)}, nil
			}
		}
		t.Fatalf("unexpected restic command %v", cmd.Args)
		return resticrun.CommandResult{}, nil
	})
}

// TestSupersedingOneAccountsRestoreLeavesAnotherAccountsAlone covers what
// the key of a superseded restore has to mean.
//
// The output of every restore of an account shares a prefix, and a new
// restore removes what that prefix matches. With the account and the
// restore's id joined by a hyphen, the prefix for "c1" also matched every
// output belonging to an account named "c1-x": restoring one customer
// deleted the archive another customer's operator was about to download,
// and that record went on pointing at a file that was no longer there.
func TestSupersedingOneAccountsRestoreLeavesAnotherAccountsAlone(t *testing.T) {
	root := t.TempDir()
	stagingRoot := filepath.Join(root, "staging")
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	worker := New(Config{
		Provider: &cpanel.Fake{Root: filepath.Join(root, "cpanel")},
		Staging:  &staging.Manager{Root: stagingRoot},
		Runner: resticrun.New(resticrun.Config{RuntimeDir: root},
			archiveRestorerFor(t, map[string]string{
				"aaaaaaaaaaaaaaaa": "c1-x",
				"bbbbbbbbbbbbbbbb": "c1",
			})),
		Log: slog.New(slog.DiscardHandler),
	})

	rebuild := func(jobID, account, snapshotID string) protocol.RestoreReport {
		t.Helper()
		report := worker.RunRestore(context.Background(), protocol.RestoreAssignment{
			JobID: jobID, CPanelUser: account, SnapshotID: snapshotID,
			Kind: protocol.RestoreAccount, SizeEstimate: 1024,
			Source: protocol.Target{Spec: destination.Spec{Type: destination.TypeLocal,
				Config: map[string]string{"root": root}}, RepoPath: "repo",
				RepoPassword: "password"},
		})
		if report.Status != "success" {
			t.Fatalf("restore of %s: %+v", account, report)
		}
		return report
	}

	neighbour := rebuild("restore-neighbour", "c1-x", "aaaaaaaaaaaaaaaa")
	rebuild("restore-mine", "c1", "bbbbbbbbbbbbbbbb")

	if _, err := os.Stat(neighbour.ArchivePath); err != nil {
		t.Errorf("restoring c1 threw away the archive belonging to c1-x at %s: %v",
			neighbour.ArchivePath, err)
	}
}

// archiveRestorerFor answers for several accounts at once, so a restore
// of one can be checked against another's output.
func archiveRestorerFor(t *testing.T, accounts map[string]string) resticrun.Execer {
	t.Helper()
	return resticrun.ExecFunc(func(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
		for i, arg := range cmd.Args {
			if arg == "snapshots" {
				var listed string
				for id, account := range accounts {
					if listed != "" {
						listed += ","
					}
					listed += `{"id":"` + id + `","summary":{"total_bytes_processed":1024},` +
						`"tags":["account:` + account + `","mode:monolithic"],` +
						`"paths":["/staging/` + account + `/cpmove-` + account + `.tar"]}`
				}
				return resticrun.CommandResult{Stdout: []byte("[" + listed + "]")}, nil
			}
			if arg != "--target" {
				continue
			}
			account := ""
			for _, other := range cmd.Args {
				// restic is asked for a subpath of a snapshot, so the
				// argument is "<id>:<path>".
				id, _, _ := strings.Cut(other, ":")
				if name, found := accounts[id]; found {
					account = name
				}
			}
			if account == "" {
				t.Fatalf("restore command names no known snapshot: %v", cmd.Args)
			}
			target := cmd.Args[i+1]
			if err := os.MkdirAll(target, 0o700); err != nil {
				return resticrun.CommandResult{}, err
			}
			archive := filepath.Join(target, "cpmove-"+account+".tar")
			writeAccountArchive(t, archive, account)
			return resticrun.CommandResult{Stdout: []byte(
				`{"message_type":"summary","files_restored":1,"bytes_restored":7}`)}, nil
		}
		t.Fatalf("unexpected restic command %v", cmd.Args)
		return resticrun.CommandResult{}, nil
	})
}

func writeAccountArchive(t *testing.T, filename, account string) {
	t.Helper()
	f, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := tar.NewWriter(f)
	body := "USER=" + account + "\n"
	if err := w.WriteHeader(&tar.Header{Name: "cpmove-" + account + "/cp/" + account, Mode: 0600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
