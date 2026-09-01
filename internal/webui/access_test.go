package webui_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shuki/cprest/internal/granular"
	"github.com/shuki/cprest/internal/webui"
)

// TestAnAccountCannotAskForTheThingsThePageDoesNotOffer covers a gap
// between what the customer page shows and what its form handler accepted.
// The page offered a short list; the handler took whatever was posted. The
// settings archive holds shadow, digestshadow and cPanel's own metadata,
// so anyone who could read the form could pull their account's password
// hashes out of a backup by editing one field.
func TestAnAccountCannotAskForTheThingsThePageDoesNotOffer(t *testing.T) {
	for _, kind := range []granular.Kind{granular.KindFiles, granular.KindDatabase, granular.KindDNS} {
		if !webui.IsUserKindForTest(string(kind)) {
			t.Errorf("%s is on the customer page but the handler refuses it", kind)
		}
	}
	for _, kind := range []string{string(granular.KindSettings), "", "../settings", "account"} {
		if webui.IsUserKindForTest(kind) {
			t.Errorf("an account was allowed to ask for %q", kind)
		}
	}
}

// TestASuspendedAccountIsNotServed covers both marks cPanel uses for a
// suspension. A suspended account's processes can still be running, and
// this interface talks to a service running as root.
func TestASuspendedAccountIsNotServed(t *testing.T) {
	root := t.TempDir()
	usersDir := filepath.Join(root, "users")
	suspendedDir := filepath.Join(root, "suspended")
	for _, dir := range []string{usersDir, suspendedDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(usersDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("active", "USER=active\n")
	write("byflag", "USER=byflag\nSUSPENDED=1\n")
	write("byfile", "USER=byfile\n")
	if err := os.WriteFile(filepath.Join(suspendedDir, "byfile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]bool{
		"active": false, "byflag": true, "byfile": true,
		// No account file at all, and a name that tries to climb out of
		// the directory: neither is an account in good standing.
		"vanished": true, "../root": true,
	} {
		if got := webui.IsSuspendedForTest(usersDir, suspendedDir, name); got != want {
			t.Errorf("suspended(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestSocketConnectionsAreBoundedBeforeHTTPParsing(t *testing.T) {
	budget := webui.NewConnectionBudgetForTest(3, 2)
	if !budget.Acquire(1001) || !budget.Acquire(1001) {
		t.Fatal("the first two connections from an account were refused")
	}
	if budget.Acquire(1001) {
		t.Fatal("one account exceeded its pre-HTTP connection budget")
	}
	if !budget.Acquire(1002) {
		t.Fatal("a second account could not use the remaining global slot")
	}
	if budget.Acquire(1003) {
		t.Fatal("the global pre-HTTP connection budget was exceeded")
	}
	budget.Release(1001)
	if !budget.Acquire(1003) {
		t.Fatal("a closed connection did not release its slot")
	}
}
