package cpanel

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	// HomeRoot is where account home directories live.
	HomeRoot string
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
func (r *Real) databases(ctx context.Context, user string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "mysql", "--batch", "--skip-column-names",
		"-e", "SHOW DATABASES")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("cpanel: list databases: %w", err)
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
	dir := filepath.Join(req.StagingDir, "databases")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cpanel: create database staging: %w", err)
	}
	for _, part := range payload.Parts {
		if part.Kind != pkgacct.PartDatabase {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(part.Path), ".sql")
		file, err := os.OpenFile(part.Path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("cpanel: create dump %s: %w", part.Path, err)
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
			return fmt.Errorf("cpanel: close dump %s: %w", part.Path, closeErr)
		}
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
