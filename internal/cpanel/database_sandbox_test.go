package cpanel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The client stand-in distinguishes SQL sent as root from SQL sent with a
// separate login. The malicious statement must reach only the latter, and
// the setup/teardown must never contain backup-controlled SQL.
func TestDatabaseDumpUsesASchemaScopedLogin(t *testing.T) {
	r, log := databaseUserRestoreHost(t)
	script := `#!/bin/sh
scope=admin
for arg in "$@"; do
 case "$arg" in
 --print-defaults) echo 'mysql would have been started with the following arguments:'; echo '--socket=/run/mysql.sock --user=root --password=admin-secret'; exit 0;;
 --execute=*) echo localhost; exit 0;;
 --user=cpr_restore_*) scope=isolated;;
 esac
done
printf '%s\n' "$scope" >> ` + shellQuoteForTest(log) + `
cat >> ` + shellQuoteForTest(log) + `
if [ "$scope" = isolated ]; then exit 1; fi
`
	if err := os.WriteFile(r.MysqlPath, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(t.TempDir(), "dump.sql")
	if err := os.WriteFile(dump, []byte("DROP DATABASE another_customer;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.LoadDatabase(t.Context(), "customer1", "customer1_wp", dump); err == nil {
		t.Error("privileged SQL from the dump succeeded")
	}
	body, _ := os.ReadFile(log)
	got := string(body)
	if !strings.Contains(got, "isolated\nDROP DATABASE another_customer;") {
		t.Errorf("backup SQL was not isolated: %s", got)
	}
	if !strings.Contains(got, "ON `customer1\\_wp`.*") || strings.Contains(got, "ON *.*") {
		t.Errorf("the temporary login was not granted exactly the selected schema: %s", got)
	}
	if !strings.Contains(got, "DROP USER") {
		t.Errorf("temporary login was not removed after failure: %s", got)
	}
}
