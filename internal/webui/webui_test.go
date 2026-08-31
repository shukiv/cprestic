package webui_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
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
	"github.com/shuki/cprest/internal/webui"
)

// newUI stands the interface up on a socket with a synthetic cPanel host
// behind it, which is how it is exercised off a real server.
func newUI(t *testing.T) (*http.Client, string, *node.Engine) {
	t.Helper()
	root := t.TempDir()

	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.Hostname = "cp01.example.com"
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}

	keyHex, err := vault.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "master.key")
	if err := os.WriteFile(keyPath, []byte(keyHex), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := vault.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.New(key)
	if err != nil {
		t.Fatal(err)
	}

	engine, err := node.New(node.Config{
		Store: store, Vault: v,
		Provider: &cpanel.Fake{
			Root:      filepath.Join(root, "cpanel"),
			Databases: map[string][]string{"customer1": {"customer1_wp"}},
			FileCount: 2, FileSize: 512,
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("build engine: %v", err)
	}

	server, err := webui.New(engine, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("build ui: %v", err)
	}

	socket := filepath.Join(root, "ui.sock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = server.Listen(ctx, socket) }()

	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	waitForSocket(t, socket)
	return client, socket, engine
}

func waitForSocket(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", socket); err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the interface never started listening")
}

func get(t *testing.T, client *http.Client, path string) (int, string) {
	t.Helper()
	resp, err := client.Get("http://ui" + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestEveryPageRenders(t *testing.T) {
	client, _, _ := newUI(t)

	for _, path := range []string{
		"/", "/destinations", "/schedule", "/accounts",
		"/restore", "/jobs", "/settings",
	} {
		status, body := get(t, client, path)
		if status != http.StatusOK {
			t.Errorf("GET %s = %d", path, status)
			continue
		}
		// The page is a fragment: WHM's own interface supplies the
		// document around it, so there is no <html> of our own.
		if !strings.Contains(body, `<div class="cprest">`) {
			t.Errorf("GET %s did not render the layout", path)
		}
		if !strings.Contains(body, "</main>") {
			t.Errorf("GET %s produced a truncated page", path)
		}
		if strings.Contains(body, "<html") {
			t.Errorf("GET %s emitted a whole document; WHM supplies that", path)
		}
	}
}

func TestSocketIsOwnerOnly(t *testing.T) {
	_, socket, _ := newUI(t)

	// On a shared cPanel server the untrusted users are already on the
	// box, and this interface can read every stored credential.
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("socket mode %04o is reachable by other users", perm)
	}
	dir, err := os.Stat(filepath.Dir(socket))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("socket directory mode %04o is reachable by other users", perm)
	}
}

func TestMutatingRequestsNeedTheCSRFToken(t *testing.T) {
	client, _, _ := newUI(t)

	resp, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"name": {"sneaky"}, "type": {"local"}, "root": {"/tmp/x"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST without a token = %d, want 403", resp.StatusCode)
	}
}

func TestAddDestinationRoundTrip(t *testing.T) {
	client, _, engine := newUI(t)

	_, page := get(t, client, "/destinations")
	token := csrfToken(t, page)

	root := t.TempDir()
	resp, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {token}, "name": {"Local disk"}, "type": {"local"},
		"root": {root}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST = %d, want a redirect", resp.StatusCode)
	}

	destinations, err := engine.Store().Destinations()
	if err != nil {
		t.Fatal(err)
	}
	if len(destinations) != 1 || destinations[0].Name != "Local disk" {
		t.Fatalf("stored destinations = %+v", destinations)
	}
	// The repository is created alongside, so a schedule has something to
	// point at without a second step.
	repos, err := engine.Store().Repositories()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Path != "cp01" {
		t.Fatalf("stored repositories = %+v", repos)
	}
}

func TestSecondDestinationCopiesChunkerParameters(t *testing.T) {
	client, _, engine := newUI(t)

	for _, name := range []string{"First", "Second"} {
		_, page := get(t, client, "/destinations")
		resp, err := client.PostForm("http://ui/destinations/add", map[string][]string{
			"csrf": {csrfToken(t, page)}, "name": {name}, "type": {"local"},
			"root": {t.TempDir()}, "repo_path": {"cp01"},
		})
		if err != nil {
			t.Fatalf("POST %s: %v", name, err)
		}
		resp.Body.Close()
	}

	repos, err := engine.Store().Repositories()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repositories, want 2", len(repos))
	}
	// Chunker parameters are fixed when a repository is created and can
	// never be changed, so the second must copy the first's.
	if repos[0].ChunkerSourceRepoID != "" {
		t.Errorf("the first repository has a chunker source: %q", repos[0].ChunkerSourceRepoID)
	}
	if repos[1].ChunkerSourceRepoID != repos[0].ID {
		t.Errorf("second chunker source = %q, want %q",
			repos[1].ChunkerSourceRepoID, repos[0].ID)
	}
}

func TestScheduleWithNoDestinationIsRefused(t *testing.T) {
	client, _, _ := newUI(t)

	// With nothing configured the page offers no form at all, because a
	// schedule with nowhere to write is not worth filling in.
	_, page := get(t, client, "/schedule")
	if strings.Contains(page, `action="schedule/save"`) {
		t.Error("the schedule form is offered before any destination exists")
	}
	if !strings.Contains(page, "Add a destination first") {
		t.Error("the page does not say what to do first")
	}

	// Once one exists the form appears, but a schedule that selects no
	// destination is still refused: it would run and store nothing.
	_, destinationPage := get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, destinationPage)}, "name": {"Local disk"},
		"type": {"local"}, "root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatalf("add destination: %v", err)
	}
	added.Body.Close()

	_, page = get(t, client, "/schedule")
	resp, err := client.PostForm("http://ui/schedule/save", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Nightly"}, "cron": {"0 2 * * *"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	location := resp.Header.Get("Location")
	if !strings.Contains(location, "kind=error") {
		t.Errorf("redirect = %q, want an error", location)
	}
}

// csrfToken pulls the token out of a rendered page.
func csrfToken(t *testing.T, page string) string {
	t.Helper()
	const marker = `name="csrf" value="`
	start := strings.Index(page, marker)
	if start < 0 {
		t.Fatal("no csrf token in the page")
	}
	rest := page[start+len(marker):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatal("malformed csrf token")
	}
	return rest[:end]
}

func TestSFTPDestinationReportsAnUnreachableHost(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/destinations")
	resp, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Backup server"}, "type": {"sftp"},
		// Nothing listens here, so the host key cannot be read.
		"host": {"127.0.0.1"}, "port": {"1"},
		"user": {"cpbackup"}, "root": {"/home/cpbackup/backups"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	location := resp.Header.Get("Location")
	if !strings.Contains(location, "kind=error") {
		t.Fatalf("redirect = %q, want an error", location)
	}
	if !strings.Contains(location, "SSH") {
		t.Errorf("redirect = %q, want it to name the problem", location)
	}
}

func TestSFTPFormAsksForNeitherAKeyNorKnownHosts(t *testing.T) {
	client, _, _ := newUI(t)
	_, page := get(t, client, "/destinations")

	// cprest generates its own key and learns the host key itself, so
	// neither is something an operator should have to prepare.
	if strings.Contains(page, `name="known_hosts_file"`) {
		t.Error("the form still asks for a known_hosts file")
	}
	if !strings.Contains(page, `name="password"`) {
		t.Error("the form has no password field for installing the key")
	}
	if !strings.Contains(page, "makes its own key") {
		t.Error("the form does not say that a key will be generated")
	}
}

func TestRedirectsStayRelativeSoWHMKeepsItsToken(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/destinations")
	resp, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {""}, "type": {"local"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	// WHM serves the plugin behind a /cpsessNNN token in the path. An
	// absolute-path redirect drops it and the browser lands on a 401, so
	// every redirect has to be a bare query string.
	location := resp.Header.Get("Location")
	if !strings.HasPrefix(location, "?") {
		t.Errorf("Location = %q, want it to start with ?", location)
	}
	if strings.Contains(location, "/destinations") {
		t.Errorf("Location = %q, want no path component", location)
	}
}

func TestEditSchedule(t *testing.T) {
	client, _, engine := newUI(t)

	// A destination to point the schedule at.
	_, page := get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Local disk"}, "type": {"local"},
		"root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added.Body.Close()
	repos, err := engine.Store().Repositories()
	if err != nil || len(repos) != 1 {
		t.Fatalf("repositories = %+v (%v)", repos, err)
	}

	_, page = get(t, client, "/schedule")
	saved, err := client.PostForm("http://ui/schedule/save", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Nightly"}, "cron": {"0 2 * * *"},
		"repository": {repos[0].ID}, "scope": {"all"}, "enabled": {"1"},
		"keep_daily": {"7"}, "mode": {"split"},
	})
	if err != nil {
		t.Fatal(err)
	}
	saved.Body.Close()

	policies, err := engine.Store().Policies()
	if err != nil || len(policies) != 1 {
		t.Fatalf("policies = %+v (%v)", policies, err)
	}
	original := policies[0]

	// The edit form must arrive filled in, or an operator retypes
	// everything and loses whatever they forget.
	editPage := func() string {
		t.Helper()
		status, body := get(t, client, "/schedule?edit="+original.ID)
		if status != http.StatusOK {
			t.Fatalf("GET edit page = %d", status)
		}
		return body
	}
	form := editPage()
	for _, want := range []string{
		`value="Nightly"`, `value="0 2 * * *"`,
		`name="id" value="` + original.ID + `"`,
		"Save changes",
	} {
		if !strings.Contains(form, want) {
			t.Errorf("the edit form is missing %q", want)
		}
	}

	// Saving with the id updates rather than creating a second schedule.
	updated, err := client.PostForm("http://ui/schedule/save", map[string][]string{
		"csrf": {csrfToken(t, form)}, "id": {original.ID},
		"name": {"Weekly"}, "cron": {"0 3 * * 0"},
		"repository": {repos[0].ID}, "scope": {"all"}, "enabled": {"1"},
		"keep_daily": {"14"}, "mode": {"split"},
	})
	if err != nil {
		t.Fatal(err)
	}
	updated.Body.Close()

	policies, err = engine.Store().Policies()
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 1 {
		t.Fatalf("editing produced %d schedules, want 1", len(policies))
	}
	if policies[0].Name != "Weekly" || policies[0].ScheduleCron != "0 3 * * 0" {
		t.Errorf("policy = %+v", policies[0])
	}
	if policies[0].Retention.KeepDaily != 14 {
		t.Errorf("retention = %+v", policies[0].Retention)
	}
	if policies[0].CreatedAt != original.CreatedAt {
		t.Error("editing changed when the schedule was created")
	}
}

func TestEditDestinationKeepsCredentialsAndRepositoryPath(t *testing.T) {
	client, _, engine := newUI(t)

	_, page := get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Wasabi"}, "type": {"s3"},
		"bucket": {"cp-backups"}, "region": {"us-east-1"},
		"endpoint":          {"s3.us-east-1.wasabisys.com"},
		"access_key_id":     {"AKIA-ORIGINAL"},
		"secret_access_key": {"SECRET-ORIGINAL"},
		"repo_path":         {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added.Body.Close()

	destinations, err := engine.Store().Destinations()
	if err != nil || len(destinations) != 1 {
		t.Fatalf("destinations = %+v (%v)", destinations, err)
	}
	original := destinations[0]

	status, form := get(t, client, "/destinations?edit="+original.ID)
	if status != http.StatusOK {
		t.Fatalf("GET edit page = %d", status)
	}
	for _, want := range []string{`value="Wasabi"`, `value="cp-backups"`, "Save changes"} {
		if !strings.Contains(form, want) {
			t.Errorf("the edit form is missing %q", want)
		}
	}

	// Correcting the bucket must not require retyping the secret key.
	edited, err := client.PostForm("http://ui/destinations/edit", map[string][]string{
		"csrf": {csrfToken(t, form)}, "id": {original.ID},
		"name": {"Wasabi Miami"}, "bucket": {"cp-backups-2"},
		"region": {"us-east-1"}, "endpoint": {"s3.us-east-1.wasabisys.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	edited.Body.Close()

	destinations, err = engine.Store().Destinations()
	if err != nil || len(destinations) != 1 {
		t.Fatalf("editing produced %d destinations", len(destinations))
	}
	updated := destinations[0]
	if updated.Name != "Wasabi Miami" || updated.Config["bucket"] != "cp-backups-2" {
		t.Errorf("destination = %+v", updated)
	}
	if updated.CredentialsSecretID != original.CredentialsSecretID {
		t.Error("the stored credentials were replaced by an empty form")
	}

	// The repository is where the backups already are.
	repos, err := engine.Store().Repositories()
	if err != nil || len(repos) != 1 {
		t.Fatalf("repositories = %+v", repos)
	}
	if repos[0].Path != "cp01" {
		t.Errorf("repository path changed to %q", repos[0].Path)
	}
}

func TestRunScheduleNow(t *testing.T) {
	client, _, engine := newUI(t)

	_, page := get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Local disk"}, "type": {"local"},
		"root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added.Body.Close()
	repos, err := engine.Store().Repositories()
	if err != nil || len(repos) != 1 {
		t.Fatalf("repositories = %+v (%v)", repos, err)
	}

	_, page = get(t, client, "/schedule")
	saved, err := client.PostForm("http://ui/schedule/save", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Nightly"}, "cron": {"0 2 * * *"},
		"repository": {repos[0].ID}, "scope": {"all"}, "enabled": {"1"}, "mode": {"split"},
	})
	if err != nil {
		t.Fatal(err)
	}
	saved.Body.Close()
	policies, err := engine.Store().Policies()
	if err != nil || len(policies) != 1 {
		t.Fatalf("policies = %+v", policies)
	}

	_, page = get(t, client, "/schedule")
	if !strings.Contains(page, "Run now") {
		t.Fatal("the schedules page offers no way to run one")
	}
	run, err := client.PostForm("http://ui/schedule/run", map[string][]string{
		"csrf": {csrfToken(t, page)}, "id": {policies[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	run.Body.Close()
	if location := run.Header.Get("Location"); !strings.Contains(location, "kind=ok") {
		t.Fatalf("redirect = %q, want success", location)
	}

	jobs, err := engine.Store().Jobs(0)
	if err != nil {
		t.Fatal(err)
	}
	// The fake host has one account, so one job should be waiting.
	if len(jobs) != 1 || jobs[0].Account != "customer1" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if jobs[0].Status != job.StatusPending {
		t.Errorf("job status = %q, want pending", jobs[0].Status)
	}

	// Running by hand must not move the schedule's marker, or tonight's
	// run would be skipped.
	after, err := engine.Store().Policy(policies[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastRunAt != nil {
		t.Error("a manual run moved the schedule's last-fired time")
	}

	// A second press while that account is still queued is a skip, not a
	// second job for the same account.
	_, page = get(t, client, "/schedule")
	again, err := client.PostForm("http://ui/schedule/run", map[string][]string{
		"csrf": {csrfToken(t, page)}, "id": {policies[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	again.Body.Close()
	if location := again.Header.Get("Location"); !strings.Contains(location, "kind=warn") {
		t.Errorf("second run redirect = %q, want a warning", location)
	}
	jobs, _ = engine.Store().Jobs(0)
	if len(jobs) != 1 {
		t.Errorf("a second press queued %d jobs for one account", len(jobs))
	}
}

func TestRunScheduleWithNoDestinationIsRefused(t *testing.T) {
	client, _, engine := newUI(t)

	// Written straight to the store: the form will not save one like this,
	// but a schedule can end up here if its destination was removed.
	policy, err := engine.Store().PutPolicy(nodestore.Policy{
		Name: "Orphaned", ScheduleCron: "0 2 * * *", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/schedule")
	resp, err := client.PostForm("http://ui/schedule/run", map[string][]string{
		"csrf": {csrfToken(t, page)}, "id": {policy.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	location := resp.Header.Get("Location")
	if !strings.Contains(location, "kind=error") || !strings.Contains(location, "nowhere") {
		t.Errorf("redirect = %q, want it to say there is nowhere to send a backup", location)
	}
}

func TestDownloadStreamsARebuiltArchive(t *testing.T) {
	client, _, engine := newUI(t)

	// A finished restore, with its archive where the engine puts them.
	staging := engine.Settings().StagingRoot
	dir := filepath.Join(staging, "stage-restore-customer1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "cpmove-customer1.tar")
	body := []byte("not really a tar, but the bytes should arrive intact")
	if err := os.WriteFile(archive, body, 0o600); err != nil {
		t.Fatal(err)
	}
	restore, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc",
		Status: job.StatusSuccess, ArchivePath: archive,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Get("http://ui/download?id=" + restore.ID)
	if err != nil {
		t.Fatalf("GET download: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download = %d", resp.StatusCode)
	}
	// Content-Disposition is what makes the browser save the file and is
	// the only place the name survives, because cpsrvd strips
	// Content-Type from what the plugin returns.
	disposition := resp.Header.Get("Content-Disposition")
	if !strings.Contains(disposition, `filename="cpmove-customer1.tar"`) {
		t.Errorf("Content-Disposition = %q", disposition)
	}
	if !strings.HasPrefix(disposition, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", disposition)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(body) {
		t.Errorf("downloaded %d bytes, want %d", len(got), len(body))
	}
}

func TestDownloadRefusesAnArchiveOutsideStaging(t *testing.T) {
	client, _, engine := newUI(t)

	// The path is ours, not the browser's, but it still becomes an open(),
	// so it is checked against the staging root.
	outside := filepath.Join(t.TempDir(), "passwd.tar")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc",
		Status: job.StatusSuccess, ArchivePath: outside,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Get("http://ui/download?id=" + restore.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("download = %d, want a redirect with an error", resp.StatusCode)
	}
	if location := resp.Header.Get("Location"); !strings.Contains(location, "kind=error") {
		t.Errorf("redirect = %q", location)
	}
}

func TestDownloadOfAnUnfinishedRestoreIsRefused(t *testing.T) {
	client, _, engine := newUI(t)

	restore, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc", Status: job.StatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get("http://ui/download?id=" + restore.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("download = %d, want a redirect with an error", resp.StatusCode)
	}
}

func TestDownloadRequestNeedsABackup(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/accounts")
	resp, err := client.PostForm("http://ui/accounts/download", map[string][]string{
		"csrf": {csrfToken(t, page)}, "account": {"customer1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	location := resp.Header.Get("Location")
	if !strings.Contains(location, "kind=error") {
		t.Fatalf("redirect = %q, want an error", location)
	}
	if !strings.Contains(location, "no+backup+yet") {
		t.Errorf("redirect = %q, want it to say there is no backup", location)
	}
}

func TestDownloadServesAnExistingArchiveInsteadOfRebuilding(t *testing.T) {
	client, _, engine := newUI(t)

	staging := engine.Settings().StagingRoot
	dir := filepath.Join(staging, "keep-restore-customer1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "cpmove-customer1.tar")
	if err := os.WriteFile(archive, []byte("already rebuilt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc",
		Status: job.StatusSuccess, ArchivePath: archive,
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/accounts")
	resp, err := client.PostForm("http://ui/accounts/download", map[string][]string{
		"csrf": {csrfToken(t, page)}, "account": {"customer1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Rebuilding takes minutes. If the archive is already there, pressing
	// Download must hand it over rather than queue a copy of it.
	location := resp.Header.Get("Location")
	if !strings.Contains(location, "p=download") {
		t.Fatalf("redirect = %q, want it to go straight to the download", location)
	}
	if restores, _ := engine.Store().Restores(0); len(restores) != 1 {
		t.Errorf("pressing Download queued another rebuild: %d restores", len(restores))
	}
}

func TestDownloadRebuildsWhenTheArchiveIsGone(t *testing.T) {
	client, _, engine := newUI(t)

	// A finished restore whose archive has since been removed.
	if _, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc", Status: job.StatusSuccess,
		ArchivePath: filepath.Join(engine.Settings().StagingRoot, "gone", "cpmove.tar"),
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/accounts")
	resp, err := client.PostForm("http://ui/accounts/download", map[string][]string{
		"csrf": {csrfToken(t, page)}, "account": {"customer1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// No backup exists in this test, so it should report that rather than
	// silently offering a download of nothing.
	location := resp.Header.Get("Location")
	if !strings.Contains(location, "kind=error") {
		t.Errorf("redirect = %q, want an error about there being no backup", location)
	}
}

func TestDownloadRedirectIsAQueryOnlyReference(t *testing.T) {
	client, _, engine := newUI(t)

	staging := engine.Settings().StagingRoot
	dir := filepath.Join(staging, "keep-restore-customer1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "cpmove-customer1.tar")
	if err := os.WriteFile(archive, []byte("rebuilt"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc",
		Status: job.StatusSuccess, ArchivePath: archive,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/accounts")
	resp, err := client.PostForm("http://ui/accounts/download", map[string][]string{
		"csrf": {csrfToken(t, page)}, "account": {"customer1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// http.Redirect resolves a query-only reference against the request
	// path, which turns this into "?p=accounts/" and loses the route.
	location := resp.Header.Get("Location")
	if !strings.HasPrefix(location, "?p=download") {
		t.Errorf("Location = %q, want it to start with ?p=download", location)
	}
	if !strings.Contains(location, restore.ID) {
		t.Errorf("Location = %q, want the restore id", location)
	}
}

func TestAccountsListMakesNoRemoteCalls(t *testing.T) {
	client, _, engine := newUI(t)

	// A destination whose repository cannot be reached at all. Listing
	// accounts must not touch it: one round trip per repository across
	// every account is the slow-page mistake this design avoids.
	_, page := get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Local disk"}, "type": {"local"},
		"root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added.Body.Close()

	started := time.Now()
	status, body := get(t, client, "/accounts")
	if status != http.StatusOK {
		t.Fatalf("GET accounts = %d", status)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("listing accounts took %s; it is talking to a destination", elapsed)
	}
	if !strings.Contains(body, "customer1") {
		t.Error("the account is missing from the list")
	}
	// The state filter and search are what make the page survive a server
	// with hundreds of accounts.
	if !strings.Contains(body, `data-filter="#accounts"`) {
		t.Error("the list has no search")
	}
	if !strings.Contains(body, `data-state="bad"`) {
		t.Error("the list has no state filter")
	}
	_ = engine
}

func TestAccountDetailShowsSnapshotsAndActivity(t *testing.T) {
	client, _, engine := newUI(t)

	// Some history for the account, so the page has something to show.
	finished := time.Now().Add(-time.Hour)
	if _, err := engine.Store().PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusSuccess, FinishedAt: &finished,
		Targets: []nodestore.JobTarget{{
			RepositoryID: "repo", Status: job.TargetSuccess, SnapshotID: "40dc1520",
			BytesAdded: 1024, BytesProcessed: 8192,
			Detail:     "error: lstat /home/customer1/tmp/sess_9f2b: no such file",
			Incomplete: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc", Kind: node.KindVerify,
		Status: job.StatusSuccess, FinishedAt: &finished,
		Detail: "archive present; account tree present; 12 files in the home directory",
	}); err != nil {
		t.Fatal(err)
	}

	status, body := get(t, client, "/account?user=customer1")
	if status != http.StatusOK {
		t.Fatalf("GET account = %d", status)
	}
	for _, want := range []string{
		"customer1",
		"1.0 KiB stored of 8.0 KiB read",
		"Restore rehearsed",
		"archive present; account tree present",
		"What restic reported",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the account page is missing %q", want)
		}
	}
}

func TestAccountDetailRejectsAnUnknownAccount(t *testing.T) {
	client, _, _ := newUI(t)

	resp, err := client.Get("http://ui/account?user=nosuchaccount")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET = %d, want a redirect", resp.StatusCode)
	}
	if location := resp.Header.Get("Location"); !strings.Contains(location, "kind=error") {
		t.Errorf("redirect = %q, want an error", location)
	}
}

func TestOverviewLeadsWithCoverage(t *testing.T) {
	client, _, engine := newUI(t)

	status, body := get(t, client, "/")
	if status != http.StatusOK {
		t.Fatalf("GET overview = %d", status)
	}
	// The question an operator actually has is whether everything is
	// protected, so that is what the page opens with.
	for _, want := range []string{
		"of 1 accounts have a usable backup",
		"Staging space",
		// With nothing configured, the first thing to say is what to do.
		"No backup destination yet",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the overview is missing %q", want)
		}
	}

	// Once a destination exists, the unprotected accounts become the
	// thing worth saying.
	_, page := get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Local disk"}, "type": {"local"},
		"root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added.Body.Close()

	_, body = get(t, client, "/")
	if !strings.Contains(body, "never been backed up") {
		t.Error("the overview does not say that an account has no backup")
	}
	_ = engine
}

func TestRoutesTravelInTheQueryParameter(t *testing.T) {
	client, _, _ := newUI(t)

	// cpsrvd will not route a path after the CGI's name, so the plugin
	// forwards the route as "p" and the service turns it back.
	status, body := get(t, client, "/?p=accounts")
	if status != http.StatusOK {
		t.Fatalf("GET ?p=accounts = %d", status)
	}
	if !strings.Contains(body, "Back up, verify or recover") {
		t.Error("?p=accounts did not reach the accounts page")
	}

	// Other parameters survive the translation.
	status, body = get(t, client, "/?p=account&user=customer1")
	if status != http.StatusOK {
		t.Fatalf("GET ?p=account = %d", status)
	}
	if !strings.Contains(body, "customer1") {
		t.Error("?p=account&user=… did not reach the account page")
	}

	// No route is the overview.
	if _, body := get(t, client, "/"); !strings.Contains(body, "Protection") {
		t.Error("the bare address did not reach the overview")
	}
}

func TestRejectsARouteThatIsNotOurs(t *testing.T) {
	client, _, _ := newUI(t)

	for _, route := range []string{"../../etc/passwd", "a//b", "a%20b", "%2e%2e%2fetc"} {
		resp, err := client.Get("http://ui/?p=" + route)
		if err != nil {
			t.Fatalf("GET %q: %v", route, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("route %q = %d, want 400", route, resp.StatusCode)
		}
	}
}

func TestPostRoutesThroughTheQueryParameter(t *testing.T) {
	client, _, _ := newUI(t)

	// A form posts to "?p=destinations/add"; without a token the service
	// must still be the thing that refuses it.
	resp, err := client.PostForm("http://ui/?p=destinations/add", map[string][]string{
		"name": {"sneaky"}, "type": {"local"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST ?p=destinations/add = %d, want 403", resp.StatusCode)
	}
}
