package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/granular"
	"github.com/shuki/cprest/internal/inventory"
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
	plan, err := granular.BuildAll(parts, itemRequests(assignment))
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
			"account", assignment.CPanelUser, "parts", itemsLabel(assignment),
			"files", files, "wrote", written)
		return report, false
	}

	archivePath := filepath.Join(dir.Path, fmt.Sprintf("items-%s-%s.tar",
		assignment.CPanelUser, itemsLabel(assignment)))
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
		"parts", itemsLabel(assignment), "files", files, "archive", report.ArchivePath)
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

// itemRequests is what this restore asks for, as granular reads it.
func itemRequests(assignment protocol.RestoreAssignment) []granular.Request {
	selections := assignment.Selections()
	requests := make([]granular.Request, 0, len(selections))
	for _, selection := range selections {
		requests = append(requests, granular.Request{
			Kind:    granular.Kind(selection.Kind),
			Account: assignment.CPanelUser,
			Names:   selection.Names,
		})
	}
	return requests
}

// itemsLabel names this restore in a log line and in the archive left to
// collect. One part of an account is named; several are counted, because
// the parts of a basket spelt out in a filename would run past what a
// filename can hold.
func itemsLabel(assignment protocol.RestoreAssignment) string {
	selections := assignment.Selections()
	switch len(selections) {
	case 0:
		return "nothing"
	case 1:
		return selections[0].Kind
	}
	return fmt.Sprintf("%dparts", len(selections))
}

// applyItems writes what came out of the backup into the live account, and
// says what it wrote.
//
// Only the kinds granular says can be applied reach here; the node refuses
// the others before a job exists. The check is made again because this runs
// as root on a live account, and a second reading of the same rule costs
// nothing next to what getting it wrong costs.
//
// Everything is checked before anything is written. A basket holding a
// database and its users, where the users would be refused, must not leave
// the database replaced and its users missing -- the account would come out
// of the restore worse off than it went in.
func (a *Agent) applyItems(ctx context.Context, log *slog.Logger,
	assignment protocol.RestoreAssignment, out string) (written, hint string, err error) {

	selections := assignment.Selections()
	if len(selections) == 0 {
		return "", "", errors.New(
			"agent: this restore names no part of the account to write back")
	}

	var (
		databaseNames, userNames          []string
		wantDumps, wantUsers, wantHomedir bool
	)
	for _, selection := range selections {
		kind := granular.Kind(selection.Kind)
		if !kind.CanApply() {
			return "", "", fmt.Errorf(
				"agent: a %s restore cannot be written into the live account", kind)
		}
		switch kind {
		case granular.KindDatabase:
			wantDumps = true
			databaseNames = append(databaseNames, selection.Names...)
		case granular.KindDBUsers:
			wantUsers = true
			userNames = append(userNames, selection.Names...)
		default:
			// Files, the website and mail are all the home directory,
			// restored where they were. A basket asking for two of them
			// asks for one tree, and writes it once.
			wantHomedir = true
		}
	}

	databases := filepath.Join(out, "databases")
	homedir := filepath.Join(out, "homedir")

	// Which databases the account has now. A dump has to load into a
	// database that is there, and a grant has to be given on one, so both
	// checks read the same list.
	var present map[string]bool
	if wantDumps || wantUsers {
		account, err := a.provider.Account(ctx, assignment.CPanelUser)
		if err != nil {
			return "", "", err
		}
		present = make(map[string]bool, len(account.Databases))
		for _, name := range account.Databases {
			present[name] = true
		}
	}

	var dumps []string
	if wantDumps {
		if dumps, hint, err = checkDatabaseDumps(
			assignment.CPanelUser, databaseNames, present, databases); err != nil {
			return "", hint, err
		}
	}
	var users []cpanel.DatabaseUser
	if wantUsers {
		if users, hint, err = checkDatabaseUsers(
			assignment.CPanelUser, userNames, present, databases); err != nil {
			return "", hint, err
		}
	}
	if wantHomedir {
		if _, err := os.Stat(homedir); err != nil {
			return "", "This backup does not contain the account's files.", errors.New(
				"agent: this backup holds none of the account's files")
		}
	}

	// Written in the order the account needs: a dump goes into a database,
	// and a grant is given on a database that is already there.
	var wrote []string
	for i, name := range databaseNames {
		if err := a.provider.LoadDatabase(ctx, assignment.CPanelUser, name, dumps[i]); err != nil {
			return "", "", err
		}
		log.Warn("database loaded from a backup",
			"account", assignment.CPanelUser, "database", name)
	}
	if wantDumps {
		wrote = append(wrote, "loaded into "+strings.Join(databaseNames, ", "))
	}
	if wantUsers {
		if err := a.provider.PutDatabaseUsers(ctx, assignment.CPanelUser, users); err != nil {
			return "", "", err
		}
		names := distinctUserNames(users)
		log.Warn("database users recreated from a backup",
			"account", assignment.CPanelUser, "users", strings.Join(names, ","))
		wrote = append(wrote, "recreated "+strings.Join(names, ", "))
	}
	if wantHomedir {
		if err := a.provider.PutHomeDir(ctx, assignment.CPanelUser, homedir); err != nil {
			return "", "", err
		}
		wrote = append(wrote, "written into the home directory of "+assignment.CPanelUser)
	}
	return strings.Join(wrote, "; "), "", nil
}

// checkDatabaseDumps makes sure every named database is still on the
// account and has a dump in this backup, and returns where those dumps are.
//
// A name that is no longer one of the account's databases is reported as
// itself rather than as a failure. Restoring a database somebody dropped is
// the reason this exists, and being told "ask your host" when the answer is
// "create it again first" would make the one case it was built for the one
// case it cannot explain.
func checkDatabaseDumps(account string, names []string, present map[string]bool,
	databases string) (dumps []string, hint string, err error) {

	if len(names) == 0 {
		return nil, "", errors.New("agent: no database was named to restore")
	}
	var missing []string
	for _, name := range names {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Sprintf(
				"The database %s is not on the account any more. Create it again first, "+
					"then restore into it: a backup can fill a database but cannot make one.",
				granular.JoinAnd(missing)), fmt.Errorf(
				"agent: %s no longer has the database(s) %s",
				account, strings.Join(missing, ", "))
	}
	for _, name := range names {
		dump := filepath.Join(databases, name+".sql")
		if _, err := os.Stat(dump); err != nil {
			return nil, fmt.Sprintf(
					"This backup holds no copy of the database %s. Try an earlier "+
						"restore point.", name),
				fmt.Errorf("agent: this backup holds no dump of the database %s", name)
		}
		dumps = append(dumps, dump)
	}
	return dumps, "", nil
}

// checkDatabaseUsers reads the account's database users out of the backup
// and makes sure every database they were granted access to is still there.
//
// The users are read out of the staged files here rather than in the
// privileged provider: what runs there runs as root against the server's
// MySQL, and it takes checked values rather than a file whose contents
// nobody has looked at.
func checkDatabaseUsers(account string, wanted []string, present map[string]bool,
	databases string) (users []cpanel.DatabaseUser, hint string, err error) {

	users, err = readStagedDatabaseUsers(databases)
	if err != nil {
		// A more recent restore point, not an earlier one. What a backup
		// can be missing here is the file that carries the stored
		// passwords, which cprest has not always written -- so going
		// further back is going further from having it.
		return nil, "This backup does not hold the account's database users. Try a " +
			"more recent restore point, or download this one and ask your host.", err
	}
	if len(users) == 0 {
		return nil, "This backup holds no database users for the account.", errors.New(
			"agent: this backup holds no database users")
	}

	// Named users, when some were chosen. A name is one login on several
	// hosts, and all of its hosts come back: a grant put back against
	// some of them is an application that connects and is refused.
	if len(wanted) > 0 {
		var chosen []cpanel.DatabaseUser
		for _, user := range users {
			if contains(wanted, user.Name) {
				chosen = append(chosen, user)
			}
		}
		var absent []string
		for _, name := range wanted {
			if !contains(distinctUserNames(chosen), name) {
				absent = append(absent, name)
			}
		}
		if len(absent) > 0 {
			sort.Strings(absent)
			return nil, fmt.Sprintf(
					"This backup holds no database user called %s. Choose one of the "+
						"users it does hold, or try another restore point.",
					granular.JoinAnd(absent)), fmt.Errorf(
					"agent: this backup holds no database user(s) %s",
					strings.Join(absent, ", "))
		}
		users = chosen
	}

	// A grant on a database the account no longer has is the same
	// situation as restoring into a dropped database, and deserves the
	// same answer rather than "ask your host".
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
		return nil, fmt.Sprintf(
				"These users had access to %s, which the account does not have any "+
					"more. Restore or create those databases first, then restore the "+
					"users: a grant cannot be given on a database that is not there.",
				granular.JoinAnd(missing)), fmt.Errorf(
				"agent: %s no longer has the database(s) %s",
				account, strings.Join(missing, ", "))
	}
	return users, "", nil
}

// distinctUserNames names each user once, however many hosts it exists on.
// Naming it once per host is what the operator's log and the customer's
// page would otherwise both say.
func distinctUserNames(users []cpanel.DatabaseUser) []string {
	var names []string
	seen := map[string]bool{}
	for _, user := range users {
		if !seen[user.Name] {
			seen[user.Name] = true
			names = append(names, user.Name)
		}
	}
	sort.Strings(names)
	return names
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// readStagedDatabaseUsers reads the users, their stored passwords and their
// grants out of a restored databases directory.
func readStagedDatabaseUsers(databases string) ([]cpanel.DatabaseUser, error) {
	raw, err := os.ReadFile(filepath.Join(databases, granular.DatabaseUsersAuthFile))
	if err != nil {
		return nil, fmt.Errorf("agent: read the database users in this backup: %w", err)
	}
	auth, err := inventory.ParseAuth(raw)
	if err != nil {
		return nil, err
	}
	raw, err = os.ReadFile(filepath.Join(databases, granular.RunnableDatabaseUsersFile))
	if err != nil {
		return nil, fmt.Errorf("agent: read the database grants in this backup: %w", err)
	}
	grants, err := inventory.ParseGrants(raw)
	if err != nil {
		return nil, err
	}

	var users []cpanel.DatabaseUser
	for name, hosts := range auth {
		for host, credentials := range hosts {
			user := cpanel.DatabaseUser{
				Name:   name,
				Host:   host,
				Plugin: credentials.Plugin,
				Hash:   credentials.Hash,
			}
			for _, grant := range grants[name+"@"+host] {
				user.Grants = append(user.Grants, cpanel.DatabaseGrant{
					Database:   grant.Database,
					Privileges: grant.Privileges,
				})
			}
			users = append(users, user)
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
