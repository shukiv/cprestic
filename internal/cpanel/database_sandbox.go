package cpanel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// loadIsolatedDatabase never gives a dump an administrative MySQL session.
// --one-database is only a USE filter; the server's grants are the boundary
// against qualified cross-schema SQL, FILE, CREATE USER and global grants.
func (r *Real) loadIsolatedDatabase(ctx context.Context, database string, dump io.Reader) (retErr error) {
	connection, err := r.mysqlConnectionOptions(ctx)
	if err != nil {
		return err
	}
	peer := exec.CommandContext(ctx, r.mysql(), "--batch", "--skip-column-names", "--execute=SELECT SUBSTRING_INDEX(USER(), '@', -1)")
	out, err := peer.Output()
	if err != nil {
		return fmt.Errorf("cpanel: determine database client host: %w", err)
	}
	host := strings.TrimSpace(string(out))
	if !usableDatabaseHost(host) || strings.ContainsAny(host, "%_") {
		return fmt.Errorf("cpanel: could not identify an exact database client host")
	}
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return err
	}
	login := "cpr_restore_" + hex.EncodeToString(entropy[:8])
	password := hex.EncodeToString(entropy[8:])
	dir, err := os.MkdirTemp("", "gniza-mysql-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	defaults := filepath.Join(dir, "client.cnf")
	if err := os.WriteFile(defaults, []byte("[client]\n"+connection+"user="+login+"\npassword="+password+"\n"), 0600); err != nil {
		return err
	}
	principal := fmt.Sprintf("`%s`@`%s`", login, host)
	if err := r.databaseAdminSQL(ctx, "CREATE USER "+principal+" IDENTIFIED BY '"+password+"';\n"); err != nil {
		return fmt.Errorf("cpanel: create isolated database restore login: %w", err)
	}
	defer func() {
		// Cancellation of the restore must not cancel removal of its login.
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := r.databaseAdminSQL(cleanup, "DROP USER "+principal+";\n"); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("cpanel: remove isolated restore login %s: %w", login, err))
		}
	}()
	// Do not allow objects that would retain this disposable login as their
	// DEFINER after cleanup. Nor may a dump install a different definer.
	// Such dumps fail explicitly; they are never retried with root access.
	const privileges = "SELECT, INSERT, UPDATE, DELETE, CREATE, DROP, ALTER, INDEX, REFERENCES, LOCK TABLES, CREATE TEMPORARY TABLES"
	if err := r.databaseAdminSQL(ctx, fmt.Sprintf("GRANT %s ON `%s`.* TO %s;\n", privileges, grantPattern(database), principal)); err != nil {
		return fmt.Errorf("cpanel: confine database restore login: %w", err)
	}
	load := exec.CommandContext(ctx, r.mysql(), "--defaults-file="+defaults,
		"--user="+login, "--binary-mode", "--local-infile=0", "--one-database", database)
	// --defaults-file excludes ordinary option files. MySQL still reads
	// .mylogin.cnf, so redirect that too, without inheriting MYSQL_PWD or
	// client plugin settings. The username is pinned on the command line.
	load.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "MYSQL_TEST_LOGIN_FILE=" + filepath.Join(dir, "no-login-paths")}
	load.Stdin = dump
	var stderr bytes.Buffer
	load.Stderr = &stderr
	if err := load.Run(); err != nil {
		return fmt.Errorf("cpanel: isolated database import failed (server-wide SQL and definer objects are not allowed): %s: %w", lastLine(stderr.Bytes()), err)
	}
	return nil
}

func (r *Real) databaseAdminSQL(ctx context.Context, statement string) error {
	cmd := exec.CommandContext(ctx, r.mysql(), "--binary-mode", "--local-infile=0")
	cmd.Stdin = strings.NewReader(statement)
	// Do not expose stderr: a server error can echo the password-bearing
	// CREATE statement. The exit error identifies failure without secrets.
	return cmd.Run()
}

// Keep the active cPanel database profile's transport/TLS settings, not its
// root password or executable client options. mysql itself resolves its
// option files, including the remote database profile in root's .my.cnf.
func (r *Real) mysqlConnectionOptions(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, r.mysql(), "--print-defaults").Output()
	if err != nil {
		return "", fmt.Errorf("cpanel: read database connection options: %w", err)
	}
	var options strings.Builder
	started := false
	for _, field := range strings.Fields(string(out)) {
		if !strings.HasPrefix(field, "--") {
			if started {
				return "", fmt.Errorf("cpanel: database connection options contain whitespace; cannot safely isolate the import")
			}
			continue
		}
		started = true
		key, value, hasValue := strings.Cut(strings.TrimPrefix(field, "--"), "=")
		switch key {
		case "host", "port", "socket", "protocol", "ssl", "skip-ssl", "ssl-mode", "ssl-ca", "ssl-capath", "ssl-cert", "ssl-key", "ssl-cipher", "ssl-crl", "ssl-crlpath", "ssl-verify-server-cert", "tls-version", "tls-ciphersuites", "default-character-set":
			if !hasValue {
				options.WriteString(key + "\n")
				continue
			}
			// Quote option values and escape the option-file metacharacters.
			value = strings.ReplaceAll(value, "\\", "\\\\")
			value = strings.ReplaceAll(value, "\"", "\\\"")
			fmt.Fprintf(&options, "%s=\"%s\"\n", key, value)
		}
	}
	return options.String(), nil
}
