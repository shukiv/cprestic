package destination_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/shuki/cprest/internal/destination"
)

// A local destination reports the filesystem holding it. The numbers are
// the machine's, so what is asserted is that they are coherent rather than
// any particular size.
func TestLocalReportsTheFilesystemItSitsOn(t *testing.T) {
	root := t.TempDir()
	local := &destination.Local{Root: root}

	space, err := local.Space(context.Background())
	if err != nil {
		t.Fatalf("Space: %v", err)
	}
	if space.TotalBytes == 0 {
		t.Error("a filesystem with no size")
	}
	if space.FreeBytes > space.TotalBytes {
		t.Errorf("more free than there is: %d of %d", space.FreeBytes, space.TotalBytes)
	}
	if space.UsedBytes() != space.TotalBytes-space.FreeBytes {
		t.Error("used does not account for the difference")
	}
}

func TestLocalSpaceFailsOnADirectoryThatIsNotThere(t *testing.T) {
	local := &destination.Local{Root: filepath.Join(t.TempDir(), "gone")}
	if _, err := local.Space(context.Background()); err == nil {
		t.Error("a missing root reported a size")
	}
}

// The measurement must not be silently wrong when the storage is a mount
// of its own: statfs answers for the filesystem the path is on, which is
// the point of asking about the root rather than the process's own disk.
func TestLocalSpaceAnswersForTheRootGiven(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	outer, err := (&destination.Local{Root: root}).Space(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	inner, err := (&destination.Local{Root: filepath.Join(root, "repo")}).Space(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outer.TotalBytes != inner.TotalBytes {
		t.Errorf("two paths on one filesystem disagree: %d and %d",
			outer.TotalBytes, inner.TotalBytes)
	}
}

// df output is read from the end of the row, because a long device name
// makes df wrap the row onto two lines and the columns then start in a
// different place. Getting this wrong reports somebody else's filesystem.
func TestRemoteDFIsReadFromTheEndOfTheRow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		total  uint64
		free   uint64
	}{
		{
			name: "ordinary",
			output: "Filesystem 1024-blocks    Used Available Capacity Mounted on\n" +
				"/dev/sda1    52403200 4194304  48208896       8% /srv\n",
			total: 52403200 * 1024,
			free:  48208896 * 1024,
		},
		{
			name: "device name long enough that df wraps the row",
			output: "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
				"/dev/mapper/a-very-long-volume-group-name-indeed\n" +
				"             52403200 4194304  48208896       8% /srv\n",
			total: 52403200 * 1024,
			free:  48208896 * 1024,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			space, err := destination.ParseDFForTest(tc.output)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if space.TotalBytes != tc.total || space.FreeBytes != tc.free {
				t.Errorf("got %d of %d, want %d of %d",
					space.FreeBytes, space.TotalBytes, tc.free, tc.total)
			}
		})
	}

	if _, err := destination.ParseDFForTest("df: /srv: No such file or directory\n"); err == nil {
		t.Error("df failing was read as a size")
	}
}

// An S3 bucket has no size and a REST server does not report one. They must
// not pretend otherwise: the page says "cannot say", which is not the same
// as a disk that is full.
func TestObjectStoresDoNotClaimASize(t *testing.T) {
	for _, built := range []destination.Destination{
		&destination.S3{Bucket: "backups"},
		&destination.REST{BaseURL: "https://backup.example:8000"},
	} {
		if _, ok := built.(destination.Sizer); ok {
			t.Errorf("%T claims to know its size", built)
		}
	}
}
