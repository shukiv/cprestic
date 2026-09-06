package cpanel

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/shukiv/gniza/internal/pkgacct"
)

// SystemAccount is the name a server's own configuration is backed up
// under. It is not a cPanel account and cannot be one: cPanel usernames
// cannot contain "@", so nothing real can collide with it.
const SystemAccount = "@system"

// systemPaths is the server-level configuration worth carrying to a new
// machine. Everything here is cPanel's or the services it configures; an
// account's own data is backed up as that account.
//
// Paths that do not exist on a given cPanel version are skipped rather
// than failing: this list spans versions, and a missing file is
// information, not an error.
var systemPaths = []string{
	"/etc/wwwacct.conf",              // the server's own identity and defaults
	"/var/cpanel/cpanel.config",      // WHM Tweak Settings
	"/var/cpanel/packages",           // hosting packages
	"/var/cpanel/features",           // feature lists
	"/var/cpanel/resellers",          // resellers and their limits
	"/var/cpanel/mainip",             // the shared IP
	"/etc/ips",                       // the rest of them
	"/var/cpanel/mysql",              // MySQL profiles and remote hosts
	"/etc/my.cnf",                    // and its configuration
	"/etc/exim.conf.localopts",       // exim as WHM configured it
	"/etc/exim.conf.local",           //
	"/etc/named.conf",                // the nameserver
	"/var/cpanel/templates",          // service configuration templates
	"/etc/apache2/conf.d/includes",   // Apache includes WHM manages
	"/etc/cpanel/ea4",                // EasyApache's own configuration
	"/var/cpanel/hulkd",              // cPHulk
	"/var/cpanel/greylist",           // greylisting
	"/var/cpanel/ssl",                // the server's certificates
	"/etc/cpupdate.conf",             // update policy
	"/var/cpanel/nameserverips.yaml", //

	// DNS signing material. This is the one thing here that cannot be
	// regenerated: a zone whose DNSSEC keys are lost has to be
	// unsigned at the registrar before it resolves again, which is a
	// support ticket per domain and an outage until it is done.
	"/var/cpanel/dnssec_keys",
	"/var/named/dnssec-keys",
	"/etc/rndc.key",
	"/etc/named",

	// Who exists on the machine. cPanel rebuilds its own accounts from
	// their archives, but the system users around them -- and the uids
	// the restored homes are owned by -- are only written down here.
	"/etc/passwd",
	"/etc/group",
	"/etc/shadow",

	// Scheduled work that belongs to no account: an account's own crontab
	// travels in its archive, root's does not.
	"/var/spool/cron",
	"/etc/crontab",
	"/etc/cron.d",

	// Where mail for a domain is delivered. cPanel can regenerate these
	// from account data, but a replacement server that has them is
	// delivering mail correctly before it has finished restoring.
	"/etc/localdomains",
	"/etc/remotedomains",
	"/etc/userdomains",

	// The rest of the server's own configuration.
	"/var/cpanel/conf",               // service configuration
	"/var/cpanel/authn",              // external authentication links
	"/var/cpanel/apps",               // AppConfig registrations
	"/var/cpanel/roles",              // which roles this server runs
	"/var/cpanel/version",            // which cPanel this was
	"/etc/cpanel_exim_system_filter", // the exim system filter
	"/etc/proftpd.conf",              // whichever ftp server is installed
	"/etc/pure-ftpd.conf",            //
}

// SystemNotCarried is what a system backup deliberately leaves out, so
// the interface can say so rather than implying the archive is a whole
// machine.
//
// Everything here is either restored as part of an account, reinstalled
// by cPanel itself, or -- in one case -- the key to the backups, which
// cannot be kept inside them.
var SystemNotCarried = []string{
	"Anything that belongs to an account: its home directory, databases, " +
		"DNS zones, mail and settings all travel in that account's own backup.",
	"cPanel's own installed files. A replacement server installs cPanel first; " +
		"this restores what was configured on top of it.",
	"Third-party software installed outside cPanel, and anything under /usr or /opt.",
	"gniza's own configuration in /etc/gniza, including the key that decrypts " +
		"these backups. A backup that contained the key to itself would protect nothing.",
}

// StageSystem copies the server's own configuration into the staging
// directory, alongside what EasyApache needs to be rebuilt.
//
// It is a copy rather than an archive so restic sees files it can
// deduplicate: a server's configuration changes a little at a time, and
// most nights it will store almost nothing.
func (r *Real) StageSystem(ctx context.Context, stagingDir string) (pkgacct.Payload, error) {
	root := filepath.Join(stagingDir, "system")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return pkgacct.Payload{}, fmt.Errorf("cpanel: create %s: %w", root, err)
	}

	var copied int
	for _, path := range systemPaths {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			// This cPanel version does not have it. Recorded below, so a
			// restore can tell "not present" from "not backed up".
			continue
		}
		if err != nil {
			return pkgacct.Payload{}, fmt.Errorf("cpanel: read %s: %w", path, err)
		}
		target := filepath.Join(root, "files", strings.TrimPrefix(path, "/"))
		if err := copyPath(path, target, info); err != nil {
			return pkgacct.Payload{}, err
		}
		copied++
	}

	// EasyApache's own record of what is installed. ea_current_to_profile
	// writes a profile ea_install_profile can rebuild from, which is the
	// supported way to put a web server back the way it was.
	if err := r.writeEasyApacheProfile(ctx, root); err != nil {
		return pkgacct.Payload{}, err
	}
	if err := r.writeSystemManifest(ctx, root, copied); err != nil {
		return pkgacct.Payload{}, err
	}

	payload := pkgacct.Payload{
		Mode:    pkgacct.ModeSystem,
		Account: SystemAccount,
		Parts:   []pkgacct.Part{{Kind: pkgacct.PartSystem, Path: root}},
	}
	return payload, payload.Verify()
}

// writeEasyApacheProfile asks EasyApache what is installed, in the form
// that can install it again.
func (r *Real) writeEasyApacheProfile(ctx context.Context, root string) error {
	tool := r.easyApacheTool()
	if _, err := os.Stat(tool); err != nil {
		// Older cPanel, or EA3. The package list below still records what
		// was there.
		return nil
	}
	// The flag takes its value with an equals sign; passed as two
	// arguments the tool reports "Unknown argument".
	out, err := exec.CommandContext(ctx, tool,
		"--output="+filepath.Join(root, "ea4-profile.json")).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cpanel: read the EasyApache profile: %w: %s", err, lastLine(out))
	}
	return nil
}

// writeSystemManifest records what was taken and what this server was, so
// a restore on a different machine can be judged rather than guessed at.
func (r *Real) writeSystemManifest(ctx context.Context, root string, copied int) error {
	var manifest strings.Builder
	manifest.WriteString("# Gniza system backup\n")
	fmt.Fprintf(&manifest, "paths_copied\t%d\n", copied)

	if version, err := os.ReadFile("/usr/local/cpanel/version"); err == nil {
		fmt.Fprintf(&manifest, "cpanel_version\t%s\n", strings.TrimSpace(string(version)))
	}
	if out, err := exec.CommandContext(ctx, "rpm", "-qa", "ea-*").Output(); err == nil {
		packages := strings.Fields(string(out))
		fmt.Fprintf(&manifest, "ea4_packages\t%d\n", len(packages))
		if err := os.WriteFile(filepath.Join(root, "ea4-packages.txt"),
			[]byte(strings.Join(packages, "\n")+"\n"), 0o600); err != nil {
			return fmt.Errorf("cpanel: write the EasyApache package list: %w", err)
		}
	}
	for _, path := range systemPaths {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			fmt.Fprintf(&manifest, "absent\t%s\n", path)
		}
	}
	return os.WriteFile(filepath.Join(root, "manifest.txt"), []byte(manifest.String()), 0o600)
}

func (r *Real) easyApacheTool() string {
	if r.EasyApachePath != "" {
		return r.EasyApachePath
	}
	return "/usr/local/bin/ea_current_to_profile"
}

// copyPath copies a file or a directory tree, keeping the permissions the
// original had: these are configuration files whose modes matter.
func copyPath(source, target string, info os.FileInfo) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("cpanel: create %s: %w", filepath.Dir(target), err)
	}
	if !info.IsDir() {
		return copyFile(source, target, info.Mode().Perm())
	}
	return filepath.Walk(source, func(path string, walked os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		into := filepath.Join(target, rel)
		switch {
		case walked.IsDir():
			return os.MkdirAll(into, 0o700)
		case walked.Mode()&os.ModeSymlink != 0:
			// A symlink in a configuration tree points at something this
			// copy does not own; the name is worth keeping, the target is
			// not followed.
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(into)
			return os.Symlink(link, into)
		case !walked.Mode().IsRegular():
			return nil
		}
		return copyFile(path, into, walked.Mode().Perm())
	})
}

func copyFile(source, target string, mode os.FileMode) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("cpanel: read %s: %w", source, err)
	}
	if err := os.WriteFile(target, data, mode); err != nil {
		return fmt.Errorf("cpanel: write %s: %w", target, err)
	}
	return nil
}
