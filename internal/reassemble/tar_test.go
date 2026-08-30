package reassemble

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractTarRefusesPathTraversal(t *testing.T) {
	// The archive comes from the server being restored, which in the
	// threat model this program is built around may be the compromised
	// one. An entry that writes outside the extraction directory must
	// never be honoured.
	for _, name := range []string{
		"../escape.txt",
		"cpmove-c1/../../escape.txt",
		"/etc/cron.d/escape",
	} {
		dir := t.TempDir()
		archive := filepath.Join(dir, "evil.tar")
		writeRawTar(t, archive, []tar.Header{
			{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
		}, []string{"evil"})

		err := extractTar(archive, filepath.Join(dir, "out"))
		if name == "/etc/cron.d/escape" {
			// An absolute path is cleaned into the extraction directory
			// rather than escaping, which is also acceptable.
			if err == nil {
				if _, statErr := os.Stat("/etc/cron.d/escape"); statErr == nil {
					t.Fatal("an absolute entry was written outside the extraction directory")
				}
			}
			continue
		}
		if err == nil {
			t.Errorf("entry %q was extracted instead of refused", name)
		}
	}
}

func TestExtractTarRefusesEscapingSymlink(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "evil.tar")
	writeRawTar(t, archive, []tar.Header{
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../../etc/passwd", Mode: 0o777},
	}, []string{""})

	if err := extractTar(archive, filepath.Join(dir, "out")); err == nil {
		t.Error("a symlink pointing outside the tree was created")
	}
}

func TestExtractTarDropsSetuidBits(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.tar")
	// 0o4755 is setuid root; only the permission bits should survive.
	writeRawTar(t, archive, []tar.Header{
		{Name: "bin/tool", Typeflag: tar.TypeReg, Mode: 0o4755, Size: 2},
	}, []string{"hi"})

	out := filepath.Join(dir, "out")
	if err := extractTar(archive, out); err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	info, err := os.Stat(filepath.Join(out, "bin", "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetuid != 0 {
		t.Errorf("mode = %v, setuid survived extraction", info.Mode())
	}
}

func TestTarRoundTrip(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(source, "cpmove-c1", "homedir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "cpmove-c1", "homedir", "index.html"),
		[]byte("<h1>hi</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(dir, "out.tar")
	if err := createTar(source, archive); err != nil {
		t.Fatalf("createTar: %v", err)
	}
	back := filepath.Join(dir, "back")
	if err := extractTar(archive, back); err != nil {
		t.Fatalf("extractTar: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(back, "cpmove-c1", "homedir", "index.html"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(body) != "<h1>hi</h1>" {
		t.Errorf("round trip changed the content: %q", body)
	}
}

func TestSafeJoin(t *testing.T) {
	root := "/srv/restore"
	for _, name := range []string{"a/b", "./a", "a/../b"} {
		got, err := safeJoin(root, name)
		if err != nil {
			t.Errorf("safeJoin(%q) returned %v", name, err)
			continue
		}
		if !strings.HasPrefix(got, root) {
			t.Errorf("safeJoin(%q) = %q, outside the root", name, got)
		}
	}
	for _, name := range []string{"../x", "a/../../x"} {
		if got, err := safeJoin(root, name); err == nil {
			t.Errorf("safeJoin(%q) = %q, want an error", name, got)
		}
	}
}

func writeRawTar(t *testing.T, path string, headers []tar.Header, bodies []string) {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	writer := tar.NewWriter(out)
	for i, header := range headers {
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg && bodies[i] != "" {
			if _, err := writer.Write([]byte(bodies[i])); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}
