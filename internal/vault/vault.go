// Package vault encrypts the credentials the controller stores.
//
// Two classes of secret live in the database: backend credentials (S3 keys,
// SSH keys, rest-server passwords) and restic repository passwords. Neither
// is ever written in plaintext. See docs/DESIGN.md §11.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	// blobVersion prefixes every ciphertext so the format can change
	// without guessing at old data.
	blobVersion byte = 1

	masterKeyLen = 32 // AES-256
	dataKeyLen   = 32
	nonceLen     = 12 // GCM standard nonce
	tagLen       = 16
)

// ErrCorruptCiphertext means the stored blob is malformed or was tampered
// with. GCM authenticates the ciphertext, so a modified blob fails here
// rather than decrypting to garbage.
var ErrCorruptCiphertext = errors.New("vault: ciphertext is corrupt or was tampered with")

// Vault seals and opens secrets with a master key.
//
// Encryption is enveloped: every secret gets its own random data key, and
// only that data key is encrypted with the master key. Rotating the master
// key therefore rewraps data keys instead of re-encrypting every secret,
// and a single secret's key never protects more than one value.
type Vault struct {
	master cipher.AEAD
	keyID  string
}

// New builds a Vault from a raw 32-byte master key.
func New(masterKey []byte) (*Vault, error) {
	if len(masterKey) != masterKeyLen {
		return nil, fmt.Errorf("vault: master key must be %d bytes, got %d",
			masterKeyLen, len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("vault: master cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vault: master gcm: %w", err)
	}

	// The key id identifies which master key sealed a blob. It is a hash,
	// not the key: publishing it in the database reveals nothing.
	digest := sha256.Sum256(masterKey)
	return &Vault{master: aead, keyID: hex.EncodeToString(digest[:8])}, nil
}

// LoadMasterKey reads a master key file containing 64 hex characters.
//
// The file must not be readable by group or others: it decrypts every
// credential in the system.
func LoadMasterKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("vault: master key file: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf(
			"vault: master key file %s is group- or world-accessible (mode %04o)", path, perm)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("vault: read master key: %w", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("vault: master key must be hex: %w", err)
	}
	if len(key) != masterKeyLen {
		return nil, fmt.Errorf("vault: master key must decode to %d bytes, got %d",
			masterKeyLen, len(key))
	}
	return key, nil
}

// GenerateMasterKey returns a new random master key as hex, for writing to
// a key file at first setup.
func GenerateMasterKey() (string, error) {
	key := make([]byte, masterKeyLen)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("vault: generate master key: %w", err)
	}
	return hex.EncodeToString(key), nil
}

// KeyID identifies the master key this Vault holds. It is stored alongside
// each ciphertext so a rotation can find blobs still sealed with the old key.
func (v *Vault) KeyID() string { return v.keyID }

// Seal encrypts a secret.
//
// Layout: version | dek nonce | wrapped dek | payload nonce | payload.
func (v *Vault) Seal(plaintext []byte) ([]byte, error) {
	dataKey := make([]byte, dataKeyLen)
	if _, err := rand.Read(dataKey); err != nil {
		return nil, fmt.Errorf("vault: generate data key: %w", err)
	}
	defer zero(dataKey)

	payloadAEAD, err := newAEAD(dataKey)
	if err != nil {
		return nil, err
	}

	dekNonce := make([]byte, nonceLen)
	if _, err := rand.Read(dekNonce); err != nil {
		return nil, fmt.Errorf("vault: generate nonce: %w", err)
	}
	payloadNonce := make([]byte, nonceLen)
	if _, err := rand.Read(payloadNonce); err != nil {
		return nil, fmt.Errorf("vault: generate nonce: %w", err)
	}

	wrappedKey := v.master.Seal(nil, dekNonce, dataKey, nil)
	payload := payloadAEAD.Seal(nil, payloadNonce, plaintext, nil)

	blob := make([]byte, 0, 1+len(dekNonce)+len(wrappedKey)+len(payloadNonce)+len(payload))
	blob = append(blob, blobVersion)
	blob = append(blob, dekNonce...)
	blob = append(blob, wrappedKey...)
	blob = append(blob, payloadNonce...)
	blob = append(blob, payload...)
	return blob, nil
}

// SealString is Seal for text secrets.
func (v *Vault) SealString(plaintext string) ([]byte, error) {
	return v.Seal([]byte(plaintext))
}

// Open decrypts a blob produced by Seal.
func (v *Vault) Open(blob []byte) ([]byte, error) {
	wrappedLen := dataKeyLen + tagLen
	minLen := 1 + nonceLen + wrappedLen + nonceLen + tagLen
	if len(blob) < minLen {
		return nil, ErrCorruptCiphertext
	}
	if blob[0] != blobVersion {
		return nil, fmt.Errorf("vault: unsupported ciphertext version %d", blob[0])
	}

	offset := 1
	dekNonce := blob[offset : offset+nonceLen]
	offset += nonceLen
	wrappedKey := blob[offset : offset+wrappedLen]
	offset += wrappedLen
	payloadNonce := blob[offset : offset+nonceLen]
	offset += nonceLen
	payload := blob[offset:]

	dataKey, err := v.master.Open(nil, dekNonce, wrappedKey, nil)
	if err != nil {
		return nil, ErrCorruptCiphertext
	}
	defer zero(dataKey)

	payloadAEAD, err := newAEAD(dataKey)
	if err != nil {
		return nil, err
	}
	plaintext, err := payloadAEAD.Open(nil, payloadNonce, payload, nil)
	if err != nil {
		return nil, ErrCorruptCiphertext
	}
	return plaintext, nil
}

// OpenString is Open for text secrets.
func (v *Vault) OpenString(blob []byte) (string, error) {
	plaintext, err := v.Open(blob)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// Rewrap re-encrypts a blob under a new Vault's master key without the
// caller ever handling the plaintext beyond this call.
func (v *Vault) Rewrap(blob []byte, next *Vault) ([]byte, error) {
	plaintext, err := v.Open(blob)
	if err != nil {
		return nil, err
	}
	defer zero(plaintext)
	return next.Seal(plaintext)
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("vault: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vault: gcm: %w", err)
	}
	return aead, nil
}

// zero overwrites key material. Go's garbage collector may still have moved
// a copy, so this is a reduction in exposure rather than a guarantee.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
	// Defeat any dead-store elimination by reading the result.
	_ = subtle.ConstantTimeCompare(b, b)
}
