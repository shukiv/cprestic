package inventory

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/shukiv/gniza/internal/resticrun"
)

// contents is what one snapshot's containers hold, read once.
type contents struct {
	// members are the names inside the account's metadata archive,
	// relative to its own top-level directory.
	members []string
	// bodies are the few members small enough, and useful enough, to keep
	// whole: the ones that are a list rather than a directory of files.
	bodies map[string][]byte
	// auth and grants are the account's database users, as the backup
	// staged them beside the dumps.
	auth   map[string]map[string]Auth
	grants map[string][]Grant
}

// wholeMembers are read for their contents rather than for their name,
// because what they hold is a list and their name is not.
func wholeMember(name string) bool {
	return strings.HasPrefix(name, "cron/") || name == "proftpdpasswd"
}

// readArchive streams the account's metadata archive out of the snapshot
// and reads the names, and a few of the bodies, out of it.
func readArchive(ctx context.Context, reader Reader, src Source) (
	members []string, bodies map[string][]byte, err error) {

	archive, err := archivePath(ctx, reader, src)
	if err != nil {
		return nil, nil, err
	}

	pipeReader, pipeWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := reader.Dump(ctx, src.Repo, src.SnapshotID, archive, pipeWriter)
		pipeWriter.CloseWithError(err)
		done <- err
	}()

	members, bodies, scanErr := scanArchive(pipeReader)
	if scanErr == nil {
		// A tar reader stops at the end-of-archive marker, and restic is
		// still writing the padding after it. Closing the pipe here would
		// fail that write and turn a complete reading into an error.
		_, _ = io.Copy(io.Discard, pipeReader)
	}
	// A scan that stopped early leaves the dump with nowhere to write, and
	// it would block there for as long as the repository takes to read.
	pipeReader.CloseWithError(io.EOF)
	dumpErr := <-done

	if scanErr != nil {
		return nil, nil, scanErr
	}
	if dumpErr != nil {
		return nil, nil, dumpErr
	}
	return members, bodies, nil
}

func scanArchive(stream io.Reader) ([]string, map[string][]byte, error) {
	var members []string
	bodies := map[string][]byte{}

	archive := tar.NewReader(stream)
	for count := 0; count < maxHeaders; count++ {
		header, err := archive.Next()
		if err == io.EOF {
			return members, bodies, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("inventory: read the account archive: %w", err)
		}
		// Names inside a cpmove archive are under a top-level directory
		// named after the account, which is not part of what anything
		// asks for.
		name := header.Name
		if slash := strings.IndexByte(name, '/'); slash >= 0 {
			name = name[slash+1:]
		}
		if name == "" {
			continue
		}
		members = append(members, name)
		if header.Typeflag == tar.TypeReg && header.Size > 0 &&
			header.Size <= maxBody && wholeMember(name) {
			body, err := io.ReadAll(io.LimitReader(archive, maxBody))
			if err != nil {
				return nil, nil, fmt.Errorf("inventory: read %s: %w", name, err)
			}
			bodies[name] = body
		}
	}
	return nil, nil, fmt.Errorf(
		"inventory: this backup's account archive holds more parts than can be listed")
}

// archivePath finds the account archive inside the snapshot's metadata
// part. pkgacct names it after the account, so it is discovered rather
// than assumed.
func archivePath(ctx context.Context, reader Reader, src Source) (string, error) {
	if src.Parts.Metadata == "" {
		return "", fmt.Errorf(
			"inventory: this backup holds none of the account's configuration")
	}
	entries, err := reader.Ls(ctx, src.Repo, src.SnapshotID, src.Parts.Metadata)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name, ".tar") {
			return path.Join(src.Parts.Metadata, entry.Name), nil
		}
	}
	return "", fmt.Errorf(
		"inventory: the configuration in this backup holds no account archive")
}

// readFile streams one file out of the snapshot, up to the body cap.
func readFile(ctx context.Context, reader Reader, repo resticrun.Repository,
	snapshotID, file string) ([]byte, error) {

	pipeReader, pipeWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := reader.Dump(ctx, repo, snapshotID, file, pipeWriter)
		pipeWriter.CloseWithError(err)
		done <- err
	}()
	body, readErr := io.ReadAll(io.LimitReader(pipeReader, maxBody))
	if readErr == nil {
		// A file past the cap has more behind it, and the dump writing
		// that remainder into a closed pipe would fail a reading that
		// gave us everything we asked for.
		_, _ = io.Copy(io.Discard, pipeReader)
	}
	pipeReader.CloseWithError(io.EOF)
	dumpErr := <-done

	if readErr != nil {
		return nil, fmt.Errorf("inventory: read %s: %w", path.Base(file), readErr)
	}
	if dumpErr != nil {
		return nil, dumpErr
	}
	return body, nil
}
