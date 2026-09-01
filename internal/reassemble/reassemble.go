package reassemble

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shuki/cprest/internal/pkgacct"
	"github.com/shuki/cprest/internal/resticrun"
)

// Layout names the subdirectories a cpmove archive keeps its parts in.
//
// These are cPanel's, not ours, and have been stable for a long time — but
// they have never been verified against a live cPanel here. Reassembly
// therefore discovers the archive's actual top-level directory rather than
// assuming it, and fails loudly if what it finds does not look like a
// cpmove tree. Verify these against the target cPanel version before
// trusting a restore in production.
const (
	HomedirDir  = "homedir"
	DatabaseDir = "mysql"
	// DatabaseUsersFile is where a cpmove archive keeps the account's
	// database users and their grants: at the top of the tree, not with
	// the databases. Checked against what /scripts/pkgacct produces on
	// cPanel 136.
	DatabaseUsersFile = "mysql.sql"
	// StagedDatabaseUsersFile is what cprest names the same thing where
	// it dumps it, beside the databases. It is granular.DatabaseUsersFile
	// spelt out rather than imported: granular imports this package.
	StagedDatabaseUsersFile = "_users.sql"
)

// Request describes a restore of one account.
type Request struct {
	Account    string
	SnapshotID string
	// WorkDir is scratch space. Everything under it is the caller's to
	// remove when the restore is finished.
	WorkDir string
	// Repo is the repository holding the snapshot.
	Repo resticrun.Repository
}

// Result is what a completed reassembly produced.
type Result struct {
	// ArchivePath is the cpmove archive, ready for restorepkg.
	ArchivePath string
	// TreeDir is the extracted tree the archive was built from, kept so an
	// operator can inspect it.
	TreeDir string
	// Mode records which payload shape was restored.
	Mode pkgacct.Mode
	// BytesRestored is what restic reported across every part.
	BytesRestored uint64
}

// Restorer performs the restic side of a restore. The concrete
// implementation is *resticrun.Runner; the interface keeps reassembly
// testable without a repository.
type Restorer interface {
	Snapshots(ctx context.Context, repo resticrun.Repository, filter resticrun.SnapshotFilter) ([]resticrun.Snapshot, error)
	Restore(ctx context.Context, repo resticrun.Repository, spec resticrun.RestoreSpec) (resticrun.RestoreResult, error)
}

// Run rebuilds an account archive from a snapshot.
//
// A monolithic snapshot holds the archive already and is simply restored. A
// split snapshot is put back together: the metadata archive is extracted,
// then the home directory and the database dumps are restored straight into
// their slots inside it, and the tree is repacked.
func Run(ctx context.Context, restorer Restorer, req Request) (Result, error) {
	if req.Account == "" {
		return Result{}, fmt.Errorf("reassemble: account is required")
	}
	if req.WorkDir == "" {
		return Result{}, fmt.Errorf("reassemble: work directory is required")
	}

	snapshot, err := findSnapshot(ctx, restorer, req)
	if err != nil {
		return Result{}, err
	}
	found, err := classifyPaths(snapshot.Paths)
	if err != nil {
		return Result{}, err
	}

	if found.mode() == pkgacct.ModeMonolithic {
		return restoreMonolithic(ctx, restorer, req, snapshot, found)
	}
	return restoreSplit(ctx, restorer, req, snapshot, found)
}

// FindSnapshot resolves the snapshot a request names, and checks it
// belongs to the account asking for it.
func FindSnapshot(ctx context.Context, restorer Restorer, req Request) (resticrun.Snapshot, error) {
	return findSnapshot(ctx, restorer, req)
}

func findSnapshot(ctx context.Context, restorer Restorer, req Request) (resticrun.Snapshot, error) {
	snapshots, err := restorer.Snapshots(ctx, req.Repo, resticrun.SnapshotFilter{
		Tags: []string{"account:" + req.Account},
	})
	if err != nil {
		return resticrun.Snapshot{}, err
	}
	for _, snapshot := range snapshots {
		if snapshot.ID != req.SnapshotID && snapshot.ShortID != req.SnapshotID {
			continue
		}
		// The listing was filtered by tag, but the match is re-checked
		// here: restoring one customer's data into another's account
		// would be about the worst thing this program could do.
		if account := snapshot.Account(); account != req.Account {
			return resticrun.Snapshot{}, fmt.Errorf(
				"reassemble: snapshot %s belongs to account %q, not %q",
				req.SnapshotID, account, req.Account)
		}
		return snapshot, nil
	}
	return resticrun.Snapshot{}, fmt.Errorf(
		"reassemble: snapshot %s does not belong to account %s in this repository",
		req.SnapshotID, req.Account)
}

// Parts maps a snapshot's recorded paths back to the roles they played.
type Parts struct {
	Metadata  string
	Homedir   string
	Databases string
	Archive   string
	// System is the server's own configuration, backed up under its own
	// name rather than as an account.
	System string
}

func (p Parts) mode() pkgacct.Mode {
	if p.Archive != "" {
		return pkgacct.ModeMonolithic
	}
	return pkgacct.ModeSplit
}

// classifyPaths works out what each snapshot path was.
//
// The agent stages metadata and database dumps in named subdirectories and
// backs up the home directory in place, so the roles are recoverable from
// the paths themselves. Nothing about the staging root is assumed.
// Classify is how a caller recovers the roles of a snapshot's paths.
func Classify(paths []string) (Parts, error) { return classifyPaths(paths) }

func classifyPaths(paths []string) (Parts, error) {
	var found Parts
	for _, path := range paths {
		switch {
		case strings.HasSuffix(path, "/metadata"):
			found.Metadata = path
		case strings.HasSuffix(path, "/databases"):
			found.Databases = path
		case strings.HasSuffix(path, "/system"):
			found.System = path
		case strings.HasSuffix(path, ".tar"), strings.HasSuffix(path, ".tar.gz"):
			found.Archive = path
		default:
			if found.Homedir != "" {
				return Parts{}, fmt.Errorf(
					"reassemble: snapshot has two candidate home directories, %s and %s",
					found.Homedir, path)
			}
			found.Homedir = path
		}
	}

	switch {
	case found.System != "":
		// A backup of the server itself: one directory of configuration,
		// and none of the parts an account has.
		if found.Metadata != "" || found.Homedir != "" || found.Archive != "" {
			return Parts{}, fmt.Errorf(
				"reassemble: snapshot mixes the server's own settings with an account's parts")
		}
		return found, nil
	case found.Archive != "" && (found.Metadata != "" || found.Homedir != ""):
		return Parts{}, fmt.Errorf("reassemble: snapshot mixes a monolithic archive with split Parts")
	case found.Archive != "":
		return found, nil
	case found.Metadata == "":
		return Parts{}, fmt.Errorf("reassemble: snapshot has no metadata part")
	case found.Homedir == "":
		return Parts{}, fmt.Errorf("reassemble: snapshot has no home directory part")
	}
	return found, nil
}

func restoreMonolithic(ctx context.Context, restorer Restorer, req Request,
	snapshot resticrun.Snapshot, found Parts) (Result, error) {

	dir := filepath.Join(req.WorkDir, "archive")
	restored, err := restorer.Restore(ctx, req.Repo, resticrun.RestoreSpec{
		SnapshotID: snapshot.ID,
		Subpath:    filepath.Dir(found.Archive),
		Target:     dir,
	})
	if err != nil {
		return Result{}, fmt.Errorf("reassemble: restore archive: %w", err)
	}

	archive := filepath.Join(dir, filepath.Base(found.Archive))
	if _, err := os.Stat(archive); err != nil {
		return Result{}, fmt.Errorf("reassemble: restored archive is missing: %w", err)
	}
	return Result{
		ArchivePath:   archive,
		Mode:          pkgacct.ModeMonolithic,
		BytesRestored: restored.BytesRestored,
	}, nil
}

func restoreSplit(ctx context.Context, restorer Restorer, req Request,
	snapshot resticrun.Snapshot, found Parts) (Result, error) {

	var bytesRestored uint64
	restore := func(subpath, target string) error {
		restored, err := restorer.Restore(ctx, req.Repo, resticrun.RestoreSpec{
			SnapshotID: snapshot.ID,
			Subpath:    subpath,
			Target:     target,
		})
		if err != nil {
			return err
		}
		bytesRestored += restored.BytesRestored
		return nil
	}

	// 1. The metadata part holds the pkgacct archive with everything except
	//    the home directory and the databases.
	metadataDir := filepath.Join(req.WorkDir, "metadata")
	if err := restore(found.Metadata, metadataDir); err != nil {
		return Result{}, fmt.Errorf("reassemble: restore metadata: %w", err)
	}

	archive, err := soleArchive(metadataDir)
	if err != nil {
		return Result{}, err
	}
	treeDir := filepath.Join(req.WorkDir, "tree")
	if err := extractTar(archive, treeDir); err != nil {
		return Result{}, err
	}

	// 2. The archive's own top-level directory is discovered rather than
	//    assumed, because its name is cPanel's to choose.
	root, err := soleDirectory(treeDir)
	if err != nil {
		return Result{}, fmt.Errorf("reassemble: metadata archive is not a cpmove tree: %w", err)
	}

	// 3. Each remaining part is restored straight into its slot.
	if err := restore(found.Homedir, filepath.Join(root, HomedirDir)); err != nil {
		return Result{}, fmt.Errorf("reassemble: restore home directory: %w", err)
	}
	if found.Databases != "" {
		if err := restore(found.Databases, filepath.Join(root, DatabaseDir)); err != nil {
			return Result{}, fmt.Errorf("reassemble: restore databases: %w", err)
		}
		if err := placeDatabaseUsers(root); err != nil {
			return Result{}, err
		}
	}

	// 4. Repack, which is the form restorepkg accepts.
	rebuilt := filepath.Join(req.WorkDir, filepath.Base(root)+".tar")
	if err := createTar(treeDir, rebuilt); err != nil {
		return Result{}, err
	}
	return Result{
		ArchivePath:   rebuilt,
		TreeDir:       treeDir,
		Mode:          pkgacct.ModeSplit,
		BytesRestored: bytesRestored,
	}, nil
}

// placeDatabaseUsers moves the grants file to the name cPanel's own
// restore reads.
//
// cprest dumps the account's database users beside its databases, because
// that is where they are produced. A cpmove archive keeps them somewhere
// else: one file per database under mysql/, and the users and their
// grants in mysql.sql at the top of the tree. restorepkg reads the
// latter and nothing else, so an archive with the file under mysql/ was
// restored with every table in place and no user able to read them.
func placeDatabaseUsers(root string) error {
	from := filepath.Join(root, DatabaseDir, StagedDatabaseUsersFile)
	if _, err := os.Stat(from); err != nil {
		// No users file: an account with no databases, or a backup taken
		// before cprest dumped them.
		return nil
	}
	to := filepath.Join(root, DatabaseUsersFile)
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("reassemble: place the database users where restorepkg reads them: %w", err)
	}
	return nil
}

// soleArchive finds the single archive a metadata restore produced.
func soleArchive(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reassemble: read %s: %w", dir, err)
	}
	var archives []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(name, ".tar") || strings.HasSuffix(name, ".tar.gz") {
			archives = append(archives, filepath.Join(dir, name))
		}
	}
	switch len(archives) {
	case 1:
		return archives[0], nil
	case 0:
		return "", fmt.Errorf("reassemble: no archive in the restored metadata part")
	default:
		return "", fmt.Errorf("reassemble: %d archives in the restored metadata part, expected one",
			len(archives))
	}
}

// soleDirectory returns the one directory inside dir, which for a cpmove
// archive is the account tree.
func soleDirectory(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reassemble: read %s: %w", dir, err)
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(dir, entry.Name()))
		}
	}
	if len(dirs) != 1 {
		return "", fmt.Errorf("found %d top-level directories, expected one", len(dirs))
	}
	return dirs[0], nil
}
