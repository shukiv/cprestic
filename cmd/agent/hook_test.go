package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
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
