package reassemble

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAccountArchiveRejectsSymlinkAndConflictingUSER(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "cpmove-customer1.tar")
	writeTestTar(t, archive, map[string]string{"cpmove-customer1/cp/customer1": "USER=victim\n"})
	if err := ValidateAccountArchive(t.Context(), archive, "customer1"); err == nil {
		t.Fatal("conflicting USER field was accepted")
	}
	writeTestTar(t, archive, map[string]string{"cpmove-customer1/cp/customer1": "USER=customer1\n"})
	if err := ValidateAccountArchive(t.Context(), archive, "customer1"); err != nil {
		t.Fatalf("valid identity was refused: %v", err)
	}
	link := filepath.Join(t.TempDir(), "cpmove-customer1.tar")
	if err := os.Symlink(archive, link); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAccountArchive(t.Context(), link, "customer1"); err == nil {
		t.Fatal("a symlink was accepted as a restored account archive")
	}
}

func TestAccountArchiveCannotContradictTheSnapshotTag(t *testing.T) {
	for _, monolithic := range []bool{false, true} {
		for _, member := range []string{"cpmove-victim/cp/victim", "cpmove-customer1/cp/victim", "cpmove-customer1/meta/user"} {
			t.Run(member+map[bool]string{false: "/split", true: "/monolithic"}[monolithic], func(t *testing.T) {
				r, root := buildSplitSnapshot(t)
				metadata := r.source[r.snapshot.Paths[0]]
				archive := filepath.Join(metadata, "cpmove-customer1.tar")
				writeTestTar(t, archive, map[string]string{member: "victim\n"})
				if monolithic {
					r.snapshot.Paths = []string{"/stage/metadata/cpmove-customer1.tar"}
					r.source = map[string]string{"/stage/metadata": metadata}
				}
				_, err := Run(context.Background(), r, Request{
					Account: "customer1", SnapshotID: r.snapshot.ID, WorkDir: filepath.Join(root, "work"),
				})
				if err == nil {
					t.Fatal("accepted another account's archive under customer1's snapshot tag")
				}
			})
		}
	}
}
