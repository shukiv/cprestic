package nodestore

import (
	"path/filepath"
	"testing"
)

// A server that was installed as cprest keeps its directories' old names
// inside the state file: the settings say where staging and the restic
// cache are, and every SFTP destination names the private key it connects
// with. The installer moves those directories; nothing moves what the
// state file says about them, and a destination whose key file is named
// under a directory that no longer exists cannot be reached at all.
func TestTheOldInstallationsPathsAreRewritten(t *testing.T) {
	store := openTestStore(t)

	settings := DefaultSettings()
	settings.StagingRoot = "/var/lib/cprest/staging"
	settings.ResticCache = "/var/cache/cprest/restic"
	settings.ConfigDir = "/etc/cprest"
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	dest, err := store.PutDestination(Destination{Name: "spare disk", Type: "sftp", Config: map[string]string{
		"host":             "backup.example",
		"root":             "/srv/restic",
		"identity_file":    "/etc/cprest/keys/prepared-d70bc05ea67d19aa",
		"known_hosts_file": "/etc/cprest/known_hosts",
	}})
	if err != nil {
		t.Fatal(err)
	}

	changed, err := store.MigrateLegacyPaths()
	if err != nil {
		t.Fatal(err)
	}
	if changed != 2 {
		t.Errorf("changed %d records, want the settings and the one destination", changed)
	}

	after, err := store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if after.StagingRoot != "/var/lib/gniza/staging" ||
		after.ResticCache != "/var/cache/gniza/restic" ||
		after.ConfigDir != "/etc/gniza" {
		t.Errorf("settings still name the old directories: %+v", after)
	}

	moved, err := store.Destination(dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Config["identity_file"] != "/etc/gniza/keys/prepared-d70bc05ea67d19aa" {
		t.Errorf("identity file is %q", moved.Config["identity_file"])
	}
	if moved.Config["known_hosts_file"] != "/etc/gniza/known_hosts" {
		t.Errorf("known hosts file is %q", moved.Config["known_hosts_file"])
	}
	if moved.Config["root"] != "/srv/restic" || moved.Config["host"] != "backup.example" {
		t.Errorf("a path on the backup server was rewritten as if it were local: %+v", moved.Config)
	}
}

// Running it twice must not be different from running it once, because it
// runs on every start.
func TestRewritingThePathsAgainChangesNothing(t *testing.T) {
	store := openTestStore(t)
	settings := DefaultSettings()
	settings.ConfigDir = "/etc/cprest"
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MigrateLegacyPaths(); err != nil {
		t.Fatal(err)
	}
	changed, err := store.MigrateLegacyPaths()
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Errorf("changed %d records on the second run, want none", changed)
	}
}

// The old directory names are a prefix of other names an operator may have
// chosen. A local destination at /var/lib/cprest-archive is not inside the
// directory the installer moved, and rewriting it would point a working
// destination at somewhere that does not exist.
func TestOnlyWholeDirectoryNamesAreRewritten(t *testing.T) {
	store := openTestStore(t)
	dest, err := store.PutDestination(Destination{Name: "archive", Type: "local", Config: map[string]string{
		"path": "/var/lib/cprest-archive/repo",
	}})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := store.MigrateLegacyPaths()
	if err != nil {
		t.Fatal(err)
	}
	kept, err := store.Destination(dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 || kept.Config["path"] != "/var/lib/cprest-archive/repo" {
		t.Errorf("changed %d records, path is now %q", changed, kept.Config["path"])
	}
}

// A server installed as Gniza has nothing to migrate, and must not be
// given a settings record it never had: the defaults are read when there
// is none, and writing one freezes today's defaults into it forever.
func TestAFreshServerIsNotGivenSettingsItNeverSaved(t *testing.T) {
	store := openTestStore(t)
	changed, err := store.MigrateLegacyPaths()
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Errorf("changed %d records on a server with nothing stored", changed)
	}
	if _, err := store.rawSettings(); err == nil {
		t.Error("a settings record was written for a server that had none")
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
