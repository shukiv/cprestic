package destination

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// SFTP is a repository reached over SSH. No cprest software runs on the
// far end; restic drives the system ssh client.
type SFTP struct {
	Host string
	// Port is the SSH port. Zero means the default (22) and produces the
	// short "sftp:user@host:/path" URI form.
	Port int
	User string
	// Root is the absolute remote directory holding the repositories.
	Root string
	// IdentityFile is the path to the private key on the agent. The key is
	// written by the agent from the credential vault at mode 0600 and
	// removed when the job ends.
	IdentityFile string
	// KnownHostsFile pins the server's host key. Empty means the ssh
	// client's default, which Preflight rejects: an unpinned host key
	// turns a DNS or routing compromise into a credential disclosure.
	KnownHostsFile string
}

var _ Destination = (*SFTP)(nil)

func (s *SFTP) Type() Type { return TypeSFTP }

func (s *SFTP) URI(repoPath string) (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	cleaned, err := CleanRepoPath(repoPath)
	if err != nil {
		return "", err
	}
	root := strings.TrimSuffix(s.Root, "/")
	if s.Port == 0 || s.Port == 22 {
		return fmt.Sprintf("sftp:%s@%s:%s/%s", s.User, s.hostForURI(), root, cleaned), nil
	}
	// The URL form needs a double slash to mark the path as absolute:
	// sftp://user@host:2222//srv/restic-repo
	return fmt.Sprintf("sftp://%s@%s:%d/%s/%s",
		s.User, s.hostForURI(), s.Port, root, cleaned), nil
}

// hostForURI bracket-wraps IPv6 literals, which restic's URL form requires.
func (s *SFTP) hostForURI() string {
	if ip := net.ParseIP(s.Host); ip != nil && ip.To4() == nil {
		return "[" + s.Host + "]"
	}
	return s.Host
}

// Env returns no variables. SSH authentication is configured through the
// identity and known-hosts files, which reach ssh via Options.
func (s *SFTP) Env() (map[string]string, error) { return map[string]string{}, nil }

// Options returns restic's sftp.args, which restic injects into the ssh
// command it builds:
//
//	ssh <host> [-p <port>] [-l <user>] <sftp.args...> -s sftp
//
// Without this the agent would fall back to root's ssh configuration:
// the wrong key, an unpinned host key, and an interactive prompt that
// hangs an unattended backup forever.
//
// File paths are not secrets, so unlike credentials they are safe in the
// argument list.
func (s *SFTP) Options() (map[string]string, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if s.KnownHostsFile == "" {
		return nil, fmt.Errorf("sftp: known-hosts file is required; refusing to trust an unpinned host key")
	}
	args := []string{
		"-i", s.IdentityFile,
		"-o", "UserKnownHostsFile=" + s.KnownHostsFile,
		"-o", "StrictHostKeyChecking=yes",
		// Never prompt: an unattended agent that is asked for a password
		// or a host-key confirmation would block until the job times out.
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
	}
	return map[string]string{"sftp.args": strings.Join(args, " ")}, nil
}

func (s *SFTP) Preflight(ctx context.Context) error {
	if err := s.validate(); err != nil {
		return err
	}
	if s.KnownHostsFile == "" {
		return fmt.Errorf("sftp: known-hosts file is required; refusing to trust an unpinned host key")
	}
	for name, path := range map[string]string{
		"identity file":    s.IdentityFile,
		"known-hosts file": s.KnownHostsFile,
	} {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("sftp: %s: %w", name, err)
		}
		if info.IsDir() {
			return fmt.Errorf("sftp: %s %q is a directory", name, path)
		}
	}
	if perm := mustStatMode(s.IdentityFile); perm&0o077 != 0 {
		return fmt.Errorf("sftp: identity file %q is group- or world-accessible (mode %04o)",
			s.IdentityFile, perm)
	}

	port := s.Port
	if port == 0 {
		port = 22
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(s.Host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("sftp: dial: %w", err)
	}
	return conn.Close()
}

func (s *SFTP) validate() error {
	switch {
	case s.Host == "":
		return fmt.Errorf("sftp: host is required")
	case s.User == "":
		return fmt.Errorf("sftp: user is required")
	case !strings.HasPrefix(s.Root, "/"):
		return fmt.Errorf("sftp: root %q must be an absolute path", s.Root)
	case s.Port < 0 || s.Port > 65535:
		return fmt.Errorf("sftp: port %d is out of range", s.Port)
	case s.IdentityFile == "":
		return fmt.Errorf("sftp: identity file is required")
	}
	// restic splits sftp.args with shell-like quoting rules, so a path
	// containing whitespace or quotes would be silently torn into several
	// arguments.
	for name, path := range map[string]string{
		"identity file":    s.IdentityFile,
		"known-hosts file": s.KnownHostsFile,
	} {
		if strings.ContainsAny(path, " \t\"'\\") {
			return fmt.Errorf("sftp: %s %q must not contain whitespace, quotes or backslashes", name, path)
		}
	}
	return nil
}

// mustStatMode returns the file's permission bits, or 0 when it cannot be
// stat'ed. Callers stat the file first, so 0 only occurs on a race and is
// treated as "no complaint" rather than a false rejection.
func mustStatMode(path string) os.FileMode {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Mode().Perm()
}
