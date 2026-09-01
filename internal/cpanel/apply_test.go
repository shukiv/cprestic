package cpanel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRestorepkg is a stand-in for cPanel's script that records the
// arguments it was given.
func fakeRestorepkg(t *testing.T) (script, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "args")
	script = filepath.Join(dir, "restorepkg")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, record
}

// TestApplyAsksForARestrictedRestore covers the default this program
// deliberately differs from cPanel on. The archive being unpacked holds a
// customer's own home directory, and restorepkg runs as root: an account
// compromised at any point since its last backup could have left
// something in there for exactly this moment. cPanel's Restricted Restore
// is for that, and cPanel's own default is off.
func TestApplyAsksForARestrictedRestore(t *testing.T) {
	script, record := fakeRestorepkg(t)
	archive := filepath.Join(t.TempDir(), "cpmove-customer1.tar")
	if err := os.WriteFile(archive, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	host := &Real{RestorepkgPath: script}

	if err := host.Apply(context.Background(), archive, ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	args := readArgs(t, record)
	if !args["--restricted"] {
		t.Error("a restore with no options asked cPanel for an unrestricted restore")
	}
	if args["--unrestricted"] {
		t.Error("both modes were asked for at once")
	}
	if args["--force"] {
		t.Error("a restore that was not asked to overwrite anything asked to overwrite")
	}
	if !args[archive] {
		t.Errorf("the archive was not passed: %v", args)
	}

	// An operator restoring onto an account that is still here has asked
	// for it to be replaced. Without --force restorepkg leaves what is
	// there alone and reports nothing wrong, so the restore the
	// interface promised silently did not happen.
	if err := host.Apply(context.Background(), archive,
		ApplyOptions{Overwrite: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if args := readArgs(t, record); !args["--force"] {
		t.Errorf("a restore onto a live account did not ask to overwrite it: %v", args)
	}
}

// TestAnOperatorCanStillAskForAnUnrestrictedRestore covers the other side:
// restricted mode refuses some legitimate archive content, so the choice
// has to exist or an operator with a real archive cannot restore it.
func TestAnOperatorCanStillAskForAnUnrestrictedRestore(t *testing.T) {
	script, record := fakeRestorepkg(t)
	archive := filepath.Join(t.TempDir(), "cpmove-customer1.tar")
	if err := os.WriteFile(archive, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	host := &Real{RestorepkgPath: script}

	if err := host.Apply(context.Background(), archive,
		ApplyOptions{Unrestricted: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	args := readArgs(t, record)
	if !args["--unrestricted"] || args["--restricted"] {
		t.Errorf("the operator's choice was not passed on: %v", args)
	}
}

func readArgs(t *testing.T, record string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("restorepkg was not run: %v", err)
	}
	args := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		args[line] = true
	}
	return args
}
