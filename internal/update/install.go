package update

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// releaseKeyPEM is the public half of the key every release is signed
// with. It is compiled in rather than read from disk because a key a
// program reads from a file it could also be handed a different one for,
// and this key is the whole reason downloading a release and running it as
// root is not simply a way of handing this server to whoever can answer
// for github.com.
//
// The same key is written out in packaging/whm/get.sh, which has no
// binary to embed it in. A test here checks the two have not drifted.
//
//go:embed release.pub
var releaseKeyPEM []byte

// The three files a release publishes. The tarball is the plugin; the
// sums are what says which tarball; the signature is what says the sums
// came from whoever holds the release key.
const (
	TarballName = "cprest-plugin-amd64.tar.gz"
	SumsName    = "SHA256SUMS"
	SigName     = "SHA256SUMS.sig"
)

// What is accepted off the network. A release tarball is around 16 MB and
// the other two are a few hundred bytes; these are room to grow, not
// estimates. Nothing here is streamed into anything that would run before
// the whole of it has been checked.
const (
	maxTarball = 128 << 20
	maxText    = 64 << 10
)

// Source is where releases are fetched from.
type Source struct {
	// Client is left nil in production, where a client with a timeout
	// long enough for a 16 MB download is made here.
	Client *http.Client
	// Base is the directory releases live under, without the tag. It is
	// GitHub unless CPREST_RELEASE_BASE says otherwise, which exists so
	// this can be exercised against a server on the machine itself: the
	// signature is checked against the compiled-in key either way, so an
	// address that points somewhere else can still only deliver what the
	// release key signed.
	Base string
	// Key is the public key signatures are checked against. Nil means the
	// release key, which is what production uses.
	Key *ecdsa.PublicKey
}

// DefaultSource reads releases from GitHub, or from wherever
// CPREST_RELEASE_BASE says.
func DefaultSource(repo string) Source {
	if repo == "" {
		repo = Repo
	}
	base := strings.TrimSpace(os.Getenv("CPREST_RELEASE_BASE"))
	if base == "" {
		base = "https://github.com/" + repo + "/releases/download"
	}
	return Source{Base: base}
}

// ReleaseKey is the key releases are signed with.
func ReleaseKey() (*ecdsa.PublicKey, error) { return parseKey(releaseKeyPEM) }

func parseKey(body []byte) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, errors.New("update: the release key is not PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("update: read the release key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("update: the release key is a %T, not an ECDSA key", parsed)
	}
	return key, nil
}

// Fetch downloads a release into dir and returns the path of the tarball,
// having checked that the release key signed the checksums and that the
// checksums describe what arrived.
//
// Nothing is unpacked here and nothing is run. What comes back is a file
// this server has decided it can believe.
func (s Source) Fetch(ctx context.Context, version, dir string) (string, error) {
	if !IsRelease(version) {
		return "", fmt.Errorf("update: %q is not a release version", version)
	}
	key := s.Key
	if key == nil {
		release, err := ReleaseKey()
		if err != nil {
			return "", err
		}
		key = release
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	sums, err := s.get(ctx, version, SumsName, maxText)
	if err != nil {
		return "", err
	}
	signature, err := s.get(ctx, version, SigName, maxText)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(sums)
	if !ecdsa.VerifyASN1(key, digest[:], signature) {
		// Everything after this point is a file this server runs as root,
		// so this is the end of the road rather than a warning.
		return "", fmt.Errorf(
			"update: the checksums of %s are not signed by the cP:Restic release key", version)
	}
	// The signature says these checksums were published by whoever holds
	// the release key. The version inside them says which release they
	// were published for. Without that second check, anybody who can make
	// a tag -- which is not the same as holding the key -- could publish
	// last year's signed release again under this year's number, and this
	// server would install it believing it had gone forward.
	if signedFor := versionIn(sums); signedFor != version {
		return "", fmt.Errorf(
			"update: those checksums are signed for %s, not %s", describe(signedFor), version)
	}
	want, err := sumFor(sums, TarballName)
	if err != nil {
		return "", err
	}

	tarball := filepath.Join(dir, TarballName)
	got, err := s.download(ctx, version, TarballName, tarball)
	if err != nil {
		return "", err
	}
	if got != want {
		_ = os.Remove(tarball)
		return "", fmt.Errorf("update: %s does not match the signed checksum", TarballName)
	}
	return tarball, nil
}

// IsRelease says whether a version is a published release rather than a
// build of somebody's own. Only these are ever fetched, so a version
// number can never put anything of its own in a URL, and nothing with
// space around it is one: this is a whole answer on its own, not a check
// that happens to be followed by another.
func IsRelease(version string) bool {
	_, ok := parse(version)
	return ok && strings.HasPrefix(version, "v") && version == strings.TrimSpace(version)
}

// versionIn reads the release a set of checksums was published for, which
// the build writes as its first line: "# cprest v1.2.3".
func versionIn(sums []byte) string {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "#" {
			continue
		}
		if len(fields) == 3 && fields[1] == "cprest" {
			return fields[2]
		}
	}
	return ""
}

// describe names what a set of checksums says it is, for the one error
// that has to distinguish "signed for another release" from "signed for
// nothing in particular".
func describe(version string) string {
	if version == "" {
		return "no particular release"
	}
	return version
}

// get reads one small file of a release into memory.
func (s Source) get(ctx context.Context, version, name string, limit int64) ([]byte, error) {
	response, err := s.open(ctx, version, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	return io.ReadAll(io.LimitReader(response.Body, limit))
}

// download writes one file of a release to disk and returns its checksum,
// which is taken as it is written rather than by reading the file back:
// what is checked is then what was received.
func (s Source) download(ctx context.Context, version, name, path string) (string, error) {
	response, err := s.open(ctx, version, name)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, sum), io.LimitReader(response.Body, maxTarball+1))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if written > maxTarball {
		_ = os.Remove(path)
		return "", fmt.Errorf("update: %s is larger than %d bytes", name, int64(maxTarball))
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func (s Source) open(ctx context.Context, version, name string) (*http.Response, error) {
	base := s.Base
	if base == "" {
		base = DefaultSource("").Base
	}
	address, err := url.JoinPath(base, version, name)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "cprest")

	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, fmt.Errorf("update: %s of %s: %s", name, version, response.Status)
	}
	return response, nil
}

// sumFor reads one entry out of a sha256sum file.
func sumFor(sums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// sha256sum writes " " or " *" before the name.
		if strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		if len(fields[0]) != sha256.Size*2 {
			return "", fmt.Errorf("update: the checksum of %s is not a sha256", name)
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return "", fmt.Errorf("update: the checksum of %s is not hexadecimal", name)
		}
		return strings.ToLower(fields[0]), nil
	}
	return "", fmt.Errorf("update: the signed checksums do not mention %s", name)
}
