package inventory

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/shuki/cprest/internal/granular"
	"github.com/shuki/cprest/internal/reassemble"
	"github.com/shuki/cprest/internal/resticrun"
)

// A cpmove archive as pkgacct writes one, cut down to the members a page
// asks about. The names are cPanel's own, read off a live 136.0.38.
var archiveMembers = map[string]string{
	"cpmove-studio/":                                   "",
	"cpmove-studio/apache_tls/":                        "",
	"cpmove-studio/apache_tls/studio.co.il":            "-----BEGIN CERTIFICATE-----\n",
	"cpmove-studio/apache_tls/shop.studio.co.il":       "-----BEGIN CERTIFICATE-----\n",
	"cpmove-studio/dnszones/":                          "",
	"cpmove-studio/dnszones/studio.co.il.db":           "$TTL 14400\n",
	"cpmove-studio/userdata/":                          "",
	"cpmove-studio/userdata/main":                      "main_domain: studio.co.il\n",
	"cpmove-studio/userdata/cache.json":                "{}",
	"cpmove-studio/userdata/studio.co.il":              "documentroot: /home/studio/public_html\n",
	"cpmove-studio/userdata/studio.co.il_SSL":          "documentroot: /home/studio/public_html\n",
	"cpmove-studio/userdata/studio.co.il.php-fpm.yaml": "---\n",
	"cpmove-studio/userdata/shop.studio.co.il":         "documentroot: /home/studio/shop\n",
	"cpmove-studio/cron/":                              "",
	"cpmove-studio/cron/studio": "SHELL=\"/usr/local/cpanel/bin/jailshell\"\nMAILTO=\"\"\n" +
		"# a comment\n*/15 * * * * /usr/local/bin/ea-php83 /home/studio/public_html/wp-cron.php >/dev/null 2>&1\n",
	"cpmove-studio/proftpdpasswd": "studio:$6$secrethash:1001:1001:studio:/home/studio:/bin/ftpsh\n" +
		"studio_logs:$6$anotherhash:1001:1001:studio:/home/studio/logs:/bin/ftpsh\n",
	"cpmove-studio/shadow": "studio:$6$accounthash:20000:0:99999:7:::\n",
}

const stagedAuth = `{"studio_wp":{"localhost":{"pass_hash":"2A46","auth_plugin":"mysql_native_password"}},` +
	`"studio_ro":{"localhost":{"pass_hash":"2A47","auth_plugin":"mysql_native_password"}}}`

const stagedGrants = "-- Database users and grants for studio\n" +
	"CREATE USER IF NOT EXISTS `studio_wp`@`localhost` IDENTIFIED WITH 'mysql_native_password' AS '*46';\n" +
	"GRANT ALL PRIVILEGES ON `studio\\_shop`.* TO 'studio_wp'@'localhost';\n" +
	"GRANT SELECT ON `studio\\_shop`.* TO 'studio_ro'@'localhost';\n"

// fakeRepo is a snapshot this package can read, with the paths a split
// backup produces.
var fakeParts = reassemble.Parts{
	Metadata:  "/var/lib/cprest/staging/stage-backup-studio/metadata",
	Homedir:   "/home/studio",
	Databases: "/var/lib/cprest/staging/stage-backup-studio/databases",
}

// fakeReader serves the snapshot without a restic binary.
type fakeReader struct {
	mu      sync.Mutex
	dumps   int
	noAuth  bool
	failing bool
}

func (f *fakeReader) read() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dumps
}

func (f *fakeReader) Ls(_ context.Context, _ resticrun.Repository, _ string,
	subpaths ...string) ([]resticrun.Entry, error) {
	return []resticrun.Entry{
		{Name: "cpmove-studio.tar", Type: "file",
			Path: fakeParts.Metadata + "/cpmove-studio.tar"},
	}, nil
}

func (f *fakeReader) Dump(_ context.Context, _ resticrun.Repository, _, file string,
	out io.Writer) error {

	f.mu.Lock()
	f.dumps++
	f.mu.Unlock()
	if f.failing {
		return fmt.Errorf("the repository could not be read")
	}
	switch {
	case strings.HasSuffix(file, ".tar"):
		archive := tar.NewWriter(out)
		for name, body := range archiveMembers {
			header := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}
			if strings.HasSuffix(name, "/") {
				header.Typeflag = tar.TypeDir
			}
			if err := archive.WriteHeader(header); err != nil {
				return err
			}
			if _, err := archive.Write([]byte(body)); err != nil {
				return err
			}
		}
		return archive.Close()
	case strings.HasSuffix(file, granular.DatabaseUsersAuthFile):
		if f.noAuth {
			return fmt.Errorf("no such file in the snapshot")
		}
		_, err := io.WriteString(out, stagedAuth)
		return err
	case strings.HasSuffix(file, granular.RunnableDatabaseUsersFile):
		_, err := io.WriteString(out, stagedGrants)
		return err
	}
	return fmt.Errorf("nothing at %s", file)
}

func source() Source {
	return Source{Key: "repo1", SnapshotID: strings.Repeat("a", 64), Parts: fakeParts}
}

// The point of the whole package: a page that names a category and nothing
// in it cannot answer the question somebody actually has, which is whether
// the thing they lost is in this restore point.
func TestABackupSaysWhatEachPartOfItHolds(t *testing.T) {
	for _, tc := range []struct {
		kind   granular.Kind
		labels []string
		names  []string
	}{
		{granular.KindDNS, []string{"studio.co.il"}, []string{"studio.co.il"}},
		{granular.KindSSL,
			[]string{"shop.studio.co.il", "studio.co.il"},
			[]string{"shop.studio.co.il", "studio.co.il"}},
		{granular.KindDomains,
			[]string{"shop.studio.co.il", "studio.co.il"},
			[]string{"shop.studio.co.il", "studio.co.il"}},
		{granular.KindDBUsers, []string{"studio_ro", "studio_wp"},
			[]string{"studio_ro", "studio_wp"}},
	} {
		cache := &Cache{}
		items, err := cache.Items(context.Background(), &fakeReader{}, source(), tc.kind)
		if err != nil {
			t.Errorf("%s: %v", tc.kind, err)
			continue
		}
		var labels, names []string
		for _, item := range items {
			labels = append(labels, item.Label)
			if item.Name != "" {
				names = append(names, item.Name)
			}
		}
		if strings.Join(labels, ",") != strings.Join(tc.labels, ",") {
			t.Errorf("%s listed %v, want %v", tc.kind, labels, tc.labels)
		}
		if strings.Join(names, ",") != strings.Join(tc.names, ",") {
			t.Errorf("%s offered %v, want %v", tc.kind, names, tc.names)
		}
	}
}

// A database user with no database to open is a login nothing uses, and a
// user that can open two is worth saying so about before somebody restores
// only one of them.
func TestADatabaseUserSaysWhatItCouldOpen(t *testing.T) {
	cache := &Cache{}
	items, err := cache.Items(context.Background(), &fakeReader{}, source(), granular.KindDBUsers)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Detail != "can open studio_shop" {
			t.Errorf("%s: detail %q", item.Name, item.Detail)
		}
	}
}

// A crontab is lines, and one line is told from another by what it runs.
// They are listed to be read, so they carry no name to be picked by.
func TestCronJobsAreListedByWhatTheyRun(t *testing.T) {
	cache := &Cache{}
	items, err := cache.Items(context.Background(), &fakeReader{}, source(), granular.KindCron)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("listed %+v", items)
	}
	if !strings.Contains(items[0].Label, "wp-cron.php") {
		t.Errorf("label %q", items[0].Label)
	}
	if items[0].Detail != "*/15 * * * *" {
		t.Errorf("schedule %q", items[0].Detail)
	}
	if items[0].Name != "" {
		t.Errorf("a cron line was offered as something to pick: %q", items[0].Name)
	}
}

// The file the FTP logins come from is a password file. Every line of it
// carries a hash, and none of it belongs on a page.
func TestFTPLoginsNeverCarryTheirPasswords(t *testing.T) {
	cache := &Cache{}
	items, err := cache.Items(context.Background(), &fakeReader{}, source(), granular.KindFTP)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("listed %+v", items)
	}
	for _, item := range items {
		for _, secret := range []string{"secrethash", "anotherhash", "$6$"} {
			if strings.Contains(item.Label+item.Detail, secret) {
				t.Fatalf("a password reached the page: %+v", item)
			}
		}
	}
	if items[0].Label != "studio" || items[0].Detail != "/home/studio" {
		t.Errorf("listed %+v", items[0])
	}
}

// Reading the archive is tens of megabytes out of a repository that may be
// across a network. Somebody looking through a backup clicks between its
// parts, and doing that again per click would make the page slower the more
// of it they used.
func TestTheArchiveIsReadOncePerRestorePoint(t *testing.T) {
	reader := &fakeReader{}
	cache := &Cache{}
	for _, kind := range []granular.Kind{
		granular.KindDNS, granular.KindSSL, granular.KindDomains,
		granular.KindCron, granular.KindFTP, granular.KindDBUsers,
	} {
		if _, err := cache.Items(context.Background(), reader, source(), kind); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	// The archive, the stored passwords and the grants: three files, read
	// once between them however many parts were asked about.
	if reader.read() != 3 {
		t.Errorf("read the snapshot %d times, want 3", reader.read())
	}
}

// A backup made before cprest staged the stored passwords still holds the
// grants. The users are worth listing from a backup that cannot restore
// them: knowing they were there is what tells somebody to look for a more
// recent restore point.
// Two pages opened together ask about the same restore point at the same
// time. The second waits for the first rather than streaming the archive
// out of the repository again -- and reads what the first left, which is
// what -race is here to check.
func TestTwoVisitsAtOnceReadTheSnapshotOnce(t *testing.T) {
	reader := &fakeReader{}
	cache := &Cache{}
	kinds := []granular.Kind{
		granular.KindDNS, granular.KindSSL, granular.KindDomains,
		granular.KindCron, granular.KindFTP, granular.KindDBUsers,
	}
	var wait sync.WaitGroup
	failures := make(chan error, len(kinds))
	for _, kind := range kinds {
		wait.Add(1)
		go func(kind granular.Kind) {
			defer wait.Done()
			items, err := cache.Items(context.Background(), reader, source(), kind)
			if err != nil {
				failures <- fmt.Errorf("%s: %w", kind, err)
				return
			}
			if len(items) == 0 {
				failures <- fmt.Errorf("%s: nothing listed", kind)
			}
		}(kind)
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if reader.read() != 3 {
		t.Errorf("read the snapshot %d times, want 3", reader.read())
	}
}

func TestUsersAreStillListedFromABackupWithoutTheirPasswords(t *testing.T) {
	cache := &Cache{}
	items, err := cache.Items(context.Background(), &fakeReader{noAuth: true},
		source(), granular.KindDBUsers)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("listed %+v", items)
	}
}

// A repository that cannot be read is reported, not remembered: the next
// click should try again rather than repeat the same failure for as long
// as a reading is kept.
func TestAFailedReadingIsNotRemembered(t *testing.T) {
	reader := &fakeReader{failing: true}
	cache := &Cache{}
	for i := 0; i < 2; i++ {
		if _, err := cache.Items(context.Background(), reader, source(), granular.KindDNS); err == nil {
			t.Fatal("a repository that could not be read was reported as empty")
		}
	}
	if reader.read() != 2 {
		t.Errorf("tried %d times, want 2", reader.read())
	}
}

// The parts of an account that are files are listed by restic itself, out
// of the snapshot's own paths. Nothing here reads them, and asking costs
// nothing.
func TestTheFileKindsAreNotReadFromTheArchive(t *testing.T) {
	reader := &fakeReader{}
	cache := &Cache{}
	for _, kind := range []granular.Kind{
		granular.KindFiles, granular.KindMailbox, granular.KindDatabase,
		granular.KindWebsite, granular.KindSettings,
	} {
		items, err := cache.Items(context.Background(), reader, source(), kind)
		if err != nil || items != nil {
			t.Errorf("%s: %v %+v", kind, err, items)
		}
	}
	if reader.read() != 0 {
		t.Errorf("read the snapshot %d times for kinds that are files", reader.read())
	}
}

// A grant on a pattern covers every database matching it, which is not one
// database. Restoring it against whichever database the pattern happens to
// look like is worse than saying so.
func TestAGrantOnAPatternIsRefused(t *testing.T) {
	if _, err := ParseGrants([]byte(
		"GRANT ALL PRIVILEGES ON `studio_%`.* TO 'studio_wp'@'localhost';\n")); err == nil {
		t.Error("a grant on a pattern was read as a grant on a database")
	}
	// The underscore MySQL escapes is the database's own, not a wildcard.
	grants, err := ParseGrants([]byte(bytes.NewBufferString(
		"GRANT SELECT ON `studio\\_shop`.* TO 'studio_ro'@'localhost';\n").String()))
	if err != nil {
		t.Fatal(err)
	}
	got := grants["studio_ro@localhost"]
	if len(got) != 1 || got[0].Database != "studio_shop" {
		t.Errorf("read %+v", got)
	}
}
