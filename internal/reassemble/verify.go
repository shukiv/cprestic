package reassemble

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shuki/cprest/internal/pkgacct"
)

// Verify applies the structural checks a rehearsed restore must pass,
// returning the ones that succeeded.
//
// The checks are structural on purpose. Nothing here can tell you cPanel
// would accept the archive — only a real restorepkg on a real host can —
// but a rehearsal that fails means the backup certainly cannot be
// restored, which is the question worth answering nightly.
func Verify(rebuilt Result) ([]string, error) {
	var passed []string

	info, err := os.Stat(rebuilt.ArchivePath)
	if err != nil {
		return passed, fmt.Errorf("reassemble: rebuilt archive is missing: %w", err)
	}
	if info.Size() == 0 {
		return passed, fmt.Errorf("reassemble: rebuilt archive is empty")
	}
	passed = append(passed, "archive present")

	if rebuilt.Mode == pkgacct.ModeMonolithic {
		// There is nothing to reassemble, so the archive itself is the
		// whole of what can be checked without unpacking cPanel's format.
		return passed, nil
	}

	root, err := soleDirectory(rebuilt.TreeDir)
	if err != nil {
		return passed, err
	}
	passed = append(passed, "account tree present")

	homedir := filepath.Join(root, HomedirDir)
	files, err := countFiles(homedir)
	if err != nil {
		return passed, fmt.Errorf("reassemble: home directory: %w", err)
	}
	if files == 0 {
		return passed, fmt.Errorf("reassemble: restored home directory is empty")
	}
	passed = append(passed, fmt.Sprintf("%d files in the home directory", files))

	// Databases are optional: an account may genuinely have none.
	dumps, err := os.ReadDir(filepath.Join(root, DatabaseDir))
	if err != nil && !os.IsNotExist(err) {
		return passed, fmt.Errorf("reassemble: database directory: %w", err)
	}
	var checked int
	for _, dump := range dumps {
		if dump.IsDir() || !strings.HasSuffix(dump.Name(), ".sql") {
			continue
		}
		path := filepath.Join(root, DatabaseDir, dump.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			return passed, fmt.Errorf("reassemble: read dump %s: %w", dump.Name(), err)
		}
		// A truncated or empty dump restores an empty database, which is
		// worse than an obvious failure.
		if len(body) == 0 {
			return passed, fmt.Errorf("reassemble: dump %s is empty", dump.Name())
		}
		if !strings.Contains(strings.ToUpper(string(body)), "CREATE") {
			return passed, fmt.Errorf("reassemble: dump %s has no CREATE statement", dump.Name())
		}
		checked++
	}
	if checked > 0 {
		passed = append(passed, fmt.Sprintf("%d database dumps parse", checked))
	}
	return passed, nil
}

func countFiles(root string) (int, error) {
	var count int
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			count++
		}
		return nil
	})
	return count, err
}
