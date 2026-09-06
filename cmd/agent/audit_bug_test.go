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

// Positive audit reproduction: a passing test means a create that the
// service answered with 500 was lost rather than left for replay.
func TestAuditServiceFailureDropsAccountBoundary(t *testing.T) {
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
	if err := cmd.Run(); err == nil {
		t.Fatal("service HTTP 500 unexpectedly reported hook success")
	}
	pending, problems := hookspool.Pending(spool)
	if len(problems) != 0 {
		t.Fatalf("read spool: %v", problems)
	}
	if len(pending) != 0 {
		t.Fatalf("boundary was unexpectedly preserved: %+v", pending)
	}
}
