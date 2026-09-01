package node_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/shuki/cprest/internal/node"
)

// TestAnUnconfirmedHostIsToldAboutRatherThanTrusted covers what "trust on
// first use" costs when nobody is asked. The fingerprint is the only
// thing standing between a backup destination and whoever answered on
// that address; learning it silently and then sending the remote server's
// root password made that decision for the operator, invisibly, every
// time a destination was added.
//
// The engine cannot be driven here without a remote server, so what is
// asserted is the contract the interface depends on: the error is
// recognisable, and it carries what an operator needs to check.
func TestAnUnconfirmedHostIsToldAboutRatherThanTrusted(t *testing.T) {
	var err error = &node.UnconfirmedHostError{
		Host:        "backup.example.com",
		Fingerprint: "SHA256:2iAbC3dEfGhIjKlMnOpQrStUvWxYz0123456789abcd",
		KeyType:     "ssh-ed25519",
	}

	var unconfirmed *node.UnconfirmedHostError
	if !errors.As(err, &unconfirmed) {
		t.Fatal("the interface cannot tell this apart from an ordinary failure")
	}
	if unconfirmed.Fingerprint == "" {
		t.Fatal("there is nothing for the operator to check")
	}
	message := err.Error()
	for _, want := range []string{"backup.example.com", unconfirmed.Fingerprint, "ssh-keygen"} {
		if !strings.Contains(message, want) {
			t.Errorf("the message does not mention %q: %s", want, message)
		}
	}
}
