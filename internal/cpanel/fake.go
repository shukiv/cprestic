package cpanel

import (
	"archive/tar"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shuki/cprest/internal/granular"
	"github.com/shuki/cprest/internal/pkgacct"
)

// Fake builds a synthetic account tree with the same shape the real
// provider produces.
//
// It exists so the agent, the controller and restic can be exercised end to
// end on a machine with no cPanel installation. It is not a test double
// living in a _test.go file because the E2E harness and the agent's
// development mode both need it.
type Fake struct {
	// Root is where synthetic home directories are created.
	Root string
	// Caps controls which pkgacct flags the fake host claims to support,
	// so degraded-mode behaviour can be exercised too.
	Caps pkgacct.Capabilities
	// Databases maps a user to the databases that account owns.
	Databases map[string][]string
	// FileCount and FileSize shape the generated home directory.
	FileCount int
	FileSize  int
	// Applied records the archives Apply was called with.
	Applied []string
}

var _ Provider = (*Fake)(nil)

// DefaultFakeCaps is a modern cPanel: every flag we care about is present.
var DefaultFakeCaps = pkgacct.Capabilities{
	NoCompressFlag:  "--nocompress",
	SkipHomedirFlag: "--skiphomedir",
	SkipDBFlag:      "--skipdb",
}

func (f *Fake) Capabilities(context.Context) (pkgacct.Capabilities, error) {
	if f.Caps == (pkgacct.Capabilities{}) {
		return DefaultFakeCaps, nil
	}
	return f.Caps, nil
}

// Accounts lists the synthetic accounts, creating their home directories on
// first sight. Like the real provider it reports names and home directories
// only; sizes and databases cost too much to gather for a listing.
func (f *Fake) Accounts(_ context.Context) ([]AccountInfo, error) {
	names := make([]string, 0, len(f.Databases))
	for user := range f.Databases {
		names = append(names, user)
	}
	sort.Strings(names)

	accounts := make([]AccountInfo, 0, len(names))
	for _, user := range names {
		home := filepath.Join(f.Root, "home", user)
		if err := f.populateHome(home, user); err != nil {
			return nil, err
		}
		accounts = append(accounts, AccountInfo{User: user, HomeDir: home})
	}
	return accounts, nil
}

// Account creates the account's home directory if it does not exist yet and
// reports its contents.
func (f *Fake) Account(_ context.Context, user string) (AccountInfo, error) {
	if err := validateUser(user); err != nil {
		return AccountInfo{}, err
	}
	home := filepath.Join(f.Root, "home", user)
	if err := f.populateHome(home, user); err != nil {
		return AccountInfo{}, err
	}
	size, err := directorySize(home)
	if err != nil {
		return AccountInfo{}, err
	}
	return AccountInfo{
		User:      user,
		HomeDir:   home,
		Databases: f.Databases[user],
		SizeBytes: size,
	}, nil
}

// Stage writes a metadata archive and per-database dumps, mirroring what
// pkgacct and mysqldump would leave behind.
func (f *Fake) Stage(ctx context.Context, req StageRequest) (pkgacct.Payload, error) {
	caps, err := f.Capabilities(ctx)
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

	for _, part := range payload.Parts {
		switch part.Kind {
		case pkgacct.PartHomedir:
			// Already on disk.
		case pkgacct.PartMetadata:
			// pkgacct writes an archive into the directory it is given, so
			// the fake does too: restore has to find and unpack it.
			if err := writeMetadataArchive(part.Path, req.Account.User); err != nil {
				return pkgacct.Payload{}, err
			}
		case pkgacct.PartDatabase:
			if err := os.MkdirAll(part.Path, 0o700); err != nil {
				return pkgacct.Payload{}, fmt.Errorf("cpanel: create %s: %w", part.Path, err)
			}
			for name, path := range payload.DumpPaths {
				if err := writeFile(path, sqlDumpFor(name)); err != nil {
					return pkgacct.Payload{}, err
				}
			}
			// The users that own those databases are staged beside them,
			// as the real provider does.
			users := filepath.Join(part.Path, granular.DatabaseUsersFile)
			if err := writeFile(users, []byte("-- database users for "+req.Account.User+"\n")); err != nil {
				return pkgacct.Payload{}, err
			}
		case pkgacct.PartArchive:
			if err := writeMonolithicArchive(part.Path, req.Account.User); err != nil {
				return pkgacct.Payload{}, err
			}
		}
	}
	return payload, nil
}

// Apply records that an archive would have been handed to cPanel.
//
// The fake cannot restore an account, so it verifies the archive exists and
// writes a marker the caller can assert on. A test that needs to know
// restorepkg was invoked checks Applied.
func (f *Fake) Apply(_ context.Context, archivePath string) error {
	info, err := os.Stat(archivePath)
	if err != nil {
		return fmt.Errorf("cpanel: restore archive: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("cpanel: restore archive %s is a directory", archivePath)
	}
	f.Applied = append(f.Applied, archivePath)
	return nil
}

// cpmoveRoot is the top-level directory name inside a cPanel account
// archive. Reassembly discovers this name rather than assuming it; the
// fake picks cPanel's conventional one so the discovery is exercised.
func cpmoveRoot(account string) string { return "cpmove-" + account }

// writeMetadataArchive produces what pkgacct leaves in its output
// directory when the home directory and databases are excluded: one
// archive holding the account's configuration.
func writeMetadataArchive(dir, account string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cpanel: create %s: %w", dir, err)
	}
	root := cpmoveRoot(account)
	return writeTar(filepath.Join(dir, root+".tar"), map[string][]byte{
		root + "/version":             []byte("6\n"),
		root + "/meta/user":           []byte(account + "\n"),
		root + "/cp/" + account:       []byte("DNS=" + account + ".example\nUSER=" + account + "\n"),
		root + "/userdata/main":       []byte("main_domain: " + account + ".example\n"),
		root + "/dnszones/" + account: []byte("; fake zone for " + account + "\n"),
	})
}

// writeMonolithicArchive produces the single-archive payload: the same
// configuration plus the home directory, the way pkgacct would.
func writeMonolithicArchive(path, account string) error {
	root := cpmoveRoot(account)
	return writeTar(path, map[string][]byte{
		root + "/version":                        []byte("6\n"),
		root + "/meta/user":                      []byte(account + "\n"),
		root + "/cp/" + account:                  []byte("USER=" + account + "\n"),
		root + "/homedir/public_html/index.html": []byte("<h1>" + account + "</h1>\n"),
	})
}

// writeTar writes an uncompressed archive with the given contents, creating
// the directory entries each path implies.
func writeTar(path string, files map[string][]byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("cpanel: create %s: %w", filepath.Dir(path), err)
	}
	out, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("cpanel: create %s: %w", path, err)
	}
	defer out.Close()

	writer := tar.NewWriter(out)
	for _, dir := range tarDirectories(files) {
		if err := writer.WriteHeader(&tar.Header{
			Name: dir + "/", Typeflag: tar.TypeDir, Mode: 0o755,
		}); err != nil {
			return fmt.Errorf("cpanel: write %s: %w", dir, err)
		}
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := files[name]
		if err := writer.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
			return fmt.Errorf("cpanel: write %s: %w", name, err)
		}
		if _, err := writer.Write(body); err != nil {
			return fmt.Errorf("cpanel: write %s: %w", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("cpanel: finish %s: %w", path, err)
	}
	return nil
}

// tarDirectories returns every directory the file paths imply, parents
// first, so the archive is well-formed.
func tarDirectories(files map[string][]byte) []string {
	seen := map[string]bool{}
	for name := range files {
		parts := strings.Split(name, "/")
		for i := 1; i < len(parts); i++ {
			seen[strings.Join(parts[:i], "/")] = true
		}
	}
	dirs := make([]string, 0, len(seen))
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// populateHome creates a deterministic-per-user tree the first time an
// account is seen, so repeated backups deduplicate the way real ones do.
func (f *Fake) populateHome(home, user string) error {
	if _, err := os.Stat(home); err == nil {
		return nil
	}
	count := f.FileCount
	if count <= 0 {
		count = 12
	}
	size := f.FileSize
	if size <= 0 {
		size = 4096
	}

	for _, dir := range []string{"public_html", "mail", ".cpanel"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			return fmt.Errorf("cpanel: create fake home: %w", err)
		}
	}
	// Seeded by user name so two runs produce identical bytes.
	source := rand.New(rand.NewSource(int64(len(user)) * 7919))
	for i := range count {
		body := make([]byte, size)
		if _, err := source.Read(body); err != nil {
			return fmt.Errorf("cpanel: generate fake content: %w", err)
		}
		path := filepath.Join(home, "public_html", fmt.Sprintf("page-%02d.html", i))
		if err := writeFile(path, body); err != nil {
			return err
		}
	}
	return nil
}

func sqlDumpFor(name string) []byte {
	return []byte(fmt.Sprintf(
		"-- cprest fake dump of %s\nCREATE TABLE posts (id int, body text);\n"+
			"INSERT INTO posts VALUES (1, 'hello');\n", name))
}

func writeFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("cpanel: create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("cpanel: write %s: %w", path, err)
	}
	return nil
}
