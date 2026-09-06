package cpanel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClient writes a stand-in for the mysql client and returns its path.
//
// The stand-in behaves the way the real one does in the way that matters:
// it reads its own commands out of the input unless it was told not to.
// That is what makes this a test of the defence rather than of the
// spelling of an argument.
func fakeClient(t *testing.T, ran string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mysql")
	script := `#!/bin/sh
binary=no
import=no
for arg in "$@"; do
    case "$arg" in
        --print-defaults) exit 0;;
        --execute=*) echo localhost; exit 0;;
        --user=cpr_restore_*) import=yes;;
        --binary-mode) binary=yes;;
    esac
done
if [ "$import" = yes ]; then printf '%s\n' "$@" > ` + shellQuoteForTest(ran) + `; fi
while IFS= read -r line; do
    case "$line" in
        '\!'*)
            # The client runs this itself, not the server -- unless client
            # commands are off.
            if [ "$binary" = no ]; then
                sh -c "${line#'\!'}"
            else
                echo "Unknown command '\!'." >&2
                exit 1
            fi
            ;;
    esac
done
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuoteForTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// TestARestoredDumpCannotRunCommands is the one this exists for. The agent
// runs as root, and the dump comes out of a backup taken on a server this
// one does not control.
func TestARestoredDumpCannotRunCommands(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "the-dump-ran-a-command")
	argv := filepath.Join(root, "argv")

	dump := filepath.Join(root, "customer1_wp.sql")
	if err := os.WriteFile(dump, []byte(
		"\\! /bin/sh -c 'touch "+marker+"'\nSELECT 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// cPanel's own record of whose database this is, which LoadDatabase
	// asks before it loads anything.
	databases := filepath.Join(root, "databases")
	if err := os.MkdirAll(databases, 0o700); err != nil {
		t.Fatal(err)
	}
	record := `{"MYSQL":{"owner":"customer1","dbs":{"customer1_wp":"localhost"},` +
		`"dbusers":{},"noprefix":{}},"version":1}`
	if err := os.WriteFile(filepath.Join(databases, "customer1.json"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}

	real := &Real{MysqlPath: fakeClient(t, argv), DatabasesDir: databases}
	// Whether the load succeeds is not the point; whether the dump reached
	// a shell is.
	_ = real.LoadDatabase(context.Background(), "customer1", "customer1_wp", dump)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a line in the dump ran a command on this server")
	}
	body, err := os.ReadFile(argv)
	if err == nil {
		args := string(body)
		for _, want := range []string{"--binary-mode", "--local-infile=0", "--one-database"} {
			if !strings.Contains(args, want) {
				t.Errorf("the client was not given %s: %q", want, args)
			}
		}
	}
}
