// Package sshkeys generates and installs the SSH credentials an SFTP
// destination needs.
//
// Backing up to another Linux server should not require an operator to run
// ssh-keygen, edit authorized_keys and hand-write a known_hosts file before
// anything works. cprest generates its own key per destination, can show
// the public half for copying, and can install it itself given the remote
// password once.
package sshkeys

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// KeyPair is a freshly generated SSH key.
type KeyPair struct {
	// PrivatePEM is in OpenSSH's own format, which is what the ssh client
	// restic drives expects.
	PrivatePEM []byte
	// AuthorizedKey is the single line to append to the remote account's
	// ~/.ssh/authorized_keys.
	AuthorizedKey string
	// Fingerprint identifies the key for an operator comparing it against
	// what the remote server ended up with.
	Fingerprint string
}

// Generate creates an ed25519 key. ed25519 because it is small enough to
// paste in one line and every current OpenSSH accepts it.
func Generate(comment string) (KeyPair, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, fmt.Errorf("sshkeys: generate: %w", err)
	}

	block, err := ssh.MarshalPrivateKey(private, comment)
	if err != nil {
		return KeyPair{}, fmt.Errorf("sshkeys: encode private key: %w", err)
	}
	signerPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		return KeyPair{}, fmt.Errorf("sshkeys: encode public key: %w", err)
	}

	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signerPublic)))
	if comment != "" {
		authorized += " " + comment
	}
	return KeyPair{
		PrivatePEM:    pem.EncodeToMemory(block),
		AuthorizedKey: authorized,
		Fingerprint:   ssh.FingerprintSHA256(signerPublic),
	}, nil
}

// WritePrivateKey saves the private key where only root can read it, which
// is also what the ssh client insists on.
func WritePrivateKey(dir, name string, pair KeyPair) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("sshkeys: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pair.PrivatePEM, 0o600); err != nil {
		return "", fmt.Errorf("sshkeys: write %s: %w", path, err)
	}
	return path, nil
}

// HostKey is a remote server's identity, as offered during the handshake.
type HostKey struct {
	// Line is the known_hosts entry.
	Line string
	// Fingerprint is what an operator compares against the remote server's
	// own "ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub".
	Fingerprint string
	Type        string
}

// FetchHostKey opens a connection far enough to read the server's host key.
//
// No credentials are needed for this: the host proves its identity before
// the client proves anything. The operator still has to decide whether the
// fingerprint is the right one, which is why it is shown rather than
// silently trusted.
func FetchHostKey(address string, timeout time.Duration) (HostKey, error) {
	var captured ssh.PublicKey

	config := &ssh.ClientConfig{
		User:    "cprest-hostkey-probe",
		Auth:    []ssh.AuthMethod{},
		Timeout: timeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			captured = key
			// Stop here: we came for the host key, not to log in.
			return errHostKeyCaptured
		},
	}

	client, err := ssh.Dial("tcp", address, config)
	if client != nil {
		_ = client.Close()
	}
	if captured == nil {
		return HostKey{}, fmt.Errorf("sshkeys: read host key from %s: %w", address, err)
	}
	if err != nil && !errors.Is(err, errHostKeyCaptured) &&
		!strings.Contains(err.Error(), errHostKeyCaptured.Error()) {
		// Any other failure means we never got a trustworthy answer.
		return HostKey{}, fmt.Errorf("sshkeys: read host key from %s: %w", address, err)
	}

	host, _, splitErr := net.SplitHostPort(address)
	if splitErr != nil {
		host = address
	}
	entry := knownHostsHost(host, address)
	return HostKey{
		Line:        strings.TrimSpace(string(ssh.MarshalAuthorizedKey(captured))),
		Fingerprint: ssh.FingerprintSHA256(captured),
		Type:        captured.Type(),
	}.withHost(entry), nil
}

// withHost prefixes the key material with the host pattern known_hosts
// expects.
func (h HostKey) withHost(host string) HostKey {
	h.Line = host + " " + h.Line
	return h
}

// knownHostsHost renders the host pattern, which carries the port in
// brackets when it is not 22.
func knownHostsHost(host, address string) string {
	_, port, err := net.SplitHostPort(address)
	if err != nil || port == "22" {
		return host
	}
	return "[" + host + "]:" + port
}

// WriteKnownHosts saves host key entries, replacing any previous entry for
// the same host so a rebuilt server does not leave a stale one behind.
func WriteKnownHosts(path string, keys ...HostKey) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("sshkeys: create %s: %w", filepath.Dir(path), err)
	}
	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key.Line)
		builder.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		return fmt.Errorf("sshkeys: write %s: %w", path, err)
	}
	return nil
}

var errHostKeyCaptured = fmt.Errorf("sshkeys: host key captured")

// InstallAuthorizedKey adds our public key to the remote account, using the
// operator's password once.
//
// The password is used for this connection and nothing else: it is never
// written to the state file, and every backup afterwards authenticates with
// the key. The host key is pinned to what was fetched beforehand, so this
// cannot be talked to by whatever answers the address in the meantime.
func InstallAuthorizedKey(address, user, password string, host HostKey, authorizedKey string, timeout time.Duration) error {
	expected, _, _, _, err := ssh.ParseAuthorizedKey([]byte(stripHostPattern(host.Line)))
	if err != nil {
		return fmt.Errorf("sshkeys: parse host key: %w", err)
	}

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		Timeout:         timeout,
		HostKeyCallback: ssh.FixedHostKey(expected),
	}
	client, err := ssh.Dial("tcp", address, config)
	if err != nil {
		return fmt.Errorf("sshkeys: log in to %s as %s: %w", address, user, err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("sshkeys: open session: %w", err)
	}
	defer session.Close()

	// Appending only when absent keeps this safe to run again, and the
	// permissions are the ones sshd insists on before it will read the
	// file at all.
	script := fmt.Sprintf(
		`set -e
mkdir -p ~/.ssh
chmod 700 ~/.ssh
touch ~/.ssh/authorized_keys
chmod 600 ~/.ssh/authorized_keys
grep -qxF %s ~/.ssh/authorized_keys || printf '%%s\n' %s >> ~/.ssh/authorized_keys`,
		shellQuote(authorizedKey), shellQuote(authorizedKey))

	if output, err := session.CombinedOutput(script); err != nil {
		return fmt.Errorf("sshkeys: install key on %s: %w: %s",
			address, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// VerifyKeyLogin checks that the key we installed actually works, so a
// destination is never saved as ready when the first backup would fail.
func VerifyKeyLogin(address, user string, privatePEM []byte, host HostKey, timeout time.Duration) error {
	signer, err := ssh.ParsePrivateKey(privatePEM)
	if err != nil {
		return fmt.Errorf("sshkeys: parse our own private key: %w", err)
	}
	expected, _, _, _, err := ssh.ParseAuthorizedKey([]byte(stripHostPattern(host.Line)))
	if err != nil {
		return fmt.Errorf("sshkeys: parse host key: %w", err)
	}

	client, err := ssh.Dial("tcp", address, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		Timeout:         timeout,
		HostKeyCallback: ssh.FixedHostKey(expected),
	})
	if err != nil {
		return fmt.Errorf("sshkeys: log in to %s as %s with the key: %w", address, user, err)
	}
	return client.Close()
}

// EnsureRemoteDir creates the backup directory on the far side, so an
// operator does not have to.
func EnsureRemoteDir(address, user string, privatePEM []byte, host HostKey, dir string, timeout time.Duration) error {
	signer, err := ssh.ParsePrivateKey(privatePEM)
	if err != nil {
		return fmt.Errorf("sshkeys: parse our own private key: %w", err)
	}
	expected, _, _, _, err := ssh.ParseAuthorizedKey([]byte(stripHostPattern(host.Line)))
	if err != nil {
		return fmt.Errorf("sshkeys: parse host key: %w", err)
	}

	client, err := ssh.Dial("tcp", address, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		Timeout:         timeout,
		HostKeyCallback: ssh.FixedHostKey(expected),
	})
	if err != nil {
		return fmt.Errorf("sshkeys: connect to %s: %w", address, err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("sshkeys: open session: %w", err)
	}
	defer session.Close()

	if output, err := session.CombinedOutput(
		"mkdir -p " + shellQuote(dir) + " && chmod 700 " + shellQuote(dir)); err != nil {
		return fmt.Errorf("sshkeys: create %s on %s: %w: %s",
			dir, address, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// stripHostPattern removes the leading host field from a known_hosts line,
// leaving the key itself.
func stripHostPattern(line string) string {
	fields := strings.Fields(line)
	if len(fields) >= 3 {
		return strings.Join(fields[1:], " ")
	}
	return line
}

// shellQuote wraps a value in single quotes for a remote shell. Public keys
// and paths reach the far side this way, and neither is trusted input.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// PublicKeyFromFile derives the authorized_keys line from a stored private
// key, so the interface can show it again after the destination was saved.
func PublicKeyFromFile(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("sshkeys: read %s: %w", path, err)
	}
	signer, err := ssh.ParsePrivateKey(body)
	if err != nil {
		return "", fmt.Errorf("sshkeys: parse %s: %w", path, err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), nil
}
