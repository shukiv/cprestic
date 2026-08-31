package cpanel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shuki/cprest/internal/granular"
	"github.com/shuki/cprest/internal/pkgacct"
)

// Real drives the cPanel tooling installed on the host.
//
// This is the one part of the agent that cannot be exercised without a
// cPanel server. Everything it depends on — flag probing, payload planning,
// staging, restic execution — is tested independently.
type Real struct {
	// PkgacctPath is the pkgacct script. Empty means the standard location.
	PkgacctPath string
	// MysqldumpPath is the dump utility. Empty means "mysqldump" on PATH.
	MysqldumpPath string
	// RestorepkgPath applies an account archive. Empty means the standard
	// location.
	RestorepkgPath string
	// HomeRoot is where account home directories live.
	HomeRoot string
	// UsersDir holds one file per cPanel account. Empty means the standard
	// location.
	UsersDir string
	// MysqlPath is the client used to read database users and their
	// grants. Empty means whatever is on PATH.
	MysqlPath string
	// EasyApachePath is the tool that writes an EasyApache profile.
	// Empty means the standard location.
	EasyApachePath string
}

var _ Provider = (*Real)(nil)

func (r *Real) pkgacct() string {
	if r.PkgacctPath != "" {
		return r.PkgacctPath
	}
	return "/scripts/pkgacct"
}

func (r *Real) mysqldump() string {
	if r.MysqldumpPath != "" {
		return r.MysqldumpPath
	}
	return "mysqldump"
}

func (r *Real) mysql() string {
	if r.MysqlPath != "" {
		return r.MysqlPath
	}
	return "mysql"
}

func (r *Real) homeRoot() string {
	if r.HomeRoot != "" {
		return r.HomeRoot
	}
	return "/home"
}

// Capabilities probes the installed pkgacct rather than assuming flag
// names, which have moved between cPanel versions.
func (r *Real) Capabilities(ctx context.Context) (pkgacct.Capabilities, error) {
	cmd := exec.CommandContext(ctx, r.pkgacct(), "--help")
	// pkgacct --help exits non-zero on some versions; the text is what
	// matters, so only a failure to run at all is fatal.
	output, err := cmd.CombinedOutput()
	if len(output) == 0 && err != nil {
		return pkgacct.Capabilities{}, fmt.Errorf("cpanel: run pkgacct --help: %w", err)
	}
	return pkgacct.ProbeCapabilities(string(output)), nil
}

// Accounts lists the cPanel accounts on this host.
//
// This is deliberately cheap: it reads the account names and their home
// directories and nothing else. Measuring sizes means walking every home
// directory, and listing databases means a MySQL round trip per account —
// on a real server with twenty accounts that was nineteen seconds, for a
// page that only needed the names.
//
// It also never drops an account because some subsystem is unhappy. An
// account whose databases cannot be listed is still an account, and hiding
// it would tell an operator they have nothing to back up.
func (r *Real) Accounts(_ context.Context) ([]AccountInfo, error) {
	entries, err := os.ReadDir(r.usersDir())
	if err != nil {
		return nil, fmt.Errorf("cpanel: list accounts: %w", err)
	}

	var accounts []AccountInfo
	for _, entry := range entries {
		user := entry.Name()
		if entry.IsDir() || strings.HasPrefix(user, ".") || user == "system" {
			continue
		}
		if err := validateUser(user); err != nil {
			continue
		}
		home := filepath.Join(r.homeRoot(), user)
		if info, err := os.Stat(home); err != nil || !info.IsDir() {
			// No home directory means no account to back up, as opposed
			// to an account we merely failed to measure.
			continue
		}
		accounts = append(accounts, AccountInfo{User: user, HomeDir: home})
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].User < accounts[j].User })
	return accounts, nil
}

func (r *Real) usersDir() string {
	if r.UsersDir != "" {
		return r.UsersDir
	}
	return "/var/cpanel/users"
}

// Account reads an account's home directory and database list.
func (r *Real) Account(ctx context.Context, user string) (AccountInfo, error) {
	if err := validateUser(user); err != nil {
		return AccountInfo{}, err
	}
	home := filepath.Join(r.homeRoot(), user)
	info, err := os.Stat(home)
	if err != nil {
		return AccountInfo{}, fmt.Errorf("cpanel: account home %s: %w", home, err)
	}
	if !info.IsDir() {
		return AccountInfo{}, fmt.Errorf("cpanel: account home %s is not a directory", home)
	}

	databases, err := r.databases(ctx, user)
	if err != nil {
		return AccountInfo{}, err
	}
	size, err := directorySize(home)
	if err != nil {
		return AccountInfo{}, err
	}
	return AccountInfo{User: user, HomeDir: home, Databases: databases, SizeBytes: size}, nil
}

// databases lists the account's MySQL databases by the cPanel convention
// that they are prefixed with the account name.
//
// A failure here is returned rather than swallowed. Backing an account up
// while quietly omitting its databases would produce a backup that looks
// fine and restores a site with no content.
func (r *Real) databases(ctx context.Context, user string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "mysql", "--batch", "--skip-column-names",
		"-e", "SHOW DATABASES")
	output, err := cmd.Output()
	if err != nil {
		detail := ""
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			detail = ": " + lastLine(exitErr.Stderr)
		}
		return nil, fmt.Errorf(
			"cpanel: list databases for %s%s (root's MySQL credentials come from "+
				"~/.my.cnf, so the service needs HOME set): %w", user, detail, err)
	}
	var databases []string
	for _, line := range strings.Split(string(output), "\n") {
		name := strings.TrimSpace(line)
		if name != "" && strings.HasPrefix(name, user+"_") {
			databases = append(databases, name)
		}
	}
	return databases, nil
}

// Stage runs pkgacct and, in split mode, dumps each database separately.
func (r *Real) Stage(ctx context.Context, req StageRequest) (pkgacct.Payload, error) {
	caps, err := r.Capabilities(ctx)
	if err != nil {
		return pkgacct.Payload{}, err
	}
	payload, err := pkgacct.Plan(pkgacct.PlanRequest{
		Account:       req.Account.User,
		HomeDir:       req.Account.HomeDir,
		Databases:     req.Account.Databases,
		StagingDir:    req.StagingDir,
		Mode:          req.Mode,
		Caps:          caps,
		SkipHomedir:   req.SkipHomedir,
		SkipDatabases: req.SkipDatabases,
	})
	if err != nil {
		return pkgacct.Payload{}, err
	}

	// pkgacct writes its archive into a directory it is given and does not
	// create it. Without this it wrote nothing, exited zero, and the
	// backup silently lost the account's configuration.
	for _, part := range payload.Parts {
		if part.Kind == pkgacct.PartMetadata {
			if err := os.MkdirAll(part.Path, 0o700); err != nil {
				return pkgacct.Payload{}, fmt.Errorf("cpanel: create %s: %w", part.Path, err)
			}
		}
	}

	args := pkgacct.CommandArgs(req.Account.User, req.StagingDir, req.Mode, caps)
	cmd := exec.CommandContext(ctx, r.pkgacct(), args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return pkgacct.Payload{}, fmt.Errorf("cpanel: pkgacct failed: %w: %s",
			err, lastLine(output))
	}

	if req.Mode == pkgacct.ModeSplit {
		if err := r.dumpDatabases(ctx, req, payload); err != nil {
			return pkgacct.Payload{}, err
		}
	}

	// pkgacct can exit zero having produced nothing useful, so what it
	// left behind is checked rather than assumed.
	if err := payload.Verify(); err != nil {
		return pkgacct.Payload{}, fmt.Errorf("%w (pkgacct said: %s)", err, lastLine(output))
	}
	return payload, nil
}

// dumpDatabaseUsers writes the account's database users and their grants.
//
// pkgacct puts them in the archive it builds — but only when it is also
// dumping the databases, which in split mode it is not. Without this a
// restore brings back every table and nothing that can read them.
func (r *Real) dumpDatabaseUsers(ctx context.Context, req StageRequest, dir string) error {
	account := req.Account.User
	if !plainAccountName(account) {
		return fmt.Errorf("cpanel: %q is not a cPanel account name", account)
	}

	// cPanel prefixes an account's database users with the account name.
	// The account itself is also a database user on most servers.
	list := exec.CommandContext(ctx, r.mysql(), "--batch", "--skip-column-names", "--execute",
		fmt.Sprintf(
			"SELECT CONCAT(QUOTE(user), '@', QUOTE(host)) FROM mysql.user "+
				"WHERE user = '%s' OR user LIKE '%s\\_%%'", account, account))
	users, err := list.Output()
	if err != nil {
		return fmt.Errorf("cpanel: list database users for %s: %w", account, err)
	}

	var script strings.Builder
	script.WriteString("-- Database users and grants for " + account + "\n")
	for _, line := range strings.Split(string(users), "\n") {
		user := strings.TrimSpace(line)
		if user == "" {
			continue
		}
		for _, statement := range []string{"SHOW CREATE USER " + user, "SHOW GRANTS FOR " + user} {
			out, err := exec.CommandContext(ctx, r.mysql(),
				"--batch", "--skip-column-names", "--execute", statement).Output()
			if err != nil {
				// A user that cannot be read is worth saying so about,
				// rather than leaving a gap in the file that looks like
				// an account with no grants.
				return fmt.Errorf("cpanel: %s: %w", statement, err)
			}
			for _, row := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if row = strings.TrimSpace(row); row != "" {
					script.WriteString(row + ";\n")
				}
			}
		}
	}

	path := filepath.Join(dir, granular.DatabaseUsersFile)
	if err := os.WriteFile(path, []byte(script.String()), 0o600); err != nil {
		return fmt.Errorf("cpanel: write database users: %w", err)
	}
	return nil
}

// plainAccountName keeps a name that reaches a SQL statement to what a
// cPanel account name can actually be.
func plainAccountName(name string) bool {
	if name == "" || len(name) > 32 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func (r *Real) dumpDatabases(ctx context.Context, req StageRequest, payload pkgacct.Payload) error {
	if len(payload.DumpPaths) == 0 {
		return nil
	}
	dir := filepath.Join(req.StagingDir, "databases")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cpanel: create database staging: %w", err)
	}
	if err := r.dumpDatabaseUsers(ctx, req, dir); err != nil {
		// A database restored without the user that owns it is a database
		// no site can open, so this is part of the backup, not a bonus.
		return err
	}
	for name, path := range payload.DumpPaths {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("cpanel: create dump %s: %w", path, err)
		}
		// Dumps are written uncompressed on purpose: restic deduplicates
		// plain SQL between nights, and cannot deduplicate gzip at all.
		cmd := exec.CommandContext(ctx, r.mysqldump(),
			"--single-transaction", "--quick", "--routines", "--events", name)
		cmd.Stdout = file
		err = cmd.Run()
		closeErr := file.Close()
		if err != nil {
			return fmt.Errorf("cpanel: mysqldump %s: %w", name, err)
		}
		if closeErr != nil {
			return fmt.Errorf("cpanel: close dump %s: %w", path, closeErr)
		}
	}
	return nil
}

// restorepkg returns the script that applies an account archive.
func (r *Real) restorepkg() string {
	if r.RestorepkgPath != "" {
		return r.RestorepkgPath
	}
	return "/scripts/restorepkg"
}

// Apply hands a rebuilt archive to cPanel.
//
// This overwrites the live account, so the agent only reaches it when an
// operator set apply on the restore job. Like the rest of this file it has
// never been run against a cPanel host.
func (r *Real) Apply(ctx context.Context, archivePath string) error {
	info, err := os.Stat(archivePath)
	if err != nil {
		return fmt.Errorf("cpanel: restore archive: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("cpanel: restore archive %s is a directory", archivePath)
	}

	cmd := exec.CommandContext(ctx, r.restorepkg(), archivePath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cpanel: restorepkg failed: %w: %s", err, lastLine(output))
	}
	return nil
}

func validateUser(user string) error {
	if user == "" {
		return fmt.Errorf("cpanel: account user is empty")
	}
	for _, r := range user {
		isAllowed := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !isAllowed {
			return fmt.Errorf("cpanel: account user %q contains an unsupported character", user)
		}
	}
	return nil
}

func directorySize(root string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			// A file that vanished mid-walk should not fail an estimate.
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		total += uint64(info.Size())
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("cpanel: measure %s: %w", root, err)
	}
	return total, nil
}

func lastLine(output []byte) string {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 {
		return "(no output)"
	}
	return strings.TrimSpace(lines[len(lines)-1])
}
