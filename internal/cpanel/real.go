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
		Account:    req.Account.User,
		HomeDir:    req.Account.HomeDir,
		Databases:  req.Account.Databases,
		StagingDir: req.StagingDir,
		Mode:       req.Mode,
		Caps:       caps,
	})
	if err != nil {
		return pkgacct.Payload{}, err
	}

	args := pkgacct.CommandArgs(req.Account.User, req.StagingDir, req.Mode, caps)
	cmd := exec.CommandContext(ctx, r.pkgacct(), args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return pkgacct.Payload{}, fmt.Errorf("cpanel: pkgacct failed: %w: %s",
			err, lastLine(output))
	}

	if req.Mode == pkgacct.ModeSplit {
		if err := r.dumpDatabases(ctx, req, payload); err != nil {
			return pkgacct.Payload{}, err
		}
	}
	return payload, nil
}

func (r *Real) dumpDatabases(ctx context.Context, req StageRequest, payload pkgacct.Payload) error {
	if len(payload.DumpPaths) == 0 {
		return nil
	}
	dir := filepath.Join(req.StagingDir, "databases")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cpanel: create database staging: %w", err)
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
