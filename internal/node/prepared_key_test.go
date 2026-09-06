package node_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/nodestore"
)

// TestPreparedKeyAt covers the form coming back: the key made a moment ago
// has to be found again from the path in the form, and nothing else on the
// server may be read that way.
func TestPreparedKeyAt(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, store, root)

	prepared, err := engine.PrepareSFTPKey()
	if err != nil {
		t.Fatalf("PrepareSFTPKey: %v", err)
	}

	again, ok := engine.PreparedKeyAt(prepared.Path)
	if !ok {
		t.Fatalf("the key just made was not found again at %s", prepared.Path)
	}
	if again.Fingerprint != prepared.Fingerprint {
		t.Errorf("fingerprint = %q, want %q", again.Fingerprint, prepared.Fingerprint)
	}
	// The public half is what somebody installs on the far side. It has to
	// be the same line both times or they install a key this server will
	// not use.
	if got, want := publicHalf(again.AuthorizedKey), publicHalf(prepared.AuthorizedKey); got != want {
		t.Errorf("authorized key = %q, want %q", got, want)
	}

	keys := filepath.Join(settings.ConfigDir, "keys")
	body, err := os.ReadFile(prepared.Path)
	if err != nil {
		t.Fatal(err)
	}
	// A key that is somebody's own, in the same directory, is not a
	// prepared key: only the ones this server made unprompted are swept,
	// and only those are offered back.
	theirs := filepath.Join(keys, "id_ed25519")
	if err := os.WriteFile(theirs, body, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "master.key")

	refused := map[string]string{
		"nothing":          "",
		"a key of theirs":  theirs,
		"a file elsewhere": outside,
		"a way out":        filepath.Join(keys, "prepared-x", "..", "..", "master.key"),
		"one that is gone": filepath.Join(keys, "prepared-gone"),
	}
	for what, path := range refused {
		if _, ok := engine.PreparedKeyAt(path); ok {
			t.Errorf("%s was described as a prepared key: %s", what, path)
		}
	}
}

// publicHalf is the key itself, without the comment after it, which is a
// label and not part of what the far side matches on.
func publicHalf(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return line
	}
	return fields[0] + " " + fields[1]
}
