package cpanel

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// This file reads the exclusions cPanel's own backups obey.
//
// An operator who has written a path into cpbackup-exclude.conf has said
// they do not want it leaving the server. cprest ignoring that file meant
// the very files they excluded were the ones being uploaded to a remote
// destination -- and the operator had no way to know, because the file
// they wrote it in is the one cPanel documents for exactly this.

// ExcludeConfName is the file an account keeps its own exclusions in, in
// its home directory. cPanel reads the same name.
const ExcludeConfName = "cpbackup-exclude.conf"

// serverExcludeConf is where the server-wide list lives.
var serverExcludeConf = "/etc/cpbackup-exclude.conf"

// NativeExcludes is what cPanel would leave out of a backup of this
// account: the server-wide list, plus the account's own.
//
// The patterns are returned as restic exclude patterns anchored under the
// account's home directory. cPanel matches them anywhere below the home,
// and anchoring keeps them from also matching inside the staged metadata
// and database dumps, which are this program's own files rather than the
// account's.
func (r *Real) NativeExcludes(home string) []string {
	if home == "" {
		return nil
	}
	seen := map[string]bool{}
	var excludes []string
	for _, path := range []string{r.serverExcludes(), filepath.Join(home, ExcludeConfName)} {
		for _, pattern := range readExcludeConf(path) {
			for _, mapped := range anchorExclude(home, pattern) {
				if !seen[mapped] {
					seen[mapped] = true
					excludes = append(excludes, mapped)
				}
			}
		}
	}
	return excludes
}

func (r *Real) serverExcludes() string {
	if r.ServerExcludeConf != "" {
		return r.ServerExcludeConf
	}
	return serverExcludeConf
}

// readExcludeConf reads one file's worth of patterns.
//
// A missing file is not an error: most servers have no per-account list,
// and an account that has never written one has excluded nothing.
func readExcludeConf(path string) []string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	var patterns []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}

// anchorExclude turns one cPanel pattern into restic patterns under the
// account's home.
//
// cPanel matches a bare name at any depth below the home, so that is what
// this produces: the name directly in the home, and the same name
// anywhere below it. A pattern that already begins with a slash is
// relative to the home and matches only there.
func anchorExclude(home, pattern string) []string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil
	}
	if strings.HasPrefix(pattern, "/") {
		return []string{filepath.Join(home, pattern)}
	}
	// Leading "*/" is cPanel's way of saying "below the top", which is
	// already what matching at any depth means here.
	trimmed := strings.TrimPrefix(pattern, "*/")
	if trimmed == "" {
		return nil
	}
	return []string{
		filepath.Join(home, trimmed),
		filepath.Join(home, "**", trimmed),
	}
}
