package reassemble

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// ValidateAccountArchive binds cPanel's embedded identity to the requested
// account. A restic tag and an archive filename are not authoritative: both
// can say customer1 while restorepkg reads cp/victim from inside the archive.
// Check the entire member list, including duplicate identity records, without
// extracting it. The caller must keep the archive in root-owned staging.
func ValidateAccountArchive(ctx context.Context, filename, account string) error {
	if account == "" || strings.ContainsAny(account, "/\\.\x00") {
		return fmt.Errorf("reassemble: invalid expected account %q", account)
	}
	base := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(filename), ".gz"), ".tar")
	if base != "cpmove-"+account && base != account {
		return fmt.Errorf("reassemble: archive filename does not belong to %s", account)
	}
	f, err := os.OpenFile(filename, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("reassemble: account archive is not a regular file")
	}
	var reader io.Reader = f
	if strings.HasSuffix(filename, ".gz") {
		z, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("reassemble: read compressed account archive: %w", err)
		}
		defer z.Close()
		reader = z
	}
	tr := tar.NewReader(reader)
	root, identity := "", false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reassemble: read account archive: %w", err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name == "." && h.Typeflag == tar.TypeDir {
			continue
		}
		for _, component := range strings.Split(name, "/") {
			if component == ".." {
				return fmt.Errorf("reassemble: unsafe account archive member %q", h.Name)
			}
		}
		name = path.Clean(name)
		top, within, _ := strings.Cut(name, "/")
		if top != "cpmove-"+account && top != account {
			return fmt.Errorf("reassemble: archive member %q does not belong to %s", h.Name, account)
		}
		if root != "" && root != top {
			return fmt.Errorf("reassemble: account archive has multiple roots")
		}
		root = top
		if (within == "" || within == "cp" || within == "meta") && h.Typeflag != tar.TypeDir {
			return fmt.Errorf("reassemble: account identity directory is not a directory: %s", h.Name)
		}
		if strings.HasPrefix(within, "cp/") || within == "meta/user" {
			if within != "cp/"+account && within != "meta/user" {
				return fmt.Errorf("reassemble: archive contains a different account record: %s", h.Name)
			}
			if h.Typeflag != tar.TypeReg || h.Size > 1<<20 {
				return fmt.Errorf("reassemble: invalid account identity record %s", h.Name)
			}
			body, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			if within == "meta/user" && strings.TrimSpace(string(body)) != account {
				return fmt.Errorf("reassemble: archive's account identity is not %s", account)
			}
			if within == "cp/"+account {
				for _, line := range strings.Split(string(body), "\n") {
					if value, ok := strings.CutPrefix(line, "USER="); ok && strings.TrimSpace(value) != account {
						return fmt.Errorf("reassemble: archive's USER field is not %s", account)
					}
				}
			}
			identity = true
		}
	}
	if !identity {
		return fmt.Errorf("reassemble: archive has no identity record for %s", account)
	}
	return nil
}
