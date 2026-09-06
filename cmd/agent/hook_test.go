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
	"time"

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

	// The time cPanel ran the hook, not the time the spool was written.
	// The two differ by the socket timeout whenever the service is
	// stopped rather than absent, and a boundary recorded late files the
	// new owner's first backups under the customer before them.
	happenedAt := time.Now().UTC().Add(-30 * time.Second)
	path, err := spoolCPanelHook(dir, "create", payload, happenedAt)
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
	if !pending[0].Event.At.Equal(happenedAt) {
		t.Errorf("the event is dated %v, want %v -- when cPanel ran the hook",
			pending[0].Event.At, happenedAt)
	}

	// A payload that names no account is refused rather than written as
	// an event about nothing: main treats that as a failure the operator
	// is told about, because it is the difference between deferred and
	// lost.
	if _, err := spoolCPanelHook(dir, "create", []byte(`{"data":{}}`), happenedAt); err == nil {
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

// TestAServerErrorKeepsTheAccountEventForReplay covers the other half of
// "the service could not take it".
//
// A hook failure was split two ways: a service that answered had reached a
// decision worth reporting, and a service that did not answer had the
// event written down for replay. But a service that answers HTTP 500 has
// not decided anything -- its store is busy, its disk is full, it has a
// bug -- and it has not recorded the event either. Reporting that as a
// failed hook and dropping the event is how a username that changed hands
// during a bad ten minutes keeps the last owner's boundary, and with it
// their backups.
//
// Only the service's own decision, a 4xx, is final enough to drop an
// account event on.
func TestAServerErrorKeepsTheAccountEventForReplay(t *testing.T) {
	for _, status := range []int{500, 502, 503} {
		err := error(&hookServiceError{StatusCode: status, Detail: "store is busy"})
		if serviceDecided(err) {
			t.Errorf("HTTP %d was treated as the service's own decision", status)
		}
		for _, event := range []string{"create", "remove"} {
			if !recordableFailure(event, err) {
				t.Errorf("a %s answered with HTTP %d was dropped rather than "+
					"written down; the account boundary is lost", event, status)
			}
		}
	}
	// A 4xx is the service's answer: asking again would be refused again,
	// and there is nothing a replay could do with it.
	for _, status := range []int{400, 409} {
		err := error(&hookServiceError{StatusCode: status, Detail: "no account named"})
		if !serviceDecided(err) {
			t.Errorf("HTTP %d was not treated as a decision", status)
		}
		if recordableFailure("create", err) {
			t.Errorf("a create refused with HTTP %d was written down for replay", status)
		}
	}
	// A service that is not there is recorded, as it was before.
	unreachable := errors.New("dial unix: connect: no such file or directory")
	if !recordableFailure("create", unreachable) {
		t.Error("an unreachable service no longer has its events written down")
	}
	// And an event with nothing to record is still reported rather than
	// spooled, whichever way it failed.
	if recordableFailure("modify", unreachable) {
		t.Error("a modify was spooled; only create and remove carry a boundary")
	}
}

// And the whole path, through a socket that answers the way an unwell
// service does: the event ends up in the spool for the service to replay.
func TestAServerErrorSpoolsTheEventThroughTheRealPath(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "lifecycle.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "the account store is busy", http.StatusInternalServerError)
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

	happenedAt := time.Now().UTC()
	payload, hookErr := runCPanelHook(socket, "create")
	if hookErr == nil {
		t.Fatal("a service answering HTTP 500 was read as success")
	}
	if !recordableFailure("create", hookErr) {
		t.Fatalf("the create was dropped rather than written down: %v", hookErr)
	}
	spool := filepath.Join(dir, "hooks")
	if _, err := spoolCPanelHook(spool, "create", payload, happenedAt); err != nil {
		t.Fatalf("spoolCPanelHook: %v", err)
	}
	pending, problems := hookspool.Pending(spool)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if len(pending) != 1 || pending[0].Event.Account != "webshop" {
		t.Fatalf("spool holds %+v, want the create the service could not take", pending)
	}
}
