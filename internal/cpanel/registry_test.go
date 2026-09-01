package cpanel

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestDatabasesComeFromCPanelsOwnRecord covers the two ways the naming
// convention is wrong, both of which put the wrong data in a backup.
//
// A server with database prefixing turned off has databases whose names
// say nothing about who owns them: guessing by prefix backs up none of
// them, and the account is restored with its tables missing. And one
// account's name can be a prefix of another's, so "rozin" guessing by
// prefix claims "rozingroup_data" — another customer's data, and the
// grants that read it, inside this customer's backup.
func TestDatabasesComeFromCPanelsOwnRecord(t *testing.T) {
	host := newFakeHost(t, "rozin")
	databasesDir := filepath.Join(t.TempDir(), "databases")
	if err := os.MkdirAll(databasesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	host.DatabasesDir = databasesDir
	// A mysql that must never be reached: the record answers the question.
	host.MysqlPath = "/bin/false"

	record := `{"MYSQL":{"owner":"rozin","dbs":{"rozin_shop":"127.0.0.1","legacy_crm":"127.0.0.1"},` +
		`"dbusers":{"rozin_shop":{}},"noprefix":{}},"version":1}`
	if err := os.WriteFile(filepath.Join(databasesDir, "rozin.json"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}

	databases, err := host.databases(context.Background(), "rozin")
	if err != nil {
		t.Fatalf("databases: %v", err)
	}
	found := map[string]bool{}
	for _, database := range databases {
		found[database] = true
	}
	if !found["legacy_crm"] {
		t.Error("a database that does not carry the account's prefix was left out of the backup")
	}
	if !found["rozin_shop"] {
		t.Error("a database that does carry the prefix was left out")
	}
	if len(databases) != 2 {
		t.Errorf("databases = %v, want exactly the two cPanel recorded", databases)
	}
}

// TestADatabaseMapForSomebodyElseIsNotTrusted covers the file being for a
// different account than the one asking — a rename, a stale copy, a
// mistake. Reading it anyway would hand one customer another's databases.
func TestADatabaseMapForSomebodyElseIsNotTrusted(t *testing.T) {
	host := newFakeHost(t, "alice")
	databasesDir := filepath.Join(t.TempDir(), "databases")
	if err := os.MkdirAll(databasesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	host.DatabasesDir = databasesDir
	record := `{"MYSQL":{"owner":"bob","dbs":{"bob_secrets":"127.0.0.1"}},"version":1}`
	if err := os.WriteFile(filepath.Join(databasesDir, "alice.json"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, recorded := host.recordedDatabases("alice"); recorded {
		t.Fatal("a database map belonging to another account was accepted")
	}
}

// TestThePrefixFallbackDefersToTheOwnerMap covers the server with no
// per-account record: the convention is all there is, and it over-claims.
// /etc/dbowners is the server-wide answer to the same question.
func TestThePrefixFallbackDefersToTheOwnerMap(t *testing.T) {
	host := newFakeHost(t, "rozin")
	owners := filepath.Join(t.TempDir(), "dbowners")
	if err := os.WriteFile(owners, []byte(
		"#dbowners v1\nrozin_shop: rozin\nrozin_group_data: rozingroup\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	host.DBOwnersPath = owners

	kept := host.ownedDatabases("rozin", []string{"rozin_shop", "rozin_group_data"})
	if len(kept) != 1 || kept[0] != "rozin_shop" {
		t.Fatalf("kept = %v, want only rozin_shop: the other belongs to rozingroup", kept)
	}
}

// TestASuspendedAccountIsRecognised covers both marks cPanel uses. The
// account's files are still backed up; what it must not do is drive this
// service.
func TestASuspendedAccountIsRecognised(t *testing.T) {
	host := newFakeHost(t, "byfile", "byflag", "active")
	suspendedDir := filepath.Join(t.TempDir(), "suspended")
	if err := os.MkdirAll(suspendedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	host.SuspendedDir = suspendedDir
	if err := os.WriteFile(filepath.Join(suspendedDir, "byfile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host.usersDir(), "byflag"),
		[]byte("USER=byflag\nSUSPENDED=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]bool{"byfile": true, "byflag": true, "active": false} {
		if got := host.suspended(name); got != want {
			t.Errorf("suspended(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestAnUnhelpfulDatabaseMapFallsBackRatherThanBackingUpNothing covers a
// file that parses and says nothing: no MYSQL section, or one with no
// owner. Reading that as "this account has no databases" would drop them
// out of the backup with nothing failing, which is the failure this whole
// file exists to stop.
func TestAnUnhelpfulDatabaseMapFallsBackRatherThanBackingUpNothing(t *testing.T) {
	databasesDir := filepath.Join(t.TempDir(), "databases")
	if err := os.MkdirAll(databasesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	host := newFakeHost(t, "customer1")
	host.DatabasesDir = databasesDir

	for what, body := range map[string]string{
		"no MYSQL section":        `{"version":1}`,
		"an empty section":        `{"MYSQL":{},"version":1}`,
		"a section with no owner": `{"MYSQL":{"dbs":{"customer1_wp":"127.0.0.1"}},"version":1}`,
		"not json at all":         `{`,
	} {
		if err := os.WriteFile(filepath.Join(databasesDir, "customer1.json"),
			[]byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, recorded := host.recordedDatabases("customer1"); recorded {
			t.Errorf("%s was treated as cPanel's answer", what)
		}
	}
}
