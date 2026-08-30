package vault

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestVault(t *testing.T) *Vault {
	t.Helper()
	keyHex, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	v, err := New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

func TestSealOpenRoundTrip(t *testing.T) {
	v := newTestVault(t)
	secret := "AKIAIOSFODNN7EXAMPLE/wJalrXUtnFEMI"

	blob, err := v.SealString(secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(blob, []byte(secret)) {
		t.Fatal("ciphertext contains the plaintext")
	}

	got, err := v.OpenString(blob)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != secret {
		t.Errorf("Open = %q, want %q", got, secret)
	}
}

func TestSealIsNonDeterministic(t *testing.T) {
	v := newTestVault(t)
	first, err := v.SealString("same secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	second, err := v.SealString("same secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Each secret gets its own data key and nonce, so identical
	// plaintexts must not produce identical blobs.
	if bytes.Equal(first, second) {
		t.Error("two seals of the same plaintext produced identical ciphertext")
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	v := newTestVault(t)
	blob, err := v.SealString("repository password")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	positions := map[string]int{
		"version":       0,
		"data key":      5,
		"wrapped key":   20,
		"payload nonce": 1 + 12 + 48 + 2,
		"payload":       len(blob) - 1,
	}
	for name, index := range positions {
		tampered := bytes.Clone(blob)
		tampered[index] ^= 0xff
		if _, err := v.Open(tampered); err == nil {
			t.Errorf("flipping the %s byte was accepted", name)
		}
	}

	if _, err := v.Open([]byte("short")); !errors.Is(err, ErrCorruptCiphertext) {
		t.Errorf("truncated blob gave %v, want ErrCorruptCiphertext", err)
	}
}

func TestOpenRejectsOtherMasterKey(t *testing.T) {
	first := newTestVault(t)
	second := newTestVault(t)

	blob, err := first.SealString("secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := second.Open(blob); !errors.Is(err, ErrCorruptCiphertext) {
		t.Errorf("err = %v, want ErrCorruptCiphertext", err)
	}
	if first.KeyID() == second.KeyID() {
		t.Error("different master keys produced the same key id")
	}
}

func TestRewrap(t *testing.T) {
	oldVault := newTestVault(t)
	newVault := newTestVault(t)

	blob, err := oldVault.SealString("rotate me")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	rewrapped, err := oldVault.Rewrap(blob, newVault)
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	got, err := newVault.OpenString(rewrapped)
	if err != nil {
		t.Fatalf("Open after rewrap: %v", err)
	}
	if got != "rotate me" {
		t.Errorf("got %q", got)
	}
	if _, err := oldVault.Open(rewrapped); err == nil {
		t.Error("the old key should no longer open a rewrapped blob")
	}
}

func TestNewRejectsWrongKeyLength(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33} {
		if _, err := New(make([]byte, size)); err == nil {
			t.Errorf("a %d-byte master key was accepted", size)
		}
	}
}

func TestLoadMasterKey(t *testing.T) {
	dir := t.TempDir()
	keyHex, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}

	good := filepath.Join(dir, "master.key")
	if err := os.WriteFile(good, []byte(keyHex+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := LoadMasterKey(good)
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	if len(key) != masterKeyLen {
		t.Errorf("key length = %d", len(key))
	}

	// This file decrypts every credential in the system.
	loose := filepath.Join(dir, "loose.key")
	if err := os.WriteFile(loose, []byte(keyHex), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(loose); err == nil ||
		!strings.Contains(err.Error(), "world-accessible") {
		t.Errorf("err = %v, want a permissions complaint", err)
	}

	short := filepath.Join(dir, "short.key")
	if err := os.WriteFile(short, []byte("abcd"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(short); err == nil {
		t.Error("a short key should be rejected")
	}

	notHex := filepath.Join(dir, "nothex.key")
	if err := os.WriteFile(notHex, []byte(strings.Repeat("z", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(notHex); err == nil {
		t.Error("a non-hex key should be rejected")
	}

	if _, err := LoadMasterKey(filepath.Join(dir, "missing.key")); err == nil {
		t.Error("a missing key file should be rejected")
	}
}
