package node_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/nodestore"

	"github.com/shuki/cprest/internal/node"
	"github.com/shuki/cprest/internal/resticrun"
)

// Attaching to backups that already exist is what a replacement server
// does, so what it refuses matters as much as what it accepts: a
// repository this server cannot read is a typo, not a destination, and
// saving it would leave an operator in a disaster believing they had
// their backups back.
func TestAttachRefusesWhatItCannotRead(t *testing.T) {
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

	for _, tc := range []struct {
		what string
		req  node.AttachRequest
	}{
		{"no recovery key", node.AttachRequest{RepositoryPath: "cp01"}},
		{"no folder", node.AttachRequest{Password: "hunter2"}},
	} {
		if _, err := engine.Attach(context.Background(), tc.req); err == nil {
			t.Errorf("attaching with %s was accepted", tc.what)
		}
	}
}

func TestSummariseSeparatesAccountsFromTheServersOwnSettings(t *testing.T) {
	now := time.Now()
	contents := node.Summarise([]resticrun.Snapshot{
		{Tags: []string{"account:studio"}, Time: now.Add(-48 * time.Hour)},
		{Tags: []string{"account:studio"}, Time: now.Add(-24 * time.Hour)},
		{Tags: []string{"account:rtflow"}, Time: now.Add(-time.Hour)},
		{Tags: []string{"account:@system"}, Time: now.Add(-2 * time.Hour)},
	})

	if contents.Snapshots != 4 {
		t.Errorf("snapshots = %d, want 4", contents.Snapshots)
	}
	if len(contents.Accounts) != 2 {
		t.Fatalf("accounts = %+v, want studio and rtflow", contents.Accounts)
	}
	if contents.Accounts[0].Account != "rtflow" || contents.Accounts[1].Account != "studio" {
		t.Errorf("accounts are not in an order anyone could scan: %+v", contents.Accounts)
	}
	if contents.Accounts[1].Snapshots != 2 {
		t.Errorf("studio has %d backups, want 2", contents.Accounts[1].Snapshots)
	}
	if !contents.System {
		t.Error("the server's own settings were counted as an account")
	}
	if contents.SystemAt.IsZero() {
		t.Error("nothing says when the server's settings were last backed up")
	}
}
