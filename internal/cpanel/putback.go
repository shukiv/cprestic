package cpanel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/shuki/cprest/internal/granular"
)

// PutHomeDir copies a restored tree into an account's home directory.
//
// The tree is in staging, which is root's; the home directory is the
// account's. The copy is therefore split in two: root reads, and the
// account writes. Nothing here runs as root inside the home directory, so
// the files that land there are owned by the account without a chown pass,
// and a link planted in the home directory before the restore can lead
// nowhere the account could not already write.
func (r *Real) PutHomeDir(ctx context.Context, user, from string) error {
	account, err := osuser.Lookup(user)
	if err != nil {
		return fmt.Errorf("cpanel: look up %s: %w", user, err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return fmt.Errorf("cpanel: %s has no numeric uid: %w", user, err)
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return fmt.Errorf("cpanel: %s has no numeric gid: %w", user, err)
	}
	// Not a guard against a hostile caller -- one that reached here has
	// root already -- but against a mistake that would write a customer's
	// backup over a system account's files.
	if uid == 0 {
		return fmt.Errorf("cpanel: %s is not a cPanel account", user)
	}
	home := filepath.Clean(account.HomeDir)
	if home == "" || home == "." || home == "/" || filepath.Dir(home) == home {
		return fmt.Errorf("cpanel: %s has no home directory of its own", user)
	}
	info, err := os.Stat(home)
	if err != nil {
		return fmt.Errorf("cpanel: home directory of %s: %w", user, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cpanel: home directory of %s is not a directory", user)
	}

	if err := dropSetIDBits(from); err != nil {
		return err
	}

	// GNU tar on both ends: the reader keeps the tree's shape and modes,
	// and the writer is the account, so ownership follows from who it is
	// rather than from what the archive claims.
	read := exec.CommandContext(ctx, "tar", "-C", from, "--numeric-owner", "-cf", "-", ".")
	write := exec.CommandContext(ctx, "tar", "-C", home, "-xpf", "-", "--no-same-owner")
	write.SysProcAttr = &syscall.SysProcAttr{
		// Groups is left empty on purpose: without it the child would
		// keep root's supplementary groups while carrying the account's
		// uid, which is more than the account has.
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
	}

	pipeRead, pipeWrite, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("cpanel: pipe: %w", err)
	}
	read.Stdout = pipeWrite
	write.Stdin = pipeRead
	var readErr, writeErr bytes.Buffer
	read.Stderr = &readErr
	write.Stderr = &writeErr

	if err := read.Start(); err != nil {
		pipeRead.Close()
		pipeWrite.Close()
		return fmt.Errorf("cpanel: read the restored tree: %w", err)
	}
	if err := write.Start(); err != nil {
		pipeRead.Close()
		pipeWrite.Close()
		_ = read.Process.Kill()
		_, _ = read.Process.Wait()
		return fmt.Errorf("cpanel: write into %s: %w", home, err)
	}
	// The parent's ends have been handed to the children. Holding them
	// open would keep the writer waiting for an end of file that the
	// finished reader has already stopped producing.
	pipeWrite.Close()
	pipeRead.Close()

	readWait := read.Wait()
	writeWait := write.Wait()
	// The writer's failure is reported first on purpose. A full disk or a
	// quota stops the writer, the reader then finds a closed pipe, and
	// "broken pipe" is not what went wrong.
	if writeWait != nil {
		return fmt.Errorf("cpanel: write into %s as %s: %s: %w",
			home, user, lastLine(writeErr.Bytes()), writeWait)
	}
	if readWait != nil {
		return fmt.Errorf("cpanel: read the restored tree: %s: %w",
			lastLine(readErr.Bytes()), readWait)
	}
	return nil
}

// grantPattern writes a database name as the pattern a GRANT takes.
//
// The database of a GRANT is matched like a LIKE pattern, so the underscore
// every cPanel database name carries means "any character" unless it is
// escaped. Backticks do not do it -- they quote the identifier and leave
// the wildcard meaning intact -- so a grant written without this covers
// every database whose name differs by one character.
func grantPattern(database string) string {
	return strings.ReplaceAll(database, "_", `\_`)
}

// dropSetIDBits clears set-user-ID and set-group-ID from a staged tree
// before it is written into an account.
//
// The account could set those bits itself on its own files, so this is not
// what stops it. It stops a bit that was set when the backup was taken from
// coming back after the reason for removing it -- a compromise, a policy
// change -- has been dealt with.
func dropSetIDBits(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&(os.ModeSetuid|os.ModeSetgid) == 0 {
			return nil
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		// Perm() is the nine permission bits and nothing else, so writing
		// it back is what drops the two.
		return os.Chmod(path, info.Mode().Perm())
	})
}

// LoadDatabase replaces the contents of one of the account's databases with
// a dump taken from a backup.
func (r *Real) LoadDatabase(ctx context.Context, user, database, dumpPath string) error {
	if err := granular.UsableDatabaseName(database); err != nil {
		return err
	}
	// Whose database this is, asked of the server rather than taken from
	// the request. The name in the backup is the name it had when the
	// backup was taken, and a name can have changed hands since.
	owned, err := r.databases(ctx, user)
	if err != nil {
		return err
	}
	found := false
	for _, name := range owned {
		if name == database {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf(
			"cpanel: the database %s does not belong to %s", database, user)
	}

	dump, err := os.Open(dumpPath)
	if err != nil {
		return fmt.Errorf("cpanel: database dump: %w", err)
	}
	defer dump.Close()
	info, err := dump.Stat()
	if err != nil {
		return fmt.Errorf("cpanel: database dump: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cpanel: database dump %s is not a file", dumpPath)
	}

	// --one-database confines the load to the database named here: a
	// statement that switched to another one would be ignored rather than
	// run. The dumps this reads are single-database dumps that never
	// switch, and this keeps that true whatever produced the file.
	load := exec.CommandContext(ctx, r.mysql(), "--one-database", database)
	load.Stdin = dump
	var stderr bytes.Buffer
	load.Stderr = &stderr
	if err := load.Run(); err != nil {
		return fmt.Errorf("cpanel: load %s from %s: %s: %w",
			database, filepath.Base(dumpPath), lastLine(stderr.Bytes()), err)
	}
	return nil
}

// PutDatabaseUsers recreates an account's database users from a backup and
// grants them what they had.
//
// It is three steps because no single interface does all of it. MySQL is
// the only thing that can be given a password as a stored hash, which is
// all a backup has -- cPanel's own API needs the password in the clear, and
// nobody has it. cPanel is the only thing that knows an account owns a
// user, and a user MySQL has but the panel does not is one the customer
// cannot see or change. So: root creates the logins, dbmaptool says whose
// they are, and the grants go through cPanel as the account, which writes
// them into the panel's record as well as into MySQL.
//
// Everything is checked before anything runs. A user another account owns,
// or a grant on a database this account does not have, fails the whole
// request rather than being skipped: a restore that quietly left out the
// grant an application connects with is a restore that reports success and
// leaves the site down.
func (r *Real) PutDatabaseUsers(ctx context.Context, user string, users []DatabaseUser) error {
	if !plainAccountName(user) {
		return fmt.Errorf("cpanel: %q is not a cPanel account name", user)
	}
	if len(users) == 0 {
		return errors.New("cpanel: no database user was named to restore")
	}

	// What this account has now, asked of the server. The backup says
	// what it had when it was taken.
	ownedList, err := r.databases(ctx, user)
	if err != nil {
		return err
	}
	owned := make(map[string]bool, len(ownedList))
	for _, name := range ownedList {
		owned[name] = true
	}
	claimed := r.databaseUserOwners()

	for _, account := range users {
		if !plainDatabaseUser(account.Name) {
			return fmt.Errorf("cpanel: %q is not a database user name", account.Name)
		}
		// A deleted user is the reason this exists, so its absence from
		// this account's record proves nothing. Another account holding
		// it is what has to stop the restore.
		if owner, known := claimed[account.Name]; known && owner != user {
			return fmt.Errorf(
				"cpanel: the database user %s belongs to %s, not to %s",
				account.Name, owner, user)
		}
		if !usableDatabaseHost(account.Host) {
			return fmt.Errorf("cpanel: %q is not a host a database user can exist on",
				account.Host)
		}
		if !usableAuthPlugin(account.Plugin) {
			return fmt.Errorf("cpanel: %q is not an authentication plugin", account.Plugin)
		}
		if !usableAuthHash(account.Hash) {
			return fmt.Errorf("cpanel: the stored password of %s is not readable in this backup",
				account.Name)
		}
		for _, grant := range account.Grants {
			if err := granular.UsableDatabaseName(grant.Database); err != nil {
				return err
			}
			if !owned[grant.Database] {
				return fmt.Errorf(
					"cpanel: the database %s does not belong to %s", grant.Database, user)
			}
			for _, privilege := range grant.Privileges {
				if !usableDatabasePrivilege(privilege) {
					return fmt.Errorf("cpanel: %q is not a database privilege", privilege)
				}
			}
		}
	}

	// The account's own MySQL user is not one of its database users: it
	// is the owner of the record they are kept in. cPanel refuses both
	// halves of the normal path for it -- dbmaptool answers "A DB map
	// Owner cannot own itself", and set_privileges_on_database fails --
	// so it is created and granted directly instead. Every account on a
	// cPanel server has one, so this is the common case rather than an
	// edge.
	var owner, mapped []DatabaseUser
	for _, account := range users {
		if account.Name == user {
			owner = append(owner, account)
			continue
		}
		mapped = append(mapped, account)
	}

	if err := r.createDatabaseUsers(ctx, users); err != nil {
		return err
	}
	if len(mapped) > 0 {
		if err := r.mapDatabaseUsers(ctx, user, mapped); err != nil {
			return err
		}
		if err := r.grantDatabaseUsers(ctx, user, mapped); err != nil {
			return err
		}
	}
	return r.grantOwnerDatabases(ctx, owner)
}

// grantOwnerDatabases gives the account's own MySQL user back what it had,
// as MySQL rather than through cPanel.
//
// cPanel has nowhere to record this one: it is the owner of the record, not
// an entry in it, and its access to the account's own databases is not
// something the panel tracks per database. The grant is still confined to
// databases PutDatabaseUsers has already checked are this account's.
func (r *Real) grantOwnerDatabases(ctx context.Context, owner []DatabaseUser) error {
	var script strings.Builder
	for _, account := range owner {
		for _, grant := range account.Grants {
			privileges := "ALL PRIVILEGES"
			if len(grant.Privileges) > 0 {
				privileges = strings.Join(grant.Privileges, ", ")
			}
			// The database of a GRANT is a pattern, so the underscore
			// every cPanel database name carries has to be escaped or
			// the grant covers every name it matches. Backticks do not
			// do it: they quote the identifier and leave the wildcard
			// meaning intact.
			fmt.Fprintf(&script, "GRANT %s ON `%s`.* TO `%s`@`%s`;\n",
				privileges, grantPattern(grant.Database),
				account.Name, account.Host)
		}
	}
	if script.Len() == 0 {
		return nil
	}
	grant := exec.CommandContext(ctx, r.mysql())
	grant.Stdin = strings.NewReader(script.String())
	var stderr bytes.Buffer
	grant.Stderr = &stderr
	if err := grant.Run(); err != nil {
		return fmt.Errorf("cpanel: grant the account's own database user: %s: %w",
			lastLine(stderr.Bytes()), err)
	}
	return nil
}

// createDatabaseUsers makes the logins exist with the passwords they had.
//
// ALTER follows CREATE because CREATE USER IF NOT EXISTS does nothing at
// all to a user who is already there. Restoring a user whose password was
// changed after the backup has to put the old password back -- that is what
// the customer asked for and what the confirmation they ticked says will
// happen -- and without the ALTER it would silently not.
func (r *Real) createDatabaseUsers(ctx context.Context, users []DatabaseUser) error {
	var script strings.Builder
	for _, account := range users {
		identified := fmt.Sprintf("IDENTIFIED WITH '%s'", account.Plugin)
		if account.Hash != "" {
			identified += " AS 0x" + account.Hash
		}
		fmt.Fprintf(&script, "CREATE USER IF NOT EXISTS `%s`@`%s` %s;\n",
			account.Name, account.Host, identified)
		fmt.Fprintf(&script, "ALTER USER `%s`@`%s` %s;\n",
			account.Name, account.Host, identified)
	}
	create := exec.CommandContext(ctx, r.mysql())
	create.Stdin = strings.NewReader(script.String())
	var stderr bytes.Buffer
	create.Stderr = &stderr
	if err := create.Run(); err != nil {
		return fmt.Errorf("cpanel: create the database users: %s: %w",
			lastLine(stderr.Bytes()), err)
	}
	return nil
}

// mapDatabaseUsers tells cPanel that these users are the account's.
//
// Without it MySQL has users the panel has never heard of: they do not
// appear under MySQL Databases, the customer cannot change their password
// or delete them, and the next thing that rebuilds the account leaves them
// behind.
func (r *Real) mapDatabaseUsers(ctx context.Context, user string, users []DatabaseUser) error {
	names := make([]string, 0, len(users))
	for _, account := range users {
		names = append(names, account.Name)
	}
	sort.Strings(names)
	names = unique(names)

	mapUser := exec.CommandContext(ctx, r.dbMapTool(), user,
		"--type", "mysql", "--dbusers", strings.Join(names, ","))
	// Both streams, whole. dbmaptool reports a name it would not map on
	// stdout and still exits zero, and when it does fail it fails with a
	// Perl stack trace whose last line is the trace rather than the
	// reason.
	output, err := mapUser.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cpanel: record the database users of %s: %s: %w",
			user, strings.TrimSpace(string(output)), err)
	}
	if report := strings.TrimSpace(string(output)); report != "" {
		return fmt.Errorf("cpanel: record the database users of %s: %s", user, report)
	}
	return nil
}

// grantDatabaseUsers gives each user back what it could do, through
// cPanel and as the account.
//
// The grant could be written straight into MySQL, and this deliberately
// does not: cPanel keeps its own record of which user may read which
// database, the panel's interface is drawn from that record rather than
// from MySQL, and running as the account means cPanel refuses a database
// that is not the account's whatever this program believed.
func (r *Real) grantDatabaseUsers(ctx context.Context, user string, users []DatabaseUser) error {
	for _, account := range users {
		for _, grant := range account.Grants {
			privileges := "ALL PRIVILEGES"
			if len(grant.Privileges) > 0 {
				privileges = strings.Join(grant.Privileges, ",")
			}
			set := exec.CommandContext(ctx, r.uapi(),
				"--user="+user, "--output=jsonpretty", "Mysql",
				"set_privileges_on_database",
				"user="+account.Name,
				"database="+grant.Database,
				"privileges="+privileges)
			var stderr bytes.Buffer
			set.Stderr = &stderr
			output, err := set.Output()
			if err != nil {
				return fmt.Errorf("cpanel: grant %s on %s: %s: %w",
					account.Name, grant.Database, lastLine(stderr.Bytes()), err)
			}
			// uapi reports a refusal in its answer and still exits
			// zero, so the exit status is not the check.
			if err := uapiRefusal(output); err != nil {
				return fmt.Errorf("cpanel: grant %s on %s: %w",
					account.Name, grant.Database, err)
			}
		}
	}
	return nil
}

// uapiRefusal reads whether cPanel actually did what it was asked.
func uapiRefusal(output []byte) error {
	var answer struct {
		Result struct {
			Status int      `json:"status"`
			Errors []string `json:"errors"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &answer); err != nil {
		return fmt.Errorf("cpanel answered something this could not read: %w", err)
	}
	if answer.Result.Status == 1 {
		return nil
	}
	if len(answer.Result.Errors) > 0 {
		return errors.New(strings.Join(answer.Result.Errors, "; "))
	}
	return errors.New("cpanel refused it without saying why")
}

func (r *Real) dbMapTool() string {
	if r.DBMapTool != "" {
		return r.DBMapTool
	}
	return "/usr/local/cpanel/bin/dbmaptool"
}

// databaseUserOwners is every database user cPanel attributes to an
// account, and which account that is.
//
// There is no server-wide index of these the way /etc/dbowners indexes
// databases, so the per-account records are read instead. A record that
// cannot be read is left out rather than treated as unowned: this answer
// is used to refuse, and a missing file is not permission.
func (r *Real) databaseUserOwners() map[string]string {
	entries, err := os.ReadDir(r.databasesDir())
	if err != nil {
		return nil
	}
	owners := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		account := strings.TrimSuffix(name, ".json")
		if !plainAccountName(account) {
			// dbindex.db.json and anything else that is not one
			// account's record.
			continue
		}
		_, users, recorded := r.recordedDatabases(account)
		if !recorded {
			continue
		}
		for _, user := range users {
			owners[user] = account
		}
	}
	return owners
}

// usableDatabaseHost holds the host half of a MySQL account to what one
// can be. It reaches a statement the mysql client cannot bind parameters
// for, which is why it is checked rather than escaped.
func usableDatabaseHost(host string) bool {
	if host == "" || len(host) > 255 {
		return false
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == ':', r == '%':
		default:
			return false
		}
	}
	return true
}

// usableAuthPlugin holds the plugin name to the shape MySQL's own are.
func usableAuthPlugin(plugin string) bool {
	if plugin == "" || len(plugin) > 64 {
		return false
	}
	for _, r := range plugin {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// usableAuthHash holds the stored password to hex, which is how it goes
// into the statement: caching_sha2_password's is binary, and carrying it
// as text is what loses it.
func usableAuthHash(hash string) bool {
	if len(hash) > 4096 || len(hash)%2 != 0 {
		return false
	}
	for _, r := range hash {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// usableDatabasePrivilege holds a privilege to a MySQL privilege name:
// capitals and single spaces, as SHOW GRANTS prints them.
func usableDatabasePrivilege(privilege string) bool {
	if privilege == "" || len(privilege) > 64 {
		return false
	}
	if strings.HasPrefix(privilege, " ") || strings.HasSuffix(privilege, " ") {
		return false
	}
	for _, r := range privilege {
		switch {
		case r >= 'A' && r <= 'Z', r == ' ':
		default:
			return false
		}
	}
	return true
}
