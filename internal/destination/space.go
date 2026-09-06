package destination

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Space is how much room the storage behind a destination has.
//
// Free is what is available to the account writing the backups, which on a
// Unix filesystem is not the same as unused: the last few percent are
// reserved for root, and a backup that fills a disk to that line has run
// out even though df still shows space. Bavail is the honest number.
type Space struct {
	TotalBytes uint64
	FreeBytes  uint64
}

// UsedBytes is what is on the storage already, ours and everybody else's.
// A destination is rarely a disk of our own.
func (s Space) UsedBytes() uint64 {
	if s.TotalBytes < s.FreeBytes {
		return 0
	}
	return s.TotalBytes - s.FreeBytes
}

// Sizer is a destination that can say how much room it has.
//
// Not every kind can. A filesystem has a size; an S3 bucket does not, and a
// restic REST server does not tell us the size of the disk underneath it.
// Callers type-assert for this and say plainly when the answer is that
// there is no answer, rather than showing a zero that reads as "full".
type Sizer interface {
	Space(ctx context.Context) (Space, error)
}

// Space reports the filesystem holding the destination's root.
func (l *Local) Space(ctx context.Context) (Space, error) {
	if err := ctx.Err(); err != nil {
		return Space{}, err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(l.Root, &stat); err != nil {
		return Space{}, fmt.Errorf("local: statfs %s: %w", l.Root, err)
	}
	size := uint64(stat.Bsize)
	return Space{
		TotalBytes: stat.Blocks * size,
		FreeBytes:  stat.Bavail * size,
	}, nil
}

// spaceTimeout bounds the remote df. A backup server that has stopped
// answering must not hold up the page that would have told the operator so.
const spaceTimeout = 15 * time.Second

// Space asks the far end how much room it has, with the same key and the
// same host-key pinning restic itself uses. It runs df and nothing else:
// this is the one place Gniza executes a command on a machine it does not
// otherwise touch, and it stays a question rather than a change.
func (s *SFTP) Space(ctx context.Context) (Space, error) {
	if err := s.validate(); err != nil {
		return Space{}, err
	}
	if s.KnownHostsFile == "" {
		return Space{}, fmt.Errorf("sftp: known-hosts file is required; refusing to trust an unpinned host key")
	}
	ctx, cancel := context.WithTimeout(ctx, spaceTimeout)
	defer cancel()

	output, err := exec.CommandContext(ctx, "ssh", s.dfArgs()...).Output()
	if err != nil {
		return Space{}, fmt.Errorf("sftp: df on %s: %w", s.Host, err)
	}
	return parseDF(string(output))
}

// parseDF reads POSIX df output, whose first line is a header and whose
// device column may be long enough that df wraps the row onto two lines.
// Fields are counted from the end for that reason.
func parseDF(output string) (Space, error) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		// Five, not six: a wrapped row's second line has no device column.
		if len(fields) < 5 || fields[len(fields)-1] == "on" {
			continue
		}
		// ... <1024-blocks> <used> <available> <capacity> <mounted on>
		blocks, err := strconv.ParseUint(fields[len(fields)-5], 10, 64)
		if err != nil {
			continue
		}
		available, err := strconv.ParseUint(fields[len(fields)-3], 10, 64)
		if err != nil {
			continue
		}
		return Space{TotalBytes: blocks * 1024, FreeBytes: available * 1024}, nil
	}
	if err := scanner.Err(); err != nil {
		return Space{}, fmt.Errorf("sftp: read df output: %w", err)
	}
	return Space{}, fmt.Errorf("sftp: could not read df output")
}

// dfArgs builds the ssh invocation. Two things here are load-bearing.
//
// The "--" comes before the host, not after it. ssh parses options until
// it sees one, so a host of "-oProxyCommand=..." is an option rather than
// a name — and ProxyCommand runs on this machine, as this process, which
// is root. With the "--" in front, ssh reads it as a hostname and refuses
// it. That is the difference between a bad configuration and local code
// execution.
//
// The remote path is single-quoted because ssh does not run the command
// it is given: it joins the remaining arguments with spaces and hands the
// string to a shell on the far end. A root of "/srv; rm -rf /" is two
// commands there, and "df -Pk -- /srv" does not protect against that —
// the "--" is read by df, long after the shell has finished splitting.
func (s *SFTP) dfArgs() []string {
	args := []string{
		"-i", s.IdentityFile,
		"-o", "UserKnownHostsFile=" + s.KnownHostsFile,
		"-o", "StrictHostKeyChecking=yes",
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-l", s.User,
	}
	if s.Port != 0 && s.Port != 22 {
		args = append(args, "-p", strconv.Itoa(s.Port))
	}
	return append(args, "--", s.Host, "df", "-Pk", "--", shellQuote(s.Root))
}

// shellQuote wraps a value so a remote shell reads it as one word. The
// only character that matters inside single quotes is the single quote
// itself, which is closed, escaped and reopened.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// ParseDFForTest exposes the df reader to the package's tests. The parsing
// is the part that can be wrong against a machine we do not control, and
// running a real ssh from a unit test is not a way to find that out.
func ParseDFForTest(output string) (Space, error) { return parseDF(output) }

// DFArgsForTest exposes the ssh invocation, so the argument order that
// keeps a hostname from becoming an option is checked rather than assumed.
func DFArgsForTest(s *SFTP) []string { return s.dfArgs() }
