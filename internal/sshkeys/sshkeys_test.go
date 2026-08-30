package sshkeys

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGeneratedKeyIsReadableByOpenSSH is the test that matters: restic
// drives the system ssh client, so a key it cannot parse is useless however
// correct it looks to us.
func TestGeneratedKeyIsReadableByOpenSSH(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not installed; skipping")
	}

	pair, err := Generate("cprest@cp01.example.com")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	path, err := WritePrivateKey(t.TempDir(), "id_ed25519", pair)
	if err != nil {
		t.Fatalf("WritePrivateKey: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// ssh refuses to use a key other users can read.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("private key mode %04o, want 0600", perm)
	}

	// -y derives the public key, which only works if OpenSSH could parse
	// the private one.
	derived, err := exec.Command(keygen, "-y", "-f", path).Output()
	if err != nil {
		t.Fatalf("ssh-keygen could not read our private key: %v", err)
	}

	// The line we hand the operator must be the same key OpenSSH derived.
	derivedFields := strings.Fields(string(derived))
	ourFields := strings.Fields(pair.AuthorizedKey)
	if len(derivedFields) < 2 || len(ourFields) < 2 {
		t.Fatalf("unexpected key formats: %q vs %q", derived, pair.AuthorizedKey)
	}
	if derivedFields[0] != ourFields[0] || derivedFields[1] != ourFields[1] {
		t.Errorf("our authorized_keys line does not match OpenSSH's:\n  ours:    %s\n  openssh: %s",
			pair.AuthorizedKey, strings.TrimSpace(string(derived)))
	}
	if !strings.HasSuffix(pair.AuthorizedKey, "cprest@cp01.example.com") {
		t.Errorf("comment missing from %q", pair.AuthorizedKey)
	}

	// And the fingerprint we show must be the one an operator sees when
	// they check the key on the far side.
	fingerprint, err := exec.Command(keygen, "-lf", path).Output()
	if err != nil {
		t.Fatalf("ssh-keygen -lf: %v", err)
	}
	if !strings.Contains(string(fingerprint), pair.Fingerprint) {
		t.Errorf("our fingerprint %q is not in %q", pair.Fingerprint, fingerprint)
	}
}

func TestGenerateIsUniquePerCall(t *testing.T) {
	first, err := Generate("a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Generate("b")
	if err != nil {
		t.Fatal(err)
	}
	// Each destination gets its own key, so revoking one on the far side
	// does not lock cprest out of the others.
	if first.AuthorizedKey == second.AuthorizedKey || first.Fingerprint == second.Fingerprint {
		t.Error("two generated keys were identical")
	}
}

func TestKnownHostsHostCarriesANonDefaultPort(t *testing.T) {
	if got := knownHostsHost("backup.example.com", "backup.example.com:22"); got != "backup.example.com" {
		t.Errorf("port 22 = %q, want a bare host", got)
	}
	// OpenSSH writes a non-default port in brackets, and will not match
	// the entry otherwise.
	if got := knownHostsHost("backup.example.com", "backup.example.com:2222"); got != "[backup.example.com]:2222" {
		t.Errorf("port 2222 = %q", got)
	}
}

func TestWriteKnownHostsIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "known_hosts")
	err := WriteKnownHosts(path, HostKey{Line: "backup.example.com ssh-ed25519 AAAA..."})
	if err != nil {
		t.Fatalf("WriteKnownHosts: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "backup.example.com ssh-ed25519") {
		t.Errorf("known_hosts = %q", body)
	}
	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("known_hosts mode %04o is group- or world-readable", perm)
	}
}

func TestStripHostPattern(t *testing.T) {
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 comment"
	if got := stripHostPattern("host " + key); got != key {
		t.Errorf("got %q, want %q", got, key)
	}
	if got := stripHostPattern("[host]:2222 " + key); got != key {
		t.Errorf("got %q, want %q", got, key)
	}
}

func TestShellQuote(t *testing.T) {
	// The public key and the remote directory both reach a remote shell.
	if got := shellQuote("plain"); got != "'plain'" {
		t.Errorf("got %q", got)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("got %q", got)
	}
	if got := shellQuote("; rm -rf /"); !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
		t.Errorf("got %q, want it fully quoted", got)
	}
}
