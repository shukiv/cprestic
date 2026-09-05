package webui

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/node"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/vault"
)

// TestARecycledNameCannotCollectTheLastHoldersRestore: a username goes
// back into the pool and is given to somebody else. What the customer
// before them recovered is still on this server, retained to be
// collected. It is not the new customer's, and the name is the only thing
// the two have in common.
func TestARecycledNameCannotCollectTheLastHoldersRestore(t *testing.T) {
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
	v, err := vault.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := node.New(node.Config{
		Store: store, Vault: v,
		Provider:   &cpanel.Fake{Root: filepath.Join(root, "cpanel")},
		AccountUID: func(string) (int, error) { return 2002, nil },
		Log:        slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	before := time.Now().UTC().Add(-24 * time.Hour)
	if _, err := store.PutIdentity(nodestore.AccountIdentity{
		Account: "customer1", UID: 1001, SinceAt: before,
	}); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(settings.StagingRoot, "keep-restore-customer1", "cpmove-customer1.tar")
	if err := os.MkdirAll(filepath.Dir(archive), 0o700); err != nil {
		t.Fatal(err)
	}
	const theirs = "the previous customer's files"
	if err := os.WriteFile(archive, []byte(theirs), 0o600); err != nil {
		t.Fatal(err)
	}
	restore, err := store.PutRestore(nodestore.Restore{
		Account: "customer1", Status: job.StatusSuccess,
		ArchivePath: archive, QueuedAt: before, AccountSince: before,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The account is removed and the name given out again.
	if err := engine.AccountRemoved("customer1"); err != nil {
		t.Fatal(err)
	}
	if err := engine.AccountCreated("customer1"); err != nil {
		t.Fatal(err)
	}

	ui, err := New(engine, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/download?id="+restore.ID, nil)
	request = request.WithContext(context.WithValue(request.Context(), accountKey{}, "customer1"))

	collected := httptest.NewRecorder()
	ui.handleUserDownload(collected, request)
	if collected.Code == 200 || strings.Contains(collected.Body.String(), theirs) {
		t.Errorf("the new customer downloaded the last one's archive: %d", collected.Code)
	}

	home := httptest.NewRecorder()
	ui.handleUserHome(home, request)
	if strings.Contains(home.Body.String(), restore.ID) {
		t.Error("the last customer's restore is listed to the new one")
	}

	// And the same restore is still the operator's to see: it is evidence
	// on their server, not something to be hidden from them.
	if _, err := store.Restore(restore.ID); err != nil {
		t.Errorf("the record was removed rather than hidden: %v", err)
	}
}

// TestTheSameCustomerStillCollectsTheirOwn: the rule must not lock a
// customer out of what they asked for. A name that never changed hands
// filters nothing.
func TestTheSameCustomerStillCollectsTheirOwn(t *testing.T) {
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
	v, err := vault.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := node.New(node.Config{
		Store: store, Vault: v,
		Provider:   &cpanel.Fake{Root: filepath.Join(root, "cpanel")},
		AccountUID: func(string) (int, error) { return 1001, nil },
		Log:        slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.AccountCreated("customer1"); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(settings.StagingRoot, "keep-restore-customer1", "cpmove-customer1.tar")
	if err := os.MkdirAll(filepath.Dir(archive), 0o700); err != nil {
		t.Fatal(err)
	}
	const mine = "my own files"
	if err := os.WriteFile(archive, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	restore, err := store.PutRestore(nodestore.Restore{
		Account: "customer1", Status: job.StatusSuccess, ArchivePath: archive,
		QueuedAt: time.Now().UTC(), AccountSince: engine.AccountSince("customer1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	ui, err := New(engine, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/download?id="+restore.ID, nil)
	request = request.WithContext(context.WithValue(request.Context(), accountKey{}, "customer1"))

	collected := httptest.NewRecorder()
	ui.handleUserDownload(collected, request)
	if collected.Code != 200 || collected.Body.String() != mine {
		t.Fatalf("a customer could not collect their own restore: %d", collected.Code)
	}
}
