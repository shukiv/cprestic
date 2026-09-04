package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/granular"
	"github.com/shuki/cprest/internal/protocol"
)

// restoredTree is what restoreItems leaves behind before anything is done
// with it: the parts of the account, under names that say where they belong.
func restoredTree(t *testing.T) string {
	t.Helper()
	out := t.TempDir()
	if err := os.MkdirAll(filepath.Join(out, "homedir", "public_html"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "homedir", "public_html", "index.php"),
		[]byte("<?php\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(out, "databases"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"c1_shop.sql", "c1_wp.sql", granular.DatabaseUsersFile} {
		if err := os.WriteFile(filepath.Join(out, "databases", name),
			[]byte("-- dump\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func quietAgent(provider cpanel.Provider) *Agent {
	return &Agent{
		provider: provider,
		log:      slog.New(slog.DiscardHandler),
	}
}

func TestApplyingADatabaseLoadsOnlyWhatWasAskedFor(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_shop", "c1_wp"}}}
	agent := quietAgent(fake)

	written, _, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindDatabase),
			ItemNames:  []string{"c1_shop"},
		}, out)
	if err != nil {
		t.Fatalf("applyItems: %v", err)
	}
	if len(fake.LoadedDatabases) != 1 {
		t.Fatalf("loaded %d databases, want 1: %+v", len(fake.LoadedDatabases), fake.LoadedDatabases)
	}
	loaded := fake.LoadedDatabases[0]
	if loaded.User != "c1" || loaded.Database != "c1_shop" {
		t.Errorf("loaded %+v", loaded)
	}
	if filepath.Base(loaded.DumpPath) != "c1_shop.sql" {
		t.Errorf("loaded the dump %s", loaded.DumpPath)
	}
	if len(fake.PutBackHome) != 0 {
		t.Errorf("a database restore also wrote files: %+v", fake.PutBackHome)
	}
	if written == "" {
		t.Error("nothing was reported as written")
	}
}

// A database the account does not own is refused by the provider. The name
// comes out of a backup, and a name can have changed hands since it was
// taken.
func TestApplyingADatabaseTheAccountDoesNotOwnIsRefused(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_shop"}}}
	agent := quietAgent(fake)

	_, _, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindDatabase),
			ItemNames:  []string{"c2_shop"},
		}, out)
	if err == nil {
		t.Fatal("a database belonging to another account was loaded")
	}
	if len(fake.LoadedDatabases) != 0 {
		t.Errorf("loaded %+v", fake.LoadedDatabases)
	}
}

// The case this feature exists for: somebody dropped their database this
// morning and wants it back. cPanel no longer lists it, so the load cannot
// go ahead -- and the answer they need is "make it again first", not the
// generic failure a customer is otherwise shown.
func TestARestoreIntoADroppedDatabaseSaysWhatToDoAboutIt(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_wp"}}}
	agent := quietAgent(fake)

	_, hint, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindDatabase),
			ItemNames:  []string{"c1_shop"},
		}, out)
	if err == nil {
		t.Fatal("a database that is not on the account was loaded")
	}
	if !strings.Contains(hint, "c1_shop") || !strings.Contains(hint, "Create it again") {
		t.Fatalf("hint = %q", hint)
	}
	// What a customer is shown must not name a path or a repository.
	if strings.Contains(hint, "/") {
		t.Errorf("the hint carries a path: %q", hint)
	}
}

func TestApplyingFilesWritesTheHomeDirectory(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{}
	agent := quietAgent(fake)

	for _, kind := range []granular.Kind{
		granular.KindFiles, granular.KindWebsite, granular.KindMailbox,
	} {
		fake.PutBackHome = nil
		if _, _, err := agent.applyItems(context.Background(), agent.log,
			protocol.RestoreAssignment{
				CPanelUser: "c1", ItemKind: string(kind),
				ItemNames: []string{"public_html"},
			}, out); err != nil {
			t.Fatalf("applyItems(%s): %v", kind, err)
		}
		if len(fake.PutBackHome) != 1 || fake.PutBackHome[0].User != "c1" {
			t.Fatalf("%s wrote %+v", kind, fake.PutBackHome)
		}
		if filepath.Base(fake.PutBackHome[0].From) != "homedir" {
			t.Errorf("%s wrote from %s", kind, fake.PutBackHome[0].From)
		}
	}
}

// The node refuses these before a job exists. The agent refuses them again,
// because this is the code that runs as root against a live account.
func TestApplyingWhatCannotBeAppliedIsRefusedByTheAgentToo(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_shop"}}}
	agent := quietAgent(fake)

	for _, kind := range []granular.Kind{
		granular.KindDNS, granular.KindSSL, granular.KindDBUsers,
		granular.KindFTP, granular.KindCron, granular.KindDomains,
		granular.KindSettings, granular.KindSystem,
	} {
		if _, _, err := agent.applyItems(context.Background(), agent.log,
			protocol.RestoreAssignment{
				CPanelUser: "c1", ItemKind: string(kind),
			}, out); err == nil {
			t.Errorf("a %s restore was written into the live account", kind)
		}
	}
	if len(fake.LoadedDatabases) != 0 || len(fake.PutBackHome) != 0 {
		t.Errorf("something was written: %+v %+v", fake.LoadedDatabases, fake.PutBackHome)
	}
}
