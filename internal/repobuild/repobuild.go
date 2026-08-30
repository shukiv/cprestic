// Package repobuild turns stored repository rows and sealed credentials
// into something restic can be pointed at.
//
// Both the controller (dispatching a job) and the maintenance runner
// (pruning and checking) need this, and neither should be the one to own
// it: keeping the unsealing in one place means there is a single audited
// path from ciphertext to a live credential.
package repobuild

import (
	"encoding/json"
	"fmt"

	"github.com/shuki/cprest/internal/destination"
	"github.com/shuki/cprest/internal/vault"
)

// Sealed is a repository and its destination as they are stored: config in
// the clear, credentials encrypted.
type Sealed struct {
	DestinationType    string
	DestinationConfig  []byte
	CredentialsSealed  []byte
	RepoPath           string
	RepoPasswordSealed []byte
}

// Opened is the same repository with its secrets decrypted, ready to hand
// to internal/resticrun.
type Opened struct {
	Spec     destination.Spec
	Path     string
	Password string
}

// Open decrypts the credentials and validates that they produce a usable
// destination, addressed the way an agent reaches it.
//
// Building the destination here means a misconfiguration fails at dispatch,
// with a message an operator can read, rather than on a remote host in the
// middle of the backup window.
func Open(v *vault.Vault, sealed Sealed) (Opened, error) {
	return open(v, sealed, false)
}

// OpenForMaintenance is Open, addressed to the endpoint that permits
// deletes where the destination declares one. An append-only rest-server
// rejects deletes from every caller, so retention needs the second
// endpoint rather than a different credential. See docs/DESIGN.md §8.
func OpenForMaintenance(v *vault.Vault, sealed Sealed) (Opened, error) {
	return open(v, sealed, true)
}

func open(v *vault.Vault, sealed Sealed, forMaintenance bool) (Opened, error) {
	secrets := map[string]string{}
	if len(sealed.CredentialsSealed) > 0 {
		plaintext, err := v.Open(sealed.CredentialsSealed)
		if err != nil {
			return Opened{}, fmt.Errorf("repobuild: open destination credentials: %w", err)
		}
		if err := json.Unmarshal(plaintext, &secrets); err != nil {
			return Opened{}, fmt.Errorf("repobuild: decode destination credentials: %w", err)
		}
	}

	password, err := v.OpenString(sealed.RepoPasswordSealed)
	if err != nil {
		return Opened{}, fmt.Errorf("repobuild: open repository password: %w", err)
	}
	if password == "" {
		return Opened{}, fmt.Errorf("repobuild: repository password is empty")
	}

	spec, err := destination.ParseSpec(
		destination.Type(sealed.DestinationType), sealed.DestinationConfig, secrets)
	if err != nil {
		return Opened{}, err
	}
	if forMaintenance {
		spec = destination.ForMaintenance(spec)
	}
	if _, err := destination.Build(spec); err != nil {
		return Opened{}, err
	}

	return Opened{Spec: spec, Path: sealed.RepoPath, Password: password}, nil
}

// SealCredentials encodes a credential map and seals it, for the CLI that
// registers a destination.
func SealCredentials(v *vault.Vault, secrets map[string]string) ([]byte, error) {
	encoded, err := json.Marshal(secrets)
	if err != nil {
		return nil, fmt.Errorf("repobuild: encode credentials: %w", err)
	}
	return v.Seal(encoded)
}
