package destination

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Local is a repository on a local filesystem or a mounted network share.
type Local struct {
	// Root is the absolute directory holding one subdirectory per repository.
	Root string
}

var _ Destination = (*Local)(nil)

func (l *Local) Type() Type { return TypeLocal }

func (l *Local) URI(repoPath string) (string, error) {
	if !filepath.IsAbs(l.Root) {
		return "", fmt.Errorf("local: root %q must be an absolute path", l.Root)
	}
	cleaned, err := CleanRepoPath(repoPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(l.Root, cleaned), nil
}

// Env returns no variables: the local backend needs no credentials.
func (l *Local) Env() (map[string]string, error) { return map[string]string{}, nil }

// Options returns none: the local backend needs no tuning.
func (l *Local) Options() (map[string]string, error) { return map[string]string{}, nil }

func (l *Local) Preflight(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(l.Root) {
		return fmt.Errorf("local: root %q must be an absolute path", l.Root)
	}
	info, err := os.Stat(l.Root)
	if err != nil {
		return fmt.Errorf("local: stat root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("local: root %q is not a directory", l.Root)
	}
	return nil
}
