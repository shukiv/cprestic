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
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return "", fmt.Errorf("webui: connection is not a unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return "", fmt.Errorf("webui: read the connection: %w", err)
	}

	var creds *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		creds, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return "", fmt.Errorf("webui: read the peer's credentials: %w", err)
	}
	if credErr != nil {
		return "", fmt.Errorf("webui: read the peer's credentials: %w", credErr)
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
	return account.Username, nil
}

// cpanelUsersDir is where cPanel records its accounts. A variable so a
// test can point it somewhere it controls.
var cpanelUsersDir = "/var/cpanel/users"

func isCPanelAccount(name string) bool {
	if name == "" || strings.ContainsAny(name, "/.") {
		return false
	}
	info, err := os.Stat(filepath.Join(cpanelUsersDir, name))
	return err == nil && info.Mode().IsRegular()
}
