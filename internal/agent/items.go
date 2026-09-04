package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/granular"
	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/protocol"
	"github.com/shuki/cprest/internal/reassemble"
	"github.com/shuki/cprest/internal/resticrun"
	"github.com/shuki/cprest/internal/staging"
)

// restoreItems takes one part of an account out of a snapshot: a mailbox,
// a database, the DNS records, the certificates.
//
// The result is a directory and an archive of it, left on this server to
// collect. When the request asks for it, and only for the parts that can be
// written back -- files, the website, mail, a database, the database users
// -- what came out is then written into the live account. The rest stay a
// copy, not because putting them back is impossible but because each needs
// a change the control panel has to make -- a zone edit, an installed
// certificate, an FTP login -- and none of those is built yet.
func (a *Agent) restoreItems(ctx context.Context, log *slog.Logger,
	assignment protocol.RestoreAssignment, repo resticrun.Repository,
	dir *staging.Dir, report protocol.RestoreReport) (protocol.RestoreReport, bool) {

	snapshot, err := reassemble.FindSnapshot(ctx, a.runner, reassemble.Request{
		Account:    assignment.CPanelUser,
		SnapshotID: assignment.SnapshotID,
		WorkDir:    dir.Path,
		Repo:       repo,
	})
	if err != nil {
		report.Error = err.Error()
		return report, false
	}
	parts, err := reassemble.Classify(snapshot.Paths)
	if err != nil {
		report.Error = err.Error()
		return report, false
	}
	plan, err := granular.Build(parts, granular.Request{
		Kind:    granular.Kind(assignment.ItemKind),
		Account: assignment.CPanelUser,
		Names:   assignment.ItemNames,
	})
	if err != nil {
		report.Error = err.Error()
		return report, false
	}

	raw := filepath.Join(dir.Path, "raw")
	restored, err := a.runner.Restore(ctx, repo, resticrun.RestoreSpec{
		SnapshotID: snapshot.ID,
		Target:     raw,
		Include:    plan.Include,
	})
	if err != nil {
		log.Error("restore items", "error", err, "include", plan.Include)
		report.Error = err.Error()
		return report, false
	}
	if restored.FilesRestored == 0 {
		report.Error = fmt.Sprintf(
			"agent: nothing in this backup matched %s", plan.Description)
		return report, false
	}

	out := filepath.Join(dir.Path, "items")
	if err := os.MkdirAll(out, 0o700); err != nil {
		report.Error = fmt.Sprintf("agent: create the restore tree: %v", err)
		return report, false
	}

	// What came out of the home directory and the database dumps keeps the
	// shape it had, under names that say where it belongs.
	if parts.Homedir != "" {
		if err := adopt(filepath.Join(raw, parts.Homedir), filepath.Join(out, "homedir")); err != nil {
			report.Error = err.Error()
			return report, false
		}
	}
	if parts.Databases != "" {
		if err := adopt(filepath.Join(raw, parts.Databases), filepath.Join(out, "databases")); err != nil {
			report.Error = err.Error()
			return report, false
		}
	}
	if parts.System != "" {
		if err := adopt(filepath.Join(raw, parts.System), filepath.Join(out, "system")); err != nil {
			report.Error = err.Error()
			return report, false
		}
	}

	// Configuration lives inside the account's metadata archive, so only
	// the members that were asked for are taken out of it.
	if len(plan.Members) > 0 {
		archive, err := soleArchive(filepath.Join(raw, plan.Metadata))
		if err != nil {
			report.Error = err.Error()
			return report, false
		}
		written, err := reassemble.ExtractMembers(archive, filepath.Join(out, "metadata"), plan.Members)
		if err != nil {
			report.Error = err.Error()
			return report, false
		}
		log.Info("took configuration out of the account archive",
			"files", written, "members", plan.Members)
	}

	files, bytes, err := measure(out)
	if err != nil {
		report.Error = err.Error()
		return report, false
	}
	if files == 0 {
		// restic restored something, but none of it was what the operator
		// asked for. Reporting success here would hand over an empty
		// directory as though it were their data.
		report.Error = fmt.Sprintf(
			"agent: this backup holds nothing for %s", plan.Description)
		return report, false
	}

	if assignment.Apply {
		// Written into the live account rather than handed over. The raw
		// restic tree is not wanted either way.
		if err := os.RemoveAll(raw); err != nil {
			log.Warn("remove the raw restore tree", "error", err)
		}
		written, hint, err := a.applyItems(ctx, log, assignment, out)
		if err != nil {
			report.Error = err.Error()
			report.Hint = hint
			return report, false
		}
		report.Status = string(job.StatusSuccess)
		report.BytesRestored = bytes
		report.Applied = true
		report.Detail = written
		log.Warn("granular restore written into the live account",
			"account", assignment.CPanelUser, "kind", assignment.ItemKind,
			"files", files, "wrote", written)
		return report, false
	}

	archivePath := filepath.Join(dir.Path, fmt.Sprintf("items-%s-%s.tar",
		assignment.CPanelUser, assignment.ItemKind))
	if err := reassemble.PackDir(out, archivePath); err != nil {
		report.Error = err.Error()
		return report, false
	}

	// The raw restic tree has served its purpose and would otherwise
	// double the space this restore holds.
	if err := os.RemoveAll(raw); err != nil {
		log.Warn("remove the raw restore tree", "error", err)
	}

	retained, err := a.staging.Retain(dir)
	if err != nil {
		log.Error("retain the restored items", "error", err)
		report.Error = err.Error()
		return report, false
	}

	report.Status = string(job.StatusSuccess)
	report.BytesRestored = bytes
	report.ArchivePath = filepath.Join(retained.Path, filepath.Base(archivePath))
	report.RestoredTo = filepath.Join(retained.Path, "items")
	report.Detail = fmt.Sprintf("%d files of %s", files, plan.Description)
	log.Info("granular restore ready to collect",
		"kind", assignment.ItemKind, "files", files, "archive", report.ArchivePath)
	return report, true
}

// adopt moves a restored subtree to where the operator will look for it.
// A missing source is not an error: a plan may ask for parts a particular
// request does not use.
func adopt(from, to string) error {
	if _, err := os.Stat(from); err != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return fmt.Errorf("agent: create %s: %w", filepath.Dir(to), err)
	}
	if err := os.Rename(from, to); err == nil {
		return nil
	}
	// Staging and the restore tree are the same volume in practice, but a
	// rename across devices fails and a copy still has to work.
	return copyTree(from, to)
}

func copyTree(from, to string) error {
	return filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case !info.Mode().IsRegular():
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		source, err := os.Open(path)
		if err != nil {
			return err
		}
		defer source.Close()
		sink, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(sink, source)
		closeErr := sink.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

// soleArchive finds the account archive inside a restored metadata part.
// pkgacct names it after the account, so it is discovered rather than
// assumed.
func soleArchive(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("agent: read the restored metadata: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tar") {
			return filepath.Join(dir, entry.Name()), nil
		}
	}
	return "", fmt.Errorf("agent: the metadata in this backup holds no account archive")
}

// measure counts what a restore actually produced.
func measure(dir string) (files int, bytes uint64, err error) {
	err = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			files++
			bytes += uint64(info.Size())
		}
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("agent: measure the restored files: %w", err)
	}
	return files, bytes, nil
}

// applyItems writes what came out of the backup into the live account, and
// says what it wrote.
//
// Only the kinds granular says can be applied reach here; the node refuses
// the others before a job exists. The check is made again because this runs
// as root on a live account, and a second reading of the same rule costs
// nothing next to what getting it wrong costs.
func (a *Agent) applyItems(ctx context.Context, log *slog.Logger,
	assignment protocol.RestoreAssignment, out string) (written, hint string, err error) {

	kind := granular.Kind(assignment.ItemKind)
	if !kind.CanApply() {
		return "", "", fmt.Errorf(
			"agent: a %s restore cannot be written into the live account", kind)
	}

	if kind == granular.KindDatabase {
		return a.loadDatabases(ctx, log, assignment, filepath.Join(out, "databases"))
	}
	if kind == granular.KindDBUsers {
		return a.restoreDatabaseUsers(ctx, log, assignment, filepath.Join(out, "databases"))
	}

	// Files, the website and mail are all the home directory, restored
	// where they were.
	homedir := filepath.Join(out, "homedir")
	if _, err := os.Stat(homedir); err != nil {
		return "", "This backup does not contain the account's files.", errors.New(
			"agent: this backup holds none of the account's files")
	}
	if err := a.provider.PutHomeDir(ctx, assignment.CPanelUser, homedir); err != nil {
		return "", "", err
	}
	return "written into the home directory of " + assignment.CPanelUser, "", nil
}

// loadDatabases puts the named database dumps back into the databases they
// came out of.
//
// The account's databases are read first, and a name that is no longer one
// of them is reported as itself rather than as a failure. Restoring a
// database somebody dropped is the reason this exists, and being told "ask
// your host" when the answer is "create it again first" would make the one
// case it was built for the one case it cannot explain.
func (a *Agent) loadDatabases(ctx context.Context, log *slog.Logger,
	assignment protocol.RestoreAssignment, databases string) (written, hint string, err error) {

	if len(assignment.ItemNames) == 0 {
		return "", "", errors.New("agent: no database was named to restore")
	}
	account, err := a.provider.Account(ctx, assignment.CPanelUser)
	if err != nil {
		return "", "", err
	}
	present := make(map[string]bool, len(account.Databases))
	for _, name := range account.Databases {
		present[name] = true
	}
	var missing []string
	for _, name := range assignment.ItemNames {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Sprintf(
				"The database %s is not on the account any more. Create it again first, "+
					"then restore into it: a backup can fill a database but cannot make one.",
				strings.Join(missing, " and ")), fmt.Errorf(
				"agent: %s no longer has the database(s) %s",
				assignment.CPanelUser, strings.Join(missing, ", "))
	}

	var loaded []string
	for _, name := range assignment.ItemNames {
		dump := filepath.Join(databases, name+".sql")
		if _, err := os.Stat(dump); err != nil {
			return "", fmt.Sprintf(
					"This backup holds no copy of the database %s. Try an earlier "+
						"restore point.", name),
				fmt.Errorf("agent: this backup holds no dump of the database %s", name)
		}
		if err := a.provider.LoadDatabase(ctx, assignment.CPanelUser, name, dump); err != nil {
			return "", "", err
		}
		log.Warn("database loaded from a backup",
			"account", assignment.CPanelUser, "database", name)
		loaded = append(loaded, name)
	}
	return "loaded into " + strings.Join(loaded, ", "), "", nil
}

// restoreDatabaseUsers recreates the account's database users from what the
// backup recorded of them.
//
// The users are read out of the staged files here rather than in the
// privileged provider: what runs there runs as root against the server's
// MySQL, and it takes checked values rather than a file whose contents
// nobody has looked at.
func (a *Agent) restoreDatabaseUsers(ctx context.Context, log *slog.Logger,
	assignment protocol.RestoreAssignment, databases string) (written, hint string, err error) {

	users, err := readStagedDatabaseUsers(databases)
	if err != nil {
		return "", "This backup does not hold the account's database users. Try an " +
			"earlier restore point.", err
	}
	if len(users) == 0 {
		return "", "This backup holds no database users for the account.", errors.New(
			"agent: this backup holds no database users")
	}

	// Which databases the account has now. A grant on one it no longer
	// has is the same situation as restoring into a dropped database, and
	// deserves the same answer rather than "ask your host".
	account, err := a.provider.Account(ctx, assignment.CPanelUser)
	if err != nil {
		return "", "", err
	}
	present := make(map[string]bool, len(account.Databases))
	for _, name := range account.Databases {
		present[name] = true
	}
	var missing []string
	for _, user := range users {
		for _, grant := range user.Grants {
			if !present[grant.Database] && !contains(missing, grant.Database) {
				missing = append(missing, grant.Database)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Sprintf(
				"These users had access to %s, which the account does not have any "+
					"more. Restore or create those databases first, then restore the "+
					"users: a grant cannot be given on a database that is not there.",
				strings.Join(missing, " and ")), fmt.Errorf(
				"agent: %s no longer has the database(s) %s",
				assignment.CPanelUser, strings.Join(missing, ", "))
	}

	if err := a.provider.PutDatabaseUsers(ctx, assignment.CPanelUser, users); err != nil {
		return "", "", err
	}
	// One name, however many hosts it exists on. Naming it once per host
	// is what the operator's log and the customer's page would otherwise
	// both say.
	var names []string
	seen := map[string]bool{}
	for _, user := range users {
		if !seen[user.Name] {
			seen[user.Name] = true
			names = append(names, user.Name)
		}
	}
	sort.Strings(names)
	log.Warn("database users recreated from a backup",
		"account", assignment.CPanelUser, "users", strings.Join(names, ","))
	return "recreated " + strings.Join(names, ", "), "", nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// stagedGrant matches the grant lines cprest itself writes into the
// runnable file: one database, no global privileges and nothing after the
// grantee. Anything else is refused rather than guessed at, because the
// two things that could be guessed -- dropping a privilege an application
// connects with, or widening one past the account -- are both worse than
// saying so.
var stagedGrant = regexp.MustCompile(
	"^GRANT (.+) ON `(.+)`\\.\\* TO '([^']+)'@'([^']+)'$")

// readStagedDatabaseUsers reads the users, their stored passwords and their
// grants out of a restored databases directory.
func readStagedDatabaseUsers(databases string) ([]cpanel.DatabaseUser, error) {
	raw, err := os.ReadFile(filepath.Join(databases, granular.DatabaseUsersAuthFile))
	if err != nil {
		return nil, fmt.Errorf("agent: read the database users in this backup: %w", err)
	}
	// cPanel's shape: user -> host -> what authenticates it.
	var auth map[string]map[string]struct {
		PassHash string `json:"pass_hash"`
		Plugin   string `json:"auth_plugin"`
	}
	if err := json.Unmarshal(raw, &auth); err != nil {
		return nil, fmt.Errorf("agent: read the database users in this backup: %w", err)
	}

	grants, err := readStagedGrants(filepath.Join(databases, granular.RunnableDatabaseUsersFile))
	if err != nil {
		return nil, err
	}

	var users []cpanel.DatabaseUser
	for name, hosts := range auth {
		for host, credentials := range hosts {
			users = append(users, cpanel.DatabaseUser{
				Name:   name,
				Host:   host,
				Plugin: credentials.Plugin,
				Hash:   credentials.PassHash,
				Grants: grants[name+"@"+host],
			})
		}
	}
	sort.Slice(users, func(i, j int) bool {
		if users[i].Name != users[j].Name {
			return users[i].Name < users[j].Name
		}
		return users[i].Host < users[j].Host
	})
	return users, nil
}

// readStagedGrants reads the privileges each user had, keyed by user@host.
func readStagedGrants(path string) (map[string][]cpanel.DatabaseGrant, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("agent: read the database grants in this backup: %w", err)
	}
	grants := map[string][]cpanel.DatabaseGrant{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ";"))
		if line == "" || strings.HasPrefix(line, "--") ||
			strings.HasPrefix(line, "CREATE USER ") {
			continue
		}
		if !strings.HasPrefix(line, "GRANT ") {
			return nil, fmt.Errorf(
				"agent: the database users in this backup hold a line this cannot read")
		}
		parts := stagedGrant.FindStringSubmatch(line)
		if parts == nil {
			return nil, fmt.Errorf(
				"agent: a grant in this backup is not one on a single database, " +
					"and restoring it is not something this can do")
		}
		database, literal := unescapeGrantDatabase(parts[2])
		if !literal {
			return nil, fmt.Errorf(
				"agent: a grant in this backup is on a pattern rather than on one "+
					"database (%s), and restoring it is not something this can do",
				parts[2])
		}
		var privileges []string
		for _, privilege := range strings.Split(parts[1], ",") {
			privilege = strings.TrimSpace(privilege)
			if privilege != "" {
				privileges = append(privileges, privilege)
			}
		}
		who := parts[3] + "@" + parts[4]
		grants[who] = append(grants[who], cpanel.DatabaseGrant{
			Database:   database,
			Privileges: privileges,
		})
	}
	return grants, nil
}

// unescapeGrantDatabase turns the database name in a GRANT back into the
// name of a database.
//
// MySQL stores the database of a grant as a LIKE pattern, so SHOW GRANTS
// prints "acct\\_shop" for the database "acct_shop" -- the backslash is
// what makes the underscore mean itself rather than any character. Carrying
// that name through unchanged would ask cPanel for a database nobody has.
//
// An underscore or a per-cent that is not escaped really is a wildcard: the
// grant covers every database matching it, which is not one database and
// not something this can put back. Those are reported rather than restored
// against whichever database the pattern happens to look like.
func unescapeGrantDatabase(pattern string) (name string, literal bool) {
	var out strings.Builder
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			if i+1 >= len(pattern) {
				return "", false
			}
			i++
			out.WriteByte(pattern[i])
		case '_', '%':
			return "", false
		default:
			out.WriteByte(pattern[i])
		}
	}
	return out.String(), true
}
