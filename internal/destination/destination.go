// Package destination models storage endpoints that restic can write to.
//
// Destinations are pure configuration. They translate operator-supplied
// settings into the repository URI and the environment variables restic
// needs, and they report whether the endpoint looks usable. They never
// execute restic: that is the job of internal/resticrun, which is shared
// by every destination type so the exec, argument-building and JSON
// parsing logic exists exactly once.
package destination

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
)

// Type identifies a destination implementation.
type Type string

const (
	TypeLocal Type = "local"
	TypeSFTP  Type = "sftp"
	TypeREST  Type = "rest"
	TypeS3    Type = "s3"
)

// ErrUnsupportedType is returned when a destination type has no implementation.
var ErrUnsupportedType = errors.New("destination: unsupported type")

// Destination is a storage endpoint plus the credentials to reach it.
//
// A single destination normally holds many restic repositories, one per
// source server, distinguished by repoPath. See docs/DESIGN.md §5.
type Destination interface {
	// Type reports the destination's implementation kind.
	Type() Type

	// URI returns the RESTIC_REPOSITORY value for a repository living at
	// repoPath inside this destination.
	URI(repoPath string) (string, error)

	// Env returns backend credentials as environment variables for the
	// restic child process. Credentials are never passed as arguments,
	// because /proc/<pid>/cmdline is world-readable on the multi-tenant
	// servers this runs on.
	Env() (map[string]string, error)

	// Options returns restic extended options, passed as "-o key=value".
	// These configure backend behaviour rather than authenticate, so
	// unlike Env they may safely appear in the argument list. Most
	// destinations need none.
	Options() (map[string]string, error)

	// Preflight checks that the endpoint is configured and reachable. It
	// must not create, modify or read repository contents.
	Preflight(ctx context.Context) error
}

// CleanRepoPath validates and normalises a repository path supplied by an
// operator. Repository paths are joined into URIs and filesystem paths, so
// traversal and absolute-path escapes are rejected here rather than at each
// call site.
//
// The returned path has no leading or trailing slash: "cp01", "eu/cp01".
func CleanRepoPath(repoPath string) (string, error) {
	trimmed := strings.Trim(strings.TrimSpace(repoPath), "/")
	if trimmed == "" {
		return "", errors.New("destination: repository path is empty")
	}
	if strings.ContainsRune(trimmed, '\\') {
		return "", fmt.Errorf("destination: repository path %q contains a backslash", repoPath)
	}
	if strings.ContainsRune(trimmed, 0) {
		return "", errors.New("destination: repository path contains a null byte")
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("destination: repository path %q escapes its destination", repoPath)
	}
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("destination: repository path %q has an invalid segment", repoPath)
		}
	}
	return cleaned, nil
}
