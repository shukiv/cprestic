package reassemble

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractionStaysInsideTheDirectory covers what a malicious backup
// would try. The archive comes from another server, and it is unpacked as
// root before cPanel's own restore -- restricted or not -- sees any of it.
func TestExtractionStaysInsideTheDirectory(t *testing.T) {
	outside := t.TempDir()
	marker := filepath.Join(outside, "marker")
	const untouched = "this file belongs to the server"
	if err := os.WriteFile(marker, []byte(untouched), 0o600); err != nil {
		t.Fatal(err)
	}

	archives := map[string][]tar.Header{
		"an absolute link, then a write through it": {
			{Name: "cpmove-c1/dnszones/link", Linkname: outside, Typeflag: tar.TypeSymlink, Mode: 0o777},
			{Name: "cpmove-c1/dnszones/link/marker", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
		},
		"a relative link out, then a write through it": {
			{Name: "cpmove-c1/dnszones/link", Linkname: "../../../../../../../../.." + outside,
				Typeflag: tar.TypeSymlink, Mode: 0o777},
			{Name: "cpmove-c1/dnszones/link/marker", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
		},
		"a name that climbs out": {
			{Name: "cpmove-c1/dnszones/../../../../../../../.." + marker,
				Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
		},
		"an absolute name": {
			{Name: marker, Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
		},
	}
	for what, headers := range archives {
		t.Run(what, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "cpmove-c1.tar")
			bodies := make([]string, len(headers))
			for i, header := range headers {
				if header.Typeflag == tar.TypeReg {
					bodies[i] = "evil"
				}
			}
			writeRawTar(t, archive, headers, bodies)

			// Both ways in: the whole archive, and the selection a
			// granular restore asks for.
			for _, extract := range []func() error{
				func() error { return extractTar(archive, filepath.Join(dir, "all")) },
				func() error {
					_, err := ExtractMembers(archive, filepath.Join(dir, "some"), []string{"dnszones/"})
					return err
				},
			} {
				_ = extract()
				body, err := os.ReadFile(marker)
				if err != nil {
					t.Fatalf("the file outside the directory is gone: %v", err)
				}
				if string(body) != untouched {
					t.Fatalf("a file outside the extraction directory was written: %q", body)
				}
			}
		})
	}
}

// TestExtractionKeepsALinkItCanKeep: the containment must not throw away
// the symlinks an account legitimately has.
func TestExtractionKeepsALinkItCanKeep(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "cpmove-c1.tar")
	writeRawTar(t, archive, []tar.Header{
		{Name: "cpmove-c1/homedir/index.html", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2},
		{Name: "cpmove-c1/homedir/link", Linkname: "index.html", Typeflag: tar.TypeSymlink, Mode: 0o777},
	}, []string{"hi", ""})

	into := filepath.Join(dir, "out")
	if err := extractTar(archive, into); err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	target, err := os.Readlink(filepath.Join(into, "cpmove-c1", "homedir", "link"))
	if err != nil {
		t.Fatalf("the link was not recreated: %v", err)
	}
	if target != "index.html" {
		t.Errorf("link points at %q", target)
	}
	body, err := os.ReadFile(filepath.Join(into, "cpmove-c1", "homedir", "link"))
	if err != nil || strings.TrimSpace(string(body)) != "hi" {
		t.Errorf("reading through the link gave %q, %v", body, err)
	}
}
