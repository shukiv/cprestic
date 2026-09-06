package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shuki/cprest/internal/hookspool"
)

func TestCPanelHookDescriptionIncludesBlockingPreRemove(t *testing.T) {
	var output bytes.Buffer
	if err := writeCPanelHookDescription(&output); err != nil {
		t.Fatal(err)
	}
	var descriptors []cpanelHookDescriptor
	if err := json.Unmarshal(output.Bytes(), &descriptors); err != nil {
		t.Fatal(err)
	}
	if len(descriptors) != 6 {
		t.Fatalf("descriptors = %d, want 6", len(descriptors))
	}
	var found, suspend, unsuspend bool
	for _, descriptor := range descriptors {
		if descriptor.Event == "Accounts::Remove" && descriptor.Stage == "pre" {
			found = descriptor.Blocking == 1 && strings.Contains(descriptor.Hook, "--cpanel-hook=remove-pre")
		}
		if descriptor.Event == "Accounts::suspendacct" && descriptor.Stage == "post" {
			suspend = strings.Contains(descriptor.Hook, "--cpanel-hook=suspend")
		}
		if descriptor.Event == "Accounts::unsuspendacct" && descriptor.Stage == "post" {
			unsuspend = strings.Contains(descriptor.Hook, "--cpanel-hook=unsuspend")
		}
		if descriptor.ExecType != "script" || !strings.HasPrefix(descriptor.Hook, "/") {
			t.Fatalf("invalid script descriptor: %+v", descriptor)
		}
	}
	if !found {
		t.Fatal("blocking Accounts::Remove pre-hook is missing")
	}
	if !suspend || !unsuspend {
		t.Fatalf("suspension descriptors = suspend:%v unsuspend:%v", suspend, unsuspend)
	}
}

func TestBlockingHookFailureOnlyBlocksDefiniteClientRefusal(t *testing.T) {
	if detail, block := blockingHookFailure(&hookServiceError{StatusCode: 409, Detail: "no backup"}); !block || detail != "no backup" {
		t.Fatalf("409 = %q, %v", detail, block)
	}
	if _, block := blockingHookFailure(&hookServiceError{StatusCode: 500, Detail: "database unavailable"}); block {
		t.Fatal("service failure would wedge cPanel account removal")
	}
	if _, block := blockingHookFailure(nil); block {
		t.Fatal("network failure would block account removal")
	}
}

func TestHookMessageIsSingleLineAndCannotInjectBailout(t *testing.T) {
	message := hookMessage("first\nBAILOUT second")
	if strings.Contains(message, "\n") || strings.Contains(message, "BAILOUT") {
		t.Fatalf("unsafe hook message %q", message)
	}
}

// TestAnUnreachableServiceDoesNotFailAPostHook covers a failure mode that
// reaches far outside this program. cPanel's post hooks run on every
// account create, modify, suspend and remove. Reporting a hook failure
// because the backup service happens to be restarting makes ordinary
// account administration look broken across the whole server -- the
// blocking pre-remove hook already reasons this way, and the others did
// not.
//
// A service that answered is different: it reached a decision, and that
// is worth reporting.
func TestAnUnreachableServiceDoesNotFailAPostHook(t *testing.T) {
	if serviceAnswered(errors.New("dial unix /var/run/cprest/hook.sock: connect: no such file")) {
		t.Error("a service that could not be reached was treated as having answered")
	}
	for _, status := range []int{400, 409, 500, 503} {
		err := error(&hookServiceError{StatusCode: status, Detail: "detail"})
		if !serviceAnswered(err) {
			t.Errorf("HTTP %d came from a reachable service and was not treated as one", status)
		}
	}
	// And wrapping must not lose it: the transport wraps the cause.
	wrapped := fmt.Errorf("notify cprest service: %w", &hookServiceError{StatusCode: 409})
	if !serviceAnswered(wrapped) {
		t.Error("a wrapped service error was not recognised")
	}
	if _, denied := blockingHookFailure(wrapped); !denied {
		t.Error("a wrapped definite refusal was not recognised as one")
	}
}

// An account event the service was not running to hear is written down
// rather than lost.
//
// The hook deliberately fails open when it cannot reach the service:
// wedging every account create and remove on the server because a backup
// program is restarting would be worse than the thing it protects
// against. But a create or a remove that nobody heard cannot be worked
// out again by looking at the account list, so it is kept for replay.
func TestAnUndeliverableAccountEventIsKeptForReplay(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hooks")
	payload := []byte(`{"context":"Accounts::Create","data":{"user":"webshop"}}`)

	path, err := spoolCPanelHook(dir, "create", payload)
	if err != nil {
		t.Fatalf("spoolCPanelHook: %v", err)
	}
	pending, problems := hookspool.Pending(dir)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if len(pending) != 1 || pending[0].Path != path {
		t.Fatalf("spool holds %+v, want the create that was just written", pending)
	}
	if pending[0].Event.Account != "webshop" || pending[0].Event.Event != "create" {
		t.Errorf("spooled %+v", pending[0].Event)
	}

	// A payload that names no account is refused rather than written as
	// an event about nothing: main treats that as a failure the operator
	// is told about, because it is the difference between deferred and
	// lost.
	if _, err := spoolCPanelHook(dir, "create", []byte(`{"data":{}}`)); err == nil {
		t.Error("an event naming no account was accepted")
	}
}

// When the service answers, nothing is spooled: the event has been
// recorded where it belongs, and a copy left behind would be replayed on
// the next restart.
func TestADeliveredAccountEventIsNotSpooled(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "lifecycle.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	stdin, err := os.CreateTemp(dir, "hook-input")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.WriteString(`{"data":{"user":"webshop"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	realStdin := os.Stdin
	os.Stdin = stdin
	defer func() { os.Stdin = realStdin }()

	if _, err := runCPanelHook(socket, "create"); err != nil {
		t.Fatalf("runCPanelHook: %v", err)
	}
	spool := filepath.Join(dir, "hooks")
	pending, problems := hookspool.Pending(spool)
	if len(pending) != 0 || len(problems) != 0 {
		t.Errorf("a delivered event was also spooled: %+v %v", pending, problems)
	}
}
