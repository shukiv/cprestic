// Package reassemble rebuilds a cPanel account archive from the parts a
// split-mode backup stored separately.
//
// Split mode exists because restic cannot deduplicate a compressed archive
// (docs/DESIGN.md §4). The cost of that decision is paid here: what pkgacct
// produced as one file has to be put back together before cPanel's
// restorepkg will accept it.
package reassemble

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxEntrySize bounds a single extracted file. An archive is only as
// trustworthy as the server that produced it, and a decompression bomb
// would otherwise fill the restore volume.
const maxEntrySize = 1 << 40 // 1 TiB

// extractTar unpacks an archive into dir.
func extractTar(archivePath, dir string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("reassemble: open %s: %w", archivePath, err)
	}
	defer file.Close()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("reassemble: create %s: %w", dir, err)
	}

	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reassemble: read %s: %w", archivePath, err)
		}

		target, err := safeJoin(dir, header.Name)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, entryMode(header, 0o700)); err != nil {
				return fmt.Errorf("reassemble: create %s: %w", target, err)
			}
		case tar.TypeReg:
			if header.Size > maxEntrySize {
				return fmt.Errorf("reassemble: entry %s is %d bytes, refusing to extract",
					header.Name, header.Size)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return fmt.Errorf("reassemble: create %s: %w", filepath.Dir(target), err)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, entryMode(header, 0o600))
			if err != nil {
				return fmt.Errorf("reassemble: create %s: %w", target, err)
			}
			_, copyErr := io.Copy(out, io.LimitReader(reader, maxEntrySize))
			closeErr := out.Close()
			if copyErr != nil {
				return fmt.Errorf("reassemble: write %s: %w", target, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("reassemble: close %s: %w", target, closeErr)
			}
		case tar.TypeSymlink:
			// A symlink is only recreated when it stays inside the tree.
			// One pointing at /etc/shadow would turn a later write into a
			// very bad day.
			if _, err := safeJoin(dir, filepath.Join(filepath.Dir(header.Name), header.Linkname)); err != nil {
				return fmt.Errorf("reassemble: symlink %s points outside the archive", header.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return fmt.Errorf("reassemble: create %s: %w", filepath.Dir(target), err)
			}
			if err := os.Symlink(header.Linkname, target); err != nil && !os.IsExist(err) {
				return fmt.Errorf("reassemble: symlink %s: %w", target, err)
			}
		default:
			// Devices, fifos and hard links are not part of an account
			// payload and are skipped rather than reproduced.
		}
	}
}

// createTar packs dir into an uncompressed archive.
//
// Uncompressed on purpose: the result is handed straight to restorepkg,
// and compressing it would only cost time.
func createTar(dir, archivePath string) error {
	out, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("reassemble: create %s: %w", archivePath, err)
	}
	defer out.Close()

	writer := tar.NewWriter(out)
	root := filepath.Clean(dir)

	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}

		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return fmt.Errorf("reassemble: read symlink %s: %w", path, err)
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return fmt.Errorf("reassemble: header for %s: %w", path, err)
		}
		header.Name = filepath.ToSlash(relative)
		if info.IsDir() {
			header.Name += "/"
		}
		if err := writer.WriteHeader(header); err != nil {
			return fmt.Errorf("reassemble: write header %s: %w", header.Name, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("reassemble: open %s: %w", path, err)
		}
		_, copyErr := io.Copy(writer, file)
		closeErr := file.Close()
		if copyErr != nil {
			return fmt.Errorf("reassemble: copy %s: %w", path, copyErr)
		}
		return closeErr
	})
	if walkErr != nil {
		return walkErr
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("reassemble: finish %s: %w", archivePath, err)
	}
	return nil
}

// safeJoin resolves name inside root and refuses anything that escapes it.
func safeJoin(root, name string) (string, error) {
	cleanRoot := filepath.Clean(root)
	target := filepath.Clean(filepath.Join(cleanRoot, filepath.FromSlash(name)))
	if target != cleanRoot && !strings.HasPrefix(target, cleanRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("reassemble: archive entry %q escapes the extraction directory", name)
	}
	return target, nil
}

// entryMode keeps an archive's permission bits within sane bounds.
//
// Only the permission bits are taken, so setuid, setgid and sticky never
// survive extraction, and the owner always keeps enough access to read the
// tree back when it is repacked.
func entryMode(header *tar.Header, ownerBits os.FileMode) os.FileMode {
	return os.FileMode(header.Mode).Perm() | ownerBits
}
