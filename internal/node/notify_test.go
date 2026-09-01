package node_test

import (
	"path/filepath"
	"testing"

	"github.com/shuki/cprest/internal/nodestore"
)

// TestSavingAChannelKeepsItsStoredSecret covers the way notifications go
// quiet without anyone noticing: an operator opens a working Telegram
// channel to change its name, the token field is blank because a sealed
// secret is never rendered back into a page, and saving wipes the token.
// From then on the server has a channel that looks configured and sends
// nothing.
func TestSavingAChannelKeepsItsStoredSecret(t *testing.T) {
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

	saved, err := engine.SaveChannel(nodestore.Channel{
		Name:    "On call",
		Kind:    "telegram",
		Config:  map[string]string{"chat_id": "12345"},
		Enabled: true,
	}, map[string]string{"token": "the-bot-token"})
	if err != nil {
		t.Fatalf("save a new channel: %v", err)
	}
	if saved.SecretsID == "" {
		t.Fatal("a channel with a token was stored without sealed credentials")
	}

	// The edit: same channel, new name, no token typed.
	renamed := saved
	renamed.Name = "On call (night)"
	again, err := engine.SaveChannel(renamed, map[string]string{"token": ""})
	if err != nil {
		t.Fatalf("rename a channel without retyping its token: %v", err)
	}
	if again.SecretsID != saved.SecretsID {
		t.Fatalf("the sealed token was replaced on a blank edit: %q became %q",
			saved.SecretsID, again.SecretsID)
	}

	// And it must be a channel that can still send: SaveChannel refuses
	// one whose required settings are missing, so a wiped token would
	// have failed above — but check the stored form too, because that is
	// what Notify will read.
	stored, err := store.Channel(again.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SecretsID != saved.SecretsID {
		t.Fatal("the stored channel lost its credentials")
	}
	if stored.Name != "On call (night)" {
		t.Fatalf("the rename did not stick: %q", stored.Name)
	}
}
