package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// release is a published release as a test can serve one: the three files,
// signed with a key the test holds.
type release struct {
	files map[string][]byte
	key   *ecdsa.PrivateKey
}

func newRelease(t *testing.T, tarball []byte) *release {
	t.Helper()
	return newReleaseFor(t, tarball, "v9.9.9")
}

// newReleaseFor publishes a release whose checksums say which release they
// belong to, as the build writes them.
func newReleaseFor(t *testing.T, tarball []byte, version string) *release {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(tarball)
	sums := []byte("# cprest " + version + "\n" +
		hex.EncodeToString(sum[:]) + "  " + TarballName + "\nabc  get.sh\n")
	digest := sha256.Sum256(sums)
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return &release{
		key: key,
		files: map[string][]byte{
			TarballName: tarball,
			SumsName:    sums,
			SigName:     signature,
		},
	}
}

func (r *release) serve(t *testing.T) Source {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, found := r.files[filepath.Base(req.URL.Path)]
		if !found {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return Source{Client: server.Client(), Base: server.URL, Key: &r.key.PublicKey}
}

// plugin is a tarball shaped like a release: one top directory with the
// installer in it.
func plugin(t *testing.T, entries ...*tar.Header) []byte {
	t.Helper()
	var buffer bytes.Buffer
	zipped := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(zipped)
	write := func(name, body string) {
		if err := archive.WriteHeader(&tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write("cprest-plugin/install.sh", "#!/bin/sh\nexit 0\n")
	write("cprest-plugin/cprest-agent", "not really a binary")
	for _, header := range entries {
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestFetchTakesASignedRelease(t *testing.T) {
	published := newRelease(t, plugin(t))
	source := published.serve(t)
	dir := t.TempDir()

	tarball, err := source.Fetch(context.Background(), "v9.9.9", dir)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	body, err := os.ReadFile(tarball)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, published.files[TarballName]) {
		t.Error("what was written is not what was served")
	}

	into := filepath.Join(dir, "unpacked")
	if err := Unpack(tarball, into); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if _, err := os.Stat(filepath.Join(into, "cprest-plugin", "install.sh")); err != nil {
		t.Errorf("the installer is not there: %v", err)
	}
}

// TestFetchRefusesWhatTheKeyDidNotSign is the test this whole package
// exists for: everything it downloads is run as root.
func TestFetchRefusesWhatTheKeyDidNotSign(t *testing.T) {
	broken := map[string]func(*release){
		"a signature from another key": func(r *release) {
			other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(r.files[SumsName])
			signature, err := ecdsa.SignASN1(rand.Reader, other, digest[:])
			if err != nil {
				t.Fatal(err)
			}
			r.files[SigName] = signature
		},
		"checksums changed after signing": func(r *release) {
			r.files[SumsName] = append([]byte("# added after signing\n"), r.files[SumsName]...)
		},
		"no signature published": func(r *release) { delete(r.files, SigName) },
		// Making a tag is not holding the key. Publishing an old release
		// again under a new number is the one attack a signature alone
		// does not stop.
		"an older release published again under this number": func(r *release) {
			old := newReleaseFor(t, plugin(t), "v0.0.1")
			old.key = r.key
			resigned := newReleaseFor(t, old.files[TarballName], "v0.0.1")
			resigned.key = r.key
			digest := sha256.Sum256(resigned.files[SumsName])
			signature, err := ecdsa.SignASN1(rand.Reader, r.key, digest[:])
			if err != nil {
				t.Fatal(err)
			}
			r.files[SumsName] = resigned.files[SumsName]
			r.files[SigName] = signature
			r.files[TarballName] = resigned.files[TarballName]
		},
		"checksums that say no release at all": func(r *release) {
			sums := []byte(strings.SplitN(string(r.files[SumsName]), "\n", 2)[1])
			digest := sha256.Sum256(sums)
			signature, err := ecdsa.SignASN1(rand.Reader, r.key, digest[:])
			if err != nil {
				t.Fatal(err)
			}
			r.files[SumsName], r.files[SigName] = sums, signature
		},
		"a tarball that is not the one signed for": func(r *release) {
			r.files[TarballName] = plugin(t, &tar.Header{
				Name: "cprest-plugin/extra", Mode: 0o644, Typeflag: tar.TypeReg,
			})
		},
		"nothing about the tarball in the checksums": func(r *release) {
			lines := strings.SplitN(string(r.files[SumsName]), "\n", 3)
			sums := []byte(lines[0] + "\n" + lines[2])
			digest := sha256.Sum256(sums)
			signature, err := ecdsa.SignASN1(rand.Reader, r.key, digest[:])
			if err != nil {
				t.Fatal(err)
			}
			r.files[SumsName], r.files[SigName] = sums, signature
		},
	}
	for what, breakIt := range broken {
		t.Run(what, func(t *testing.T) {
			published := newRelease(t, plugin(t))
			breakIt(published)
			source := published.serve(t)
			dir := t.TempDir()

			if _, err := source.Fetch(context.Background(), "v9.9.9", dir); err == nil {
				t.Fatal("a release that should not have been believed was accepted")
			}
			// Nothing that failed a check is left where an installer
			// could be pointed at it.
			if _, err := os.Stat(filepath.Join(dir, TarballName)); err == nil {
				t.Error("the tarball was kept")
			}
		})
	}
}

func TestFetchOnlyTakesReleaseVersions(t *testing.T) {
	published := newRelease(t, plugin(t))
	source := published.serve(t)
	for _, version := range []string{
		"", "dev", "v0.1", "v0.1.0-3-gabc1234-dirty", "../../etc", "v1.0.0/x", "v9.9.9\n", " v9.9.9",
	} {
		if _, err := source.Fetch(context.Background(), version, t.TempDir()); err == nil {
			t.Errorf("%q was fetched", version)
		}
	}
}

func TestUnpackRefusesWhatWritesElsewhere(t *testing.T) {
	refused := map[string]*tar.Header{
		"a path climbing out": {Name: "cprest-plugin/../../evil", Mode: 0o644, Typeflag: tar.TypeReg},
		"an absolute path":    {Name: "/etc/cron.d/evil", Mode: 0o644, Typeflag: tar.TypeReg},
		"a file of its own":   {Name: "elsewhere/evil", Mode: 0o644, Typeflag: tar.TypeReg},
		"a symlink":           {Name: "cprest-plugin/link", Linkname: "/etc/shadow", Typeflag: tar.TypeSymlink},
		"a hard link":         {Name: "cprest-plugin/hard", Linkname: "/etc/shadow", Typeflag: tar.TypeLink},
		"a device":            {Name: "cprest-plugin/disk", Typeflag: tar.TypeChar},
	}
	for what, entry := range refused {
		t.Run(what, func(t *testing.T) {
			dir := t.TempDir()
			tarball := filepath.Join(dir, TarballName)
			if err := os.WriteFile(tarball, plugin(t, entry), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := Unpack(tarball, filepath.Join(dir, "unpacked")); err == nil {
				t.Fatal("the archive was unpacked")
			}
			if _, err := os.Stat(filepath.Join(dir, "evil")); err == nil {
				t.Error("a file was written outside the directory")
			}
		})
	}
}

func TestUnpackNeedsAnInstaller(t *testing.T) {
	var buffer bytes.Buffer
	zipped := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(zipped)
	if err := archive.WriteHeader(&tar.Header{
		Name: "cprest-plugin/readme", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	_ = archive.Close()
	_ = zipped.Close()

	dir := t.TempDir()
	tarball := filepath.Join(dir, TarballName)
	if err := os.WriteFile(tarball, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Unpack(tarball, filepath.Join(dir, "unpacked")); err == nil {
		t.Fatal("an archive with no installer in it was accepted")
	}
}

// TestTheKeyIsTheSameEverywhere guards the one drift that would be found
// only by a server failing to update: get.sh carries the same key written
// out, because a shell script has no binary to embed it in.
func TestTheKeyIsTheSameEverywhere(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "packaging", "whm", "get.sh"))
	if err != nil {
		t.Fatal(err)
	}
	const begin, end = "-----BEGIN PUBLIC KEY-----", "-----END PUBLIC KEY-----"
	from := strings.Index(string(script), begin)
	to := strings.Index(string(script), end)
	if from < 0 || to < 0 {
		t.Fatal("get.sh has no public key in it")
	}
	inScript := strings.TrimSpace(string(script)[from : to+len(end)])
	if inScript != strings.TrimSpace(string(releaseKeyPEM)) {
		t.Errorf("get.sh carries a different release key:\n%s\n---\n%s",
			inScript, strings.TrimSpace(string(releaseKeyPEM)))
	}
	if _, err := ReleaseKey(); err != nil {
		t.Errorf("the embedded release key does not parse: %v", err)
	}
}
