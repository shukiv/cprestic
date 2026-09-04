package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

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
// written back -- files, the website, mail, a database -- what came out is
// then written into the live account. The rest stay a copy: putting a zone
// file back is a DNS change and reinstating an FTP login is a change to a
// password store, neither of which is a file copy, and both are worth doing
// on purpose rather than as a side effect of a restore.
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
