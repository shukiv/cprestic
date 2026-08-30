// Package resticrun executes the restic binary.
//
// There is one runner for every destination type. Destinations supply the
// repository URI and backend environment (see internal/destination); this
// package owns argument construction, credential handling, execution and
// JSON parsing, so that logic exists once rather than once per backend.
package resticrun

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// Command is a single restic invocation.
type Command struct {
	Path string
	Args []string
	// Env is the complete environment for the child process, in "K=V"
	// form. Credentials travel here and never in Args, because
	// /proc/<pid>/cmdline is world-readable.
	Env []string
	Dir string
}

// CommandResult is the outcome of an invocation that actually ran. A
// non-zero ExitCode is reported here rather than as an error, so callers
// can distinguish restic's partial-success codes from a failure to start.
type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Execer runs a Command. Tests substitute a fake so the suite needs no
// restic binary.
type Execer interface {
	Exec(ctx context.Context, cmd Command) (CommandResult, error)
}

// ExecFunc adapts a function to Execer.
type ExecFunc func(ctx context.Context, cmd Command) (CommandResult, error)

func (f ExecFunc) Exec(ctx context.Context, cmd Command) (CommandResult, error) {
	return f(ctx, cmd)
}

// OSExec runs commands as real child processes.
type OSExec struct {
	// MaxOutputBytes caps captured stdout and stderr. Zero means 8 MiB.
	MaxOutputBytes int
}

var _ Execer = (*OSExec)(nil)

func (o *OSExec) Exec(ctx context.Context, cmd Command) (CommandResult, error) {
	limit := o.MaxOutputBytes
	if limit <= 0 {
		limit = 8 << 20
	}

	c := exec.CommandContext(ctx, cmd.Path, cmd.Args...)
	c.Env = cmd.Env
	c.Dir = cmd.Dir

	var stdout, stderr bytes.Buffer
	c.Stdout = &cappedWriter{buf: &stdout, limit: limit}
	c.Stderr = &cappedWriter{buf: &stderr, limit: limit}

	err := c.Run()
	result := CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return result, nil
	case errors.As(err, &exitErr):
		// The process ran and exited non-zero. That is a result, not a
		// failure to execute; the caller decides what the code means.
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	default:
		return result, err
	}
}

// cappedWriter discards output past a limit so a pathological restic run
// cannot exhaust the agent's memory.
type cappedWriter struct {
	buf   *bytes.Buffer
	limit int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if remaining := w.limit - w.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			w.buf.Write(p[:remaining])
		} else {
			w.buf.Write(p)
		}
	}
	// Always report a full write: truncation is intentional and must not
	// look like a broken pipe to the child process.
	return len(p), nil
}
