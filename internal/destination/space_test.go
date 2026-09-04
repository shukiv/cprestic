package destination_test

import (
	"context"
	"os"
	"os/exec"
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

// ssh parses options until it meets one it does not know, so a host of
// "-oProxyCommand=..." is an option — and ProxyCommand runs here, as this
// process, which is root. The "--" has to come before the host, and the
// host has to be refused as well.
func TestTheRemoteProbeCannotBeTalkedIntoRunningSomethingElse(t *testing.T) {
	sftp := &destination.SFTP{
		Host: "backup.example", User: "cprest", Root: "/srv/restic",
		IdentityFile: "/etc/cprest/id_ed25519", KnownHostsFile: "/etc/cprest/known_hosts",
	}
	args := destination.DFArgsForTest(sftp)

	end := -1
	host := -1
	for i, arg := range args {
		if arg == "--" && end == -1 {
			end = i
		}
		if arg == "backup.example" {
			host = i
		}
	}
	if end == -1 || host == -1 || end > host {
		t.Fatalf("the host is not behind an end-of-options marker: %v", args)
	}

	// The far end runs the command through a shell, so the path is one
	// word there whatever is in it.
	hostile := &destination.SFTP{
		Host: "backup.example", User: "cprest", Root: "/srv/restic'; rm -rf /tmp/x; '",
		IdentityFile: "/etc/cprest/id_ed25519", KnownHostsFile: "/etc/cprest/known_hosts",
	}
	last := destination.DFArgsForTest(hostile)
	quoted := last[len(last)-1]

	// Asserting on the quoting by eye proves nothing; a shell is the thing
	// that reads it, so a shell is asked. It must come back as one word,
	// byte for byte the path, with nothing run.
	echoed, err := exec.Command("/bin/sh", "-c", "printf %s "+quoted).Output()
	if err != nil {
		t.Fatalf("shell could not read the quoted path %q: %v", quoted, err)
	}
	if string(echoed) != hostile.Root {
		t.Errorf("the shell read %q, want %q (from %s)", echoed, hostile.Root, quoted)
	}
}

// The same values reach restic's argument list and a remote shell, so they
// are refused before they are stored, not only when they are used.
func TestASFTPDestinationRefusesAHostThatIsAnOption(t *testing.T) {
	for _, tc := range []struct {
		name string
		sftp destination.SFTP
	}{
		{"host is an ssh option", destination.SFTP{
			Host: "-oProxyCommand=/bin/sh", User: "cprest", Root: "/srv",
			IdentityFile: "/k", KnownHostsFile: "/kh"}},
		{"host carries whitespace", destination.SFTP{
			Host: "backup.example -oProxyCommand=x", User: "cprest", Root: "/srv",
			IdentityFile: "/k", KnownHostsFile: "/kh"}},
		{"root carries a newline", destination.SFTP{
			Host: "backup.example", User: "cprest", Root: "/srv\nrm -rf /",
			IdentityFile: "/k", KnownHostsFile: "/kh"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := tc.sftp
			if _, err := dest.URI("host"); err == nil {
				t.Error("it was accepted")
			}
		})
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
