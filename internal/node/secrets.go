package node

import (
	"encoding/json"
	"fmt"

	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/vault"
)

// Secret kinds, matching fleet mode's enum values.
const (
	SecretBackendCredentials = "backend_credentials"
	SecretRepositoryPassword = "repository_password"
)

// SealCredentials encodes and seals a destination's credentials, returning
// the stored secret's id. Plaintext never reaches the state file.
func SealCredentials(store *nodestore.Store, v *vault.Vault, secrets map[string]string) (string, error) {
	if len(secrets) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(secrets)
	if err != nil {
		return "", fmt.Errorf("node: encode credentials: %w", err)
	}
	sealed, err := v.Seal(encoded)
	if err != nil {
		return "", err
	}
	return store.PutSecret(SecretBackendCredentials, sealed, v.KeyID())
}

// SealRepositoryPassword seals a repository password.
func SealRepositoryPassword(store *nodestore.Store, v *vault.Vault, password string) (string, error) {
	sealed, err := v.SealString(password)
	if err != nil {
		return "", err
	}
	return store.PutSecret(SecretRepositoryPassword, sealed, v.KeyID())
}

func decodeSecrets(plaintext []byte, target *map[string]string) error {
	if err := json.Unmarshal(plaintext, target); err != nil {
		return fmt.Errorf("node: decode credentials: %w", err)
	}
	return nil
}
