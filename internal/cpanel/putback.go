package cpanel

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/shuki/cprest/internal/granular"
)

// PutHomeDir copies a restored tree into an account's home directory.
//
// The tree is in staging, which is root's; the home directory is the
// account's. The copy is therefore split in two: root reads, and the
// account writes. Nothing here runs as root inside the home directory, so
// the files that land there are owned by the account without a chown pass,
// and a link planted in the home directory before the restore can lead
// nowhere the account could not already write.
func (r *Real) PutHomeDir(ctx context.Context, user, from string) error {
	account, err := osuser.Lookup(user)
	if err != nil {
		return fmt.Errorf("cpanel: look up %s: %w", user, err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return fmt.Errorf("cpanel: %s has no numeric uid: %w", user, err)
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return fmt.Errorf("cpanel: %s has no numeric gid: %w", user, err)
	}
	// Not a guard against a hostile caller -- one that reached here has
	// root already -- but against a mistake that would write a customer's
	// backup over a system account's files.
	if uid == 0 {
		return fmt.Errorf("cpanel: %s is not a cPanel account", user)
	}
	home := filepath.Clean(account.HomeDir)
	if home == "" || home == "." || home == "/" || filepath.Dir(home) == home {
		return fmt.Errorf("cpanel: %s has no home directory of its own", user)
	}
	info, err := os.Stat(home)
	if err != nil {
		return fmt.Errorf("cpanel: home directory of %s: %w", user, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cpanel: home directory of %s is not a directory", user)
	}

	if err := dropSetIDBits(from); err != nil {
		return err
	}

	// GNU tar on both ends: the reader keeps the tree's shape and modes,
	// and the writer is the account, so ownership follows from who it is
	// rather than from what the archive claims.
	read := exec.CommandContext(ctx, "tar", "-C", from, "--numeric-owner", "-cf", "-", ".")
	write := exec.CommandContext(ctx, "tar", "-C", home, "-xpf", "-", "--no-same-owner")
	write.SysProcAttr = &syscall.SysProcAttr{
		// Groups is left empty on purpose: without it the child would
		// keep root's supplementary groups while carrying the account's
		// uid, which is more than the account has.
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
	}

	pipeRead, pipeWrite, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("cpanel: pipe: %w", err)
	}
	read.Stdout = pipeWrite
	write.Stdin = pipeRead
	var readErr, writeErr bytes.Buffer
	read.Stderr = &readErr
	write.Stderr = &writeErr

	if err := read.Start(); err != nil {
		pipeRead.Close()
		pipeWrite.Close()
		return fmt.Errorf("cpanel: read the restored tree: %w", err)
	}
	if err := write.Start(); err != nil {
		pipeRead.Close()
		pipeWrite.Close()
		_ = read.Process.Kill()
		_, _ = read.Process.Wait()
		return fmt.Errorf("cpanel: write into %s: %w", home, err)
	}
	// The parent's ends have been handed to the children. Holding them
	// open would keep the writer waiting for an end of file that the
	// finished reader has already stopped producing.
	pipeWrite.Close()
	pipeRead.Close()

	readWait := read.Wait()
	writeWait := write.Wait()
	// The writer's failure is reported first on purpose. A full disk or a
	// quota stops the writer, the reader then finds a closed pipe, and
	// "broken pipe" is not what went wrong.
	if writeWait != nil {
		return fmt.Errorf("cpanel: write into %s as %s: %s: %w",
			home, user, lastLine(writeErr.Bytes()), writeWait)
	}
	if readWait != nil {
		return fmt.Errorf("cpanel: read the restored tree: %s: %w",
			lastLine(readErr.Bytes()), readWait)
	}
	return nil
}

// dropSetIDBits clears set-user-ID and set-group-ID from a staged tree
// before it is written into an account.
//
// The account could set those bits itself on its own files, so this is not
// what stops it. It stops a bit that was set when the backup was taken from
// coming back after the reason for removing it -- a compromise, a policy
// change -- has been dealt with.
func dropSetIDBits(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&(os.ModeSetuid|os.ModeSetgid) == 0 {
			return nil
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		// Perm() is the nine permission bits and nothing else, so writing
		// it back is what drops the two.
		return os.Chmod(path, info.Mode().Perm())
	})
}

// LoadDatabase replaces the contents of one of the account's databases with
// a dump taken from a backup.
func (r *Real) LoadDatabase(ctx context.Context, user, database, dumpPath string) error {
	if err := granular.UsableDatabaseName(database); err != nil {
		return err
	}
	// Whose database this is, asked of the server rather than taken from
	// the request. The name in the backup is the name it had when the
	// backup was taken, and a name can have changed hands since.
	owned, err := r.databases(ctx, user)
	if err != nil {
		return err
	}
	found := false
	for _, name := range owned {
		if name == database {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf(
			"cpanel: the database %s does not belong to %s", database, user)
	}

	dump, err := os.Open(dumpPath)
	if err != nil {
		return fmt.Errorf("cpanel: database dump: %w", err)
	}
	defer dump.Close()
	info, err := dump.Stat()
	if err != nil {
		return fmt.Errorf("cpanel: database dump: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cpanel: database dump %s is not a file", dumpPath)
	}

	// --one-database confines the load to the database named here: a
	// statement that switched to another one would be ignored rather than
	// run. The dumps this reads are single-database dumps that never
	// switch, and this keeps that true whatever produced the file.
	load := exec.CommandContext(ctx, r.mysql(), "--one-database", database)
	load.Stdin = dump
	var stderr bytes.Buffer
	load.Stderr = &stderr
	if err := load.Run(); err != nil {
		return fmt.Errorf("cpanel: load %s from %s: %s: %w",
			database, filepath.Base(dumpPath), lastLine(stderr.Bytes()), err)
	}
	return nil
}
