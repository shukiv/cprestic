package cpanel

import (
	"os"
	"path/filepath"
	"testing"
)

// A set-user-ID bit that was on a file when the backup was taken must not
// come back with it. The account could set one on its own file again, so
// this is not what stops them: it stops a restore from quietly undoing the
// removal of one, which is a thing done after a compromise.
func TestSetIDBitsDoNotSurviveAStagedTree(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "bin")
	if err := os.MkdirAll(nested, 0o2755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(nested, "helper")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(binary, 0o4755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nested, os.FileMode(0o755)|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}

	if err := dropSetIDBits(root); err != nil {
		t.Fatalf("dropSetIDBits: %v", err)
	}

	for _, path := range []string{binary, nested} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			t.Errorf("%s kept mode %v", path, info.Mode())
		}
	}
	// The ordinary permissions are what the account is getting back, so
	// they have to survive.
	info, err := os.Stat(binary)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("the executable bit was lost with the setuid bit: %v", info.Mode())
	}
}

func TestADatabaseIsLoadedOnlyIntoTheAccountThatOwnsIt(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "c1_shop.sql")
	if err := os.WriteFile(dump, []byte("-- dump\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &Fake{Databases: map[string][]string{"c1": {"c1_shop"}}}
	if err := fake.LoadDatabase(t.Context(), "c2", "c1_shop", dump); err == nil {
		t.Fatal("another account's database was loaded")
	}
	if err := fake.LoadDatabase(t.Context(), "c1", "c1_shop", dump); err != nil {
		t.Fatalf("the account's own database was refused: %v", err)
	}
}
