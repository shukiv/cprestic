package cpanel

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run only inside a disposable database container. Never enable this on a
// hosting server: the fixture deliberately creates and drops two databases.
func TestIsolatedDatabaseImportAgainstMySQL(t *testing.T) {
	if os.Getenv("CPREST_DISPOSABLE_MYSQL_TEST") != "1" {
		t.Skip("requires a disposable MySQL server; set CPREST_DISPOSABLE_MYSQL_TEST=1 only in a test container")
	}
	admin := func(sql string) string {
		t.Helper()
		cmd := exec.Command("mysql", "--batch", "--skip-column-names")
		cmd.Stdin = strings.NewReader(sql)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture SQL failed: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	admin("CREATE DATABASE cpresttest_owned; CREATE DATABASE cpresttest_other; CREATE TABLE cpresttest_other.keep_me (id INT); INSERT INTO cpresttest_other.keep_me VALUES (42);")
	t.Cleanup(func() { admin("DROP DATABASE cpresttest_owned; DROP DATABASE cpresttest_other;") })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpresttest.json"), []byte(`{"MYSQL":{"owner":"cpresttest","dbs":{"cpresttest_owned":{}},"dbusers":{},"noprefix":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	r := &Real{MysqlPath: "mysql", DatabasesDir: dir}
	for name, sql := range map[string]string{
		"normal":           "CREATE TABLE posts (id INT); INSERT INTO posts VALUES (1);",
		"qualified schema": "DROP TABLE cpresttest_other.keep_me;",
		"server login":     "CREATE USER 'cprest_unauthorized'@'localhost' IDENTIFIED BY 'unauthorized';",
		"server file":      "SELECT 'test' INTO OUTFILE '/tmp/cprest-unexpected-outfile';",
		"global privilege": "GRANT ALL ON *.* TO 'cpresttest'@'localhost';",
		"definer object":   "CREATE DEFINER='root'@'localhost' VIEW stolen AS SELECT * FROM cpresttest_other.keep_me;",
	} {
		t.Run(name, func(t *testing.T) {
			dump := filepath.Join(t.TempDir(), "dump.sql")
			if err := os.WriteFile(dump, []byte(sql), 0600); err != nil {
				t.Fatal(err)
			}
			err := r.LoadDatabase(t.Context(), "cpresttest", "cpresttest_owned", dump)
			if name == "normal" && err != nil {
				t.Fatalf("legitimate table import failed: %v", err)
			}
			if name != "normal" && err == nil {
				t.Fatalf("unsafe SQL succeeded: %s", name)
			}
			if got := admin("SELECT COUNT(*) FROM mysql.user WHERE User LIKE 'cpr_restore_%';"); got != "0" {
				t.Fatalf("restore logins leaked: %s", got)
			}
			if got := admin("SELECT id FROM cpresttest_other.keep_me;"); got != "42" {
				t.Fatalf("another schema changed: %s", got)
			}
		})
	}
	if got := admin("SELECT id FROM cpresttest_owned.posts;"); got != "1" {
		t.Fatalf("table data was not restored: %s", got)
	}
}
