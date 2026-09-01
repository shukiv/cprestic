package webui

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// peerAccount is the cPanel account on the other end of a unix socket.
//
// The user-facing interface runs as the account it belongs to — cpsrvd
// executes a user's CGI as that user — so the kernel already knows who is
// asking. Reading it from the socket means the browser never gets to say
// who it is, and a request cannot be pointed at somebody else's backups by
// changing a parameter.
func peerAccount(conn net.Conn) (string, error) {
	creds, err := peerCredentials(conn)
	if err != nil {
		return "", err
	}

	// root reaching the user interface is a mistake somewhere, not an
	// account: there is no account whose backups it should show.
	if creds.Uid == 0 {
		return "", fmt.Errorf("webui: this interface is for cPanel accounts, not for root")
	}
	account, err := user.LookupId(strconv.FormatUint(uint64(creds.Uid), 10))
	if err != nil {
		return "", fmt.Errorf("webui: uid %d is not an account on this server", creds.Uid)
	}
	// A system user is not a customer. cPanel keeps one file per account,
	// and that file existing is what makes a name an account rather than
	// any unix user who happens to be able to open a socket.
	if !isCPanelAccount(account.Username) {
		return "", fmt.Errorf("webui: %q is not a cPanel account", account.Username)
	}
	// A suspended account is one the operator has cut off. Its files are
	// still backed up — that is the point of a backup — but the person
	// behind it does not get to keep driving this service, and a
	// suspended account's processes can still be running.
	if isSuspended(account.Username) {
		return "", fmt.Errorf("webui: %q is suspended", account.Username)
	}
	return account.Username, nil
}

// peerCredentials asks the kernel who opened a Unix-socket connection.
// It is also used before HTTP parsing to enforce per-UID connection limits.
func peerCredentials(conn net.Conn) (*unix.Ucred, error) {
	if budgeted, ok := conn.(*budgetedConn); ok {
		conn = budgeted.Conn
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, fmt.Errorf("webui: connection is not a unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("webui: read the connection: %w", err)
	}

	var creds *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		creds, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, fmt.Errorf("webui: read the peer's credentials: %w", err)
	}
	if credErr != nil {
		return nil, fmt.Errorf("webui: read the peer's credentials: %w", credErr)
	}
	return creds, nil
}

// cpanelUsersDir is where cPanel records its accounts. A variable so a
// test can point it somewhere it controls.
var cpanelUsersDir = "/var/cpanel/users"

func isCPanelAccount(name string) bool {
	if !plainName(name) {
		return false
	}
	info, err := os.Stat(filepath.Join(cpanelUsersDir, name))
	return err == nil && info.Mode().IsRegular()
}

// cpanelSuspendedDir is where cPanel marks a suspended account. A variable
// for the same reason as cpanelUsersDir.
var cpanelSuspendedDir = "/var/cpanel/suspended"

// isSuspended reports whether cPanel has this account suspended. cPanel
// records it two ways depending on version and on what suspended it, so
// both are checked: an account that is suspended by only one of them is
// still suspended.
func isSuspended(name string) bool {
	if !plainName(name) {
		// Not a name this program will act on either way.
		return true
	}
	if _, err := os.Stat(filepath.Join(cpanelSuspendedDir, name)); err == nil {
		return true
	}
	raw, err := os.ReadFile(filepath.Join(cpanelUsersDir, name))
	if err != nil {
		// The account file is what made this a cPanel account a moment
		// ago. If it cannot be read now, do not assume the account is in
		// good standing.
		return true
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "SUSPENDED=1" {
			return true
		}
	}
	return false
}

// plainName keeps a name that is about to be joined onto a path to
// something that cannot climb out of it.
func plainName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "/.")
}
