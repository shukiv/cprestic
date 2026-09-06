package resticrun

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/destination"
)

// TestTheLogNeverCarriesACredential is the rule this logging exists under.
// A log an operator reads to debug a backup must not be a place a
// repository password, a backend key or a password file's path turns up:
// the log is downloadable from the interface and pasteable into an issue.
func TestTheLogNeverCarriesACredential(t *testing.T) {
	const password = "repository-password-do-not-log"
	var written bytes.Buffer
	runner := New(Config{
		Binary:     "/bin/true",
		RuntimeDir: t.TempDir(),
		Log:        slog.New(slog.NewTextHandler(&written, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, execFunc(func(context.Context, Command) (CommandResult, error) {
		return CommandResult{ExitCode: 0}, nil
	}))

	dest, err := destination.Build(destination.Spec{
		Type:   destination.TypeLocal,
		Config: map[string]string{"root": t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Check(context.Background(), Repository{
		Dest: dest, Path: "customer1", Password: password,
	}, CheckSpec{}); err != nil {
		t.Fatalf("check: %v", err)
	}

	log := written.String()
	if log == "" {
		t.Fatal("nothing was written at debug")
	}
	for _, forbidden := range []string{password, "RESTIC_PASSWORD_FILE", "RESTIC_REPOSITORY="} {
		if strings.Contains(log, forbidden) {
			t.Errorf("the log carries %q:\n%s", forbidden, log)
		}
	}
	if !strings.Contains(log, "ran restic") {
		t.Errorf("the invocation was not written down:\n%s", log)
	}
}

// TestAnAddressWithALoginInItIsNotWrittenDown covers the backends whose
// repository address can carry a user and password.
func TestAnAddressWithALoginInItIsNotWrittenDown(t *testing.T) {
	for address, want := range map[string]string{
		"rest:https://user:secret@backup.example/repo": "rest:[credentials]@backup.example/repo",
		"sftp:gniza@backup.example:/srv/backups":       "sftp:[credentials]@backup.example:/srv/backups",
		"/srv/local/repo":                              "/srv/local/repo",
	} {
		if got := safeURI(address); got != want {
			t.Errorf("safeURI(%q) = %q, want %q", address, got, want)
		}
		if strings.Contains(safeURI(address), "secret") {
			t.Errorf("a password survived in %q", safeURI(address))
		}
	}
}

// TestABackendOptionKeepsItsNameAndLosesItsValue: an option can carry a
// path or a token, and the key alone says which option was set.
func TestABackendOptionKeepsItsNameAndLosesItsValue(t *testing.T) {
	got := strings.Join(elideOptionValues([]string{
		"-o", "sftp.args=-i /etc/gniza/keys/private", "check", "--read-data-subset", "5%",
	}), " ")
	want := "-o sftp.args=[set] check --read-data-subset 5%"
	if got != want {
		t.Fatalf("elideOptionValues gave %q, want %q", got, want)
	}
}

type execFunc func(context.Context, Command) (CommandResult, error)

func (f execFunc) Exec(ctx context.Context, c Command) (CommandResult, error) { return f(ctx, c) }
