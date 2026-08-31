package agent

import (
	"context"
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
// The result is a directory and an archive of it, left on this server for
// the operator to put back where it belongs. Nothing is written into the
// live account: restoring a mailbox in place needs the file ownership the
// account had, and putting a zone file back is a DNS change, not a file
// copy. Both are worth doing on purpose rather than as a side effect of a
// download.
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
