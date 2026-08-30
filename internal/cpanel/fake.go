package cpanel

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"

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
	// Accounts maps a user to its databases.
	Accounts map[string][]string
	// FileCount and FileSize shape the generated home directory.
	FileCount int
	FileSize  int
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
		Databases: f.Accounts[user],
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

	for _, part := range payload.Parts {
		switch part.Kind {
		case pkgacct.PartHomedir:
			// Already on disk.
		case pkgacct.PartMetadata:
			if err := writeFile(part.Path, metadataFor(req.Account)); err != nil {
				return pkgacct.Payload{}, err
			}
		case pkgacct.PartDatabase:
			name := strings.TrimSuffix(filepath.Base(part.Path), ".sql")
			if err := writeFile(part.Path, sqlDumpFor(name)); err != nil {
				return pkgacct.Payload{}, err
			}
		case pkgacct.PartArchive:
			if err := writeFile(part.Path, metadataFor(req.Account)); err != nil {
				return pkgacct.Payload{}, err
			}
		}
	}
	return payload, nil
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

func metadataFor(account AccountInfo) []byte {
	var builder strings.Builder
	builder.WriteString("# cprest fake pkgacct metadata\n")
	fmt.Fprintf(&builder, "user: %s\n", account.User)
	fmt.Fprintf(&builder, "homedir: %s\n", account.HomeDir)
	for _, db := range account.Databases {
		fmt.Fprintf(&builder, "database: %s\n", db)
	}
	return []byte(builder.String())
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
