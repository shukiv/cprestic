package hookspool_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/hookspool"
)

// The order matters more than anything else here: a remove and a create
// of the same username, replayed the other way round, would retire the
// account that has just been made.
func TestSpooledEventsComeBackInTheOrderTheyHappened(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hooks")

	created := time.Now().UTC()
	removed := created.Add(-time.Hour)
	for _, event := range []hookspool.Event{
		{At: created, Event: "create", Account: "webshop"},
		{At: removed, Event: "remove", Account: "webshop"},
	} {
		if _, err := hookspool.Write(dir, event); err != nil {
			t.Fatalf("Write %s: %v", event.Event, err)
		}
	}

	pending, problems := hookspool.Pending(dir)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %d, want 2", len(pending))
	}
	if pending[0].Event.Event != "remove" || pending[1].Event.Event != "create" {
		t.Fatalf("replayed %s then %s, want the remove first",
			pending[0].Event.Event, pending[1].Event.Event)
	}
	if !pending[1].Event.At.Equal(created) {
		t.Errorf("the create is dated %v, want %v", pending[1].Event.At, created)
	}

	// Done clears one event and leaves the other: replay records one at a
	// time and must not drop what it has not managed yet.
	if err := hookspool.Done(pending[0]); err != nil {
		t.Fatal(err)
	}
	left, _ := hookspool.Pending(dir)
	if len(left) != 1 || left[0].Event.Event != "create" {
		t.Fatalf("after clearing the remove, the spool holds %+v", left)
	}
	// And clearing something twice is not an error: a crash between the
	// record and the delete replays the event, which has to be harmless.
	if err := hookspool.Done(pending[0]); err != nil {
		t.Errorf("clearing an already-cleared event: %v", err)
	}
}

func TestOnlyOwnershipEventsAreSpooled(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hooks")
	for _, event := range []string{"modify", "suspend", "unsuspend", "remove-pre", ""} {
		if hookspool.Spooled(event) {
			t.Errorf("%q is kept for replay; only create and remove should be", event)
		}
		if _, err := hookspool.Write(dir, hookspool.Event{
			Event: event, Account: "webshop"}); err == nil {
			t.Errorf("%q was written to the spool", event)
		}
	}
	for _, event := range []string{"create", "remove"} {
		if !hookspool.Spooled(event) {
			t.Errorf("%q is not kept, so an ownership boundary would be lost", event)
		}
	}
}

// What is written has to survive the process that wrote it, and a file
// nobody else may read: the payload is cPanel's account data.
func TestASpooledEventIsOnTheDiskAndPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hooks")
	path, err := hookspool.Write(dir, hookspool.Event{
		Event: "create", Payload: []byte(`{"data":{"user":"webshop"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %v, want 0700", dirInfo.Mode().Perm())
	}

	// The account is read out of the payload when the caller does not
	// name one, so a replay decides it exactly as the live hook would.
	pending, problems := hookspool.Pending(dir)
	if len(problems) != 0 || len(pending) != 1 {
		t.Fatalf("pending = %+v, problems = %v", pending, problems)
	}
	if pending[0].Event.Account != "webshop" {
		t.Errorf("account = %q, want webshop", pending[0].Event.Account)
	}
}

// A file that cannot be read stays where it is. It is still evidence that
// something happened to an account, and deleting it would throw away the
// only record of an ownership boundary.
func TestAnUnreadableEventIsKeptAndReported(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "00000000000000000001-create-webshop.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	pending, problems := hookspool.Pending(dir)
	if len(pending) != 0 {
		t.Errorf("an unreadable file was replayed as %+v", pending)
	}
	if len(problems) != 1 || !strings.Contains(problems[0].Error(), broken) {
		t.Errorf("problems = %v, want the file named", problems)
	}
	if _, err := os.Stat(broken); err != nil {
		t.Errorf("the unreadable event was deleted: %v", err)
	}
}

// An empty or missing spool is the ordinary case, not a failure.
func TestAnEmptySpoolIsNotAProblem(t *testing.T) {
	pending, problems := hookspool.Pending(filepath.Join(t.TempDir(), "never-written"))
	if len(pending) != 0 || len(problems) != 0 {
		t.Errorf("pending = %+v, problems = %v", pending, problems)
	}
}
