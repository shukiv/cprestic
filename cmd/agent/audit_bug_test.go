package main

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shuki/cprest/internal/hookspool"
)

// TestAServiceFailureDoesNotLoseTheAccountBoundary runs the hook binary
// itself against a service that answers the way an unwell one does.
//
// The hook split failures two ways: a service that answered had reached a
// decision worth reporting to WHM, and a service that could not be reached
// had the event written down for replay. A 500 fell on the reporting side,
// so a create or a remove the service could not record was reported as a
// failed hook and then dropped. A username that changed hands during those
// minutes keeps the previous owner's boundary, and with it their backups.
//
// A 500 is not a decision. The event is written down, exactly as it is for
// a service that is not running.
func TestAServiceFailureDoesNotLoseTheAccountBoundary(t *testing.T) {
	root, err := os.MkdirTemp("/dev/shm", "cpr-hook-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket := filepath.Join(root, "lifecycle.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "state database write failed", http.StatusInternalServerError)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	binary := filepath.Join(root, "cprest-agent")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build hook binary: %v\n%s", err, output)
	}
	spool := filepath.Join(root, "spool")
	cmd := exec.Command(binary,
		"-cpanel-hook=create", "-lifecycle-socket="+socket, "-hook-spool="+spool)
	cmd.Stdin = strings.NewReader(`{"data":{"user":"customer1"}}`)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the hook failed rather than recording the event: %v\n%s", err, output)
	}
	// cPanel reads the first character: 1 is a hook that succeeded.
	if !strings.HasPrefix(string(output), "1 ") {
		t.Errorf("the hook reported failure to WHM: %s", output)
	}
	pending, problems := hookspool.Pending(spool)
	if len(problems) != 0 {
		t.Fatalf("read spool: %v", problems)
	}
	if len(pending) != 1 {
		t.Fatalf("the account boundary was lost: the spool holds %+v", pending)
	}
	if pending[0].Event.Account != "customer1" || pending[0].Event.Event != "create" {
		t.Errorf("spooled %+v", pending[0].Event)
	}
}
