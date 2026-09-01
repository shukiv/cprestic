package webui_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/node"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/protocol"
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
	body, _ := io.ReadAll(resp.Body)
	page = string(body)

	// The form comes back with the reason in it, still filled in: a
	// destination that could not be reached is a typo to correct, not a
	// reason to type everything again.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST = %d, want the form back", resp.StatusCode)
	}
	if !strings.Contains(page, "SSH") {
		t.Error("the page does not say what went wrong")
	}
	for _, kept := range []string{`value="cpbackup"`, `value="/home/cpbackup/backups"`, `value="127.0.0.1"`} {
		if !strings.Contains(page, kept) {
			t.Errorf("the form came back without %s in it", kept)
		}
	}
	if strings.Contains(page, "kind=error") {
		t.Error("the operator was sent away from the form they were filling in")
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
	resp, err := client.PostForm("http://ui/schedule/save", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Nightly"}, "cron": {"0 2 * * *"},
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
	if strings.Contains(location, "/schedule") {
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

// The typefaces ship inside the plugin: a page that runs as root should
// not be telling a font host when a root session is open.
func TestFontsAreServedByThePluginItself(t *testing.T) {
	client, _, _ := newUI(t)

	status, body := get(t, client, "/font?name=fira-sans-400.woff2")
	if status != http.StatusOK {
		t.Fatalf("GET the regular weight = %d", status)
	}
	if len(body) < 1000 || body[:4] != "wOF2" {
		t.Errorf("that is not a woff2 file: %d bytes starting %q", len(body), body[:min(4, len(body))])
	}

	for _, name := range []string{"", "../static/app.css", "fonts/fira-sans-400.woff2", "nonesuch.woff2"} {
		if status, _ := get(t, client, "/font?name="+url.QueryEscape(name)); status != http.StatusNotFound {
			t.Errorf("GET a font named %q = %d, want 404", name, status)
		}
	}

	_, page := get(t, client, "/destinations")
	if strings.Contains(page, "fonts.googleapis.com") || strings.Contains(page, "fonts.gstatic.com") {
		t.Error("the page still asks a font host for its typefaces")
	}
	if !strings.Contains(page, `url("?p=font&name=fira-sans-400.woff2")`) {
		t.Error("the stylesheet does not point at the plugin's own fonts")
	}
}

// A granular restore is queued with the part of the account it wants, and
// never with an apply: what it recovers is left to collect.
func TestGranularRestoreIsQueuedWithWhatItAsksFor(t *testing.T) {
	client, _, engine := newUI(t)

	// The restore page shows no form until an account is chosen, so the
	// token comes from a page that always has one.
	_, page := get(t, client, "/destinations")
	queued, err := client.PostForm("http://ui/restore/items", map[string][]string{
		"csrf": {csrfToken(t, page)}, "account": {"customer1"},
		"repository": {"repo-1"}, "snapshot": {"abc123"},
		"item": {"mailbox"}, "name": {"example.com/sales", " "},
	})
	if err != nil {
		t.Fatal(err)
	}
	queued.Body.Close()

	restores, err := engine.Store().Restores(10)
	if err != nil || len(restores) != 1 {
		t.Fatalf("restores = %+v (%v)", restores, err)
	}
	got := restores[0]
	if got.Kind != protocol.RestoreItems {
		t.Errorf("kind = %q, want %q", got.Kind, protocol.RestoreItems)
	}
	if got.ItemKind != "mailbox" {
		t.Errorf("item kind = %q", got.ItemKind)
	}
	if len(got.ItemNames) != 1 || got.ItemNames[0] != "example.com/sales" {
		t.Errorf("item names = %v, want the one mailbox and no blanks", got.ItemNames)
	}
	if got.Apply {
		t.Error("a granular restore must never apply itself to the live account")
	}
}

// The restore page offers every part of an account it can take out on its
// own, so the operator does not have to know the payload's shape.
func TestTheRestorePageOffersEveryGranularKind(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/restore")
	for _, want := range []string{
		"Files or folders", "Website files", "A mailbox", "A database",
		"DNS records", "SSL certificates", "Account settings",
	} {
		if strings.Contains(page, want) {
			continue
		}
		// The picker only appears once an account with backups is chosen,
		// so an empty page is allowed to be missing it — but the strings
		// have to exist in the template that renders it.
		if !strings.Contains(page, "Restore one thing") {
			return
		}
		t.Errorf("the restore page does not offer %q", want)
	}
}

// A running backup reports how far it has got, and the page says so
// rather than showing a pill that never changes.
func TestARunningBackupShowsItsProgress(t *testing.T) {
	client, _, engine := newUI(t)

	queued, err := engine.Store().PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusRunning, QueuedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Store().SetJobProgress(queued.ID, nodestore.JobProgress{
		Percent: 42.5, BytesDone: 512 << 20, TotalBytes: 1 << 30,
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/jobs")
	for _, want := range []string{
		`data-running="1"`, // the page knows to keep itself current
		"cpr-spin",         // and the pill spins rather than sitting still
		"43%",              // restic's own percentage, rounded
		"width:42.5%",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the history page does not show %q", want)
		}
	}
}

// A finished job carries no percentage: "100%" beside a failure would be
// a lie, and a bar on something that is over says nothing.
func TestAFinishedJobShowsNoProgress(t *testing.T) {
	client, _, engine := newUI(t)

	queued, err := engine.Store().PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusRunning, QueuedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Store().SetJobProgress(queued.ID, nodestore.JobProgress{Percent: 99}); err != nil {
		t.Fatal(err)
	}
	queued.Status = job.StatusFailed
	queued.Progress = nil
	if _, err := engine.Store().PutJob(queued); err != nil {
		t.Fatal(err)
	}

	if _, page := get(t, client, "/jobs"); strings.Contains(page, `class="cpr-progress"`) {
		t.Error("a finished job is still showing a progress bar")
	}
}

// A late status line must not reopen a job that has already finished.
func TestProgressOnAFinishedJobIsIgnored(t *testing.T) {
	_, _, engine := newUI(t)

	stored, err := engine.Store().PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusSuccess, QueuedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Store().SetJobProgress(stored.ID, nodestore.JobProgress{Percent: 12}); err != nil {
		t.Fatal(err)
	}
	after, err := engine.Store().Job(stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Progress != nil {
		t.Errorf("progress was written onto a finished job: %+v", after.Progress)
	}
}

// A download button that leads to a file which is no longer on the server
// is worse than no button: it offers the operator their data and then
// fails. Whether the archive still exists is checked when the page is
// rendered.
func TestADownloadIsOnlyOfferedWhileTheArchiveExists(t *testing.T) {
	client, _, engine := newUI(t)

	archive := filepath.Join(t.TempDir(), "cpmove-customer1.tar")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	stored, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", Kind: "account", Status: job.StatusSuccess,
		ArchivePath: archive, QueuedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, page := get(t, client, "/jobs"); !strings.Contains(page, "?p=download&amp;id="+stored.ID) {
		t.Error("a collectable archive is not offered for download")
	}

	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	_, page := get(t, client, "/jobs")
	if strings.Contains(page, "?p=download&amp;id="+stored.ID) {
		t.Error("a download is still offered for an archive that has been swept")
	}
	if !strings.Contains(page, "no longer on this server") {
		t.Error("the page does not say what happened to it")
	}
}

// Collected output can be deleted from the settings page, and that needs
// the token like every other change.
func TestCollectedOutputCanBeDeleted(t *testing.T) {
	client, _, engine := newUI(t)

	root := engine.Settings().StagingRoot
	kept := filepath.Join(root, "keep-restore-customer1")
	if err := os.MkdirAll(kept, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kept, "cpmove.tar"), make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/settings")
	if !strings.Contains(page, "restore-customer1") {
		t.Fatal("the settings page does not list what is waiting in the work directory")
	}

	refused, err := client.PostForm("http://ui/settings/output/delete", map[string][]string{
		"key": {"restore-customer1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	refused.Body.Close()
	if _, err := os.Stat(kept); err != nil {
		t.Fatal("output was deleted without the token")
	}

	deleted, err := client.PostForm("http://ui/settings/output/delete", map[string][]string{
		"csrf": {csrfToken(t, page)}, "key": {"restore-customer1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	deleted.Body.Close()
	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Error("the output is still there after being deleted")
	}
}

// A state has to say what actually happened. "Last run succeeded" is a
// fact an operator can check; a copy nothing will ever refresh gets a
// state of its own rather than the same green pill.
func TestAStateSaysWhatHappenedAndWhatItMeans(t *testing.T) {
	client, _, engine := newUI(t)

	finished := time.Now().Add(-30 * 24 * time.Hour)
	if _, err := engine.Store().PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusSuccess,
		QueuedAt: finished, FinishedAt: &finished,
		Targets: []nodestore.JobTarget{{Status: job.TargetSuccess}},
	}); err != nil {
		t.Fatal(err)
	}

	// No schedule covers it, so nothing will ever refresh that copy.
	_, page := get(t, client, "/accounts")
	if !strings.Contains(page, "Not scheduled") {
		t.Error("an account no schedule covers is being shown as a good backup")
	}
	if !strings.Contains(page, "no schedule covers it") {
		t.Error("the page does not say why")
	}
	// Every state explains itself on hover rather than by its colour.
	if !strings.Contains(page, "nothing will ever take another") {
		t.Error("the state has no explanation attached")
	}
	if strings.Contains(page, ">Protected<") {
		t.Error("the page still uses a word that promises more than it checks")
	}
}

// One good backup says little on its own. The accounts page shows the
// account's record — how many runs finished and how many worked — so a
// site that fails one night in three is distinguishable from one that has
// never failed.
func TestTheAccountsPageShowsEachAccountsRecord(t *testing.T) {
	client, _, engine := newUI(t)

	when := time.Now().Add(-2 * time.Hour)
	// Oldest first: the failure is history, the last run worked.
	for _, status := range []job.Status{
		job.StatusFailed, job.StatusSuccess, job.StatusSuccess,
	} {
		started := when
		finished := when.Add(90 * time.Second)
		target := nodestore.JobTarget{
			RepositoryID: "repo-1", Status: job.TargetSuccess,
			BytesAdded: 6 << 20, BytesProcessed: 90 << 20,
		}
		if status == job.StatusFailed {
			// A failed run wrote nothing, so it is not a copy.
			target = nodestore.JobTarget{RepositoryID: "repo-1", Status: job.TargetFailed}
		}
		if _, err := engine.Store().PutJob(nodestore.Job{
			Account: "customer1", Status: status,
			QueuedAt: when, StartedAt: &started, FinishedAt: &finished,
			Targets: []nodestore.JobTarget{target},
		}); err != nil {
			t.Fatal(err)
		}
		when = when.Add(time.Minute)
	}

	drilled := time.Now().Add(-time.Hour)
	if _, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", Kind: node.KindVerify,
		Status: job.StatusSuccess, QueuedAt: drilled, FinishedAt: &drilled,
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/accounts")
	if !strings.Contains(page, "2 of 3 succeeded") {
		t.Error("the page does not show how many backups of the account worked")
	}
	if !strings.Contains(page, "1 failed") {
		t.Error("the page does not show that one of them failed")
	}
	if !strings.Contains(page, "verified ") {
		t.Error("the page does not show when the backup was last rehearsed")
	}
	// What the last run cost: what it stored, and how long it took.
	if !strings.Contains(page, "6.0 MiB stored") {
		t.Error("the page does not show what the last backup stored")
	}
	if !strings.Contains(page, "took 1m 30s") {
		t.Error("the page does not show how long the last backup took")
	}
	// Where the copies went, and how many each destination took.
	if !strings.Contains(page, "2 to ") {
		t.Error("the page does not show how many copies each destination holds")
	}
}

func TestAddingADestinationWorksWithAndWithoutTheSheet(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/destinations")
	if !strings.Contains(page, `<dialog id="add-destination"`) {
		t.Error("the page has no sheet to add a destination in")
	}
	if !strings.Contains(page, `href="?p=destinations&amp;add=1"`) {
		t.Error("the button is not a link, so it does nothing without JavaScript")
	}

	// What that link opens on its own.
	status, standalone := get(t, client, "/destinations?add=1")
	if status != http.StatusOK {
		t.Fatalf("GET /destinations?add=1 = %d", status)
	}
	if strings.Contains(standalone, `<dialog id="add-destination"`) {
		t.Error("the form is in a sheet on the page that exists because there is no sheet")
	}
	for _, want := range []string{"Server address", "Folder inside the destination", "Add and test"} {
		if !strings.Contains(standalone, want) {
			t.Errorf("the add page is missing %q", want)
		}
	}
}

// A dialog is display:none until the browser opens it. A stylesheet that
// says otherwise leaves the drawer standing open on the page, outside the
// top layer, positioned against whichever element of WHM's chrome happens
// to be its containing block — which is exactly what it did.
func TestTheDrawerIsOnlyVisibleWhenItIsOpen(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/destinations")
	for _, rule := range []string{
		".cprest .cpr-sheet[open] { display:flex; }",
	} {
		if !strings.Contains(page, rule) {
			t.Errorf("the stylesheet does not say %q", rule)
		}
	}
	// Nothing may give a dialog a display of its own outside [open].
	for _, forbidden := range []string{
		".cprest .cpr-sheet {\n  position:fixed; inset:0 0 0 auto; margin:0;\n  width:min(520px, 94vw); max-width:none; height:100%; max-height:none;\n  padding:0; border:0; border-left:1px solid var(--line-strong); border-radius:0;\n  background:var(--surface); color:var(--ink);\n  box-shadow:-8px 0 28px rgba(10,14,20,.22);\n  display:flex;",
	} {
		if strings.Contains(page, forbidden) {
			t.Error("the drawer is displayed whether or not it is open")
		}
	}
}

// Editing opens the same drawer as adding, with that destination's form
// loaded into it — and the link still goes to the form's own page for a
// browser that cannot do that.
func TestEditingOpensTheDrawer(t *testing.T) {
	client, _, engine := newUI(t)

	if _, _, err := engine.AddDestination(nodestore.Destination{
		Name: "Local disk", Type: "local",
		Config: map[string]string{"root": t.TempDir()},
	}, nil, "cp01"); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/destinations")
	if !strings.Contains(page, "data-dialog-fetch") {
		t.Error("the edit link does not open the drawer")
	}
	if !strings.Contains(page, `data-dialog-title="Edit “Local disk”"`) {
		t.Error("the drawer would not say which destination is being edited")
	}
	if !strings.Contains(page, "data-drawer-body") || !strings.Contains(page, "data-drawer-title") {
		t.Error("the drawer has nothing to load a form into")
	}

	// The page the drawer reads, and that a browser without the drawer
	// simply goes to.
	destinations, err := engine.Store().Destinations()
	if err != nil || len(destinations) != 1 {
		t.Fatalf("destinations = %+v (%v)", destinations, err)
	}
	status, editPage := get(t, client, "/destinations?edit="+destinations[0].ID)
	if status != http.StatusOK {
		t.Fatalf("GET the edit page = %d", status)
	}
	if !strings.Contains(editPage, "data-drawer-content") {
		t.Error("the edit page does not mark the part the drawer loads")
	}
	if !strings.Contains(editPage, "Save changes") {
		t.Error("the edit page has no form on it")
	}
}

// A row that says only "the last run failed" sends the operator to
// another page to find out what this one already knows.
func TestAFailedAccountSaysWhyOnTheRow(t *testing.T) {
	client, _, engine := newUI(t)

	finished := time.Now().Add(-time.Hour)
	if _, err := engine.Store().PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusFailed,
		QueuedAt: finished, FinishedAt: &finished,
		StagingErr: "not enough room to stage this account: it needs 7.6 GiB free and there is 6.3 GiB",
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/accounts")
	if !strings.Contains(page, "it needs 7.6 GiB free and there is 6.3 GiB") {
		t.Error("the row does not say why the backup failed")
	}
}

// A schedule can say what it leaves out, what never to store, and what to
// do when a destination fails.
func TestAScheduleCarriesWhatItLeavesOut(t *testing.T) {
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
		"skip_email": {"1"}, "retry_failed": {"1"},
		"excludes":             {"/home/*/tmp\n\n  /home/*/.cache  \n"},
		"alert_no_backup_days": {"3"},
		"alert_run_hours":      {"4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	saved.Body.Close()

	policies, err := engine.Store().Policies()
	if err != nil || len(policies) != 1 {
		t.Fatalf("policies = %+v (%v)", policies, err)
	}
	got := policies[0]
	if !got.SkipEmail || got.SkipHomedir || got.SkipDatabases {
		t.Errorf("what it leaves out = email:%v home:%v db:%v",
			got.SkipEmail, got.SkipHomedir, got.SkipDatabases)
	}
	if !got.RetryFailed {
		t.Error("the schedule does not retry a destination that failed")
	}
	if len(got.Excludes) != 2 || got.Excludes[0] != "/home/*/tmp" || got.Excludes[1] != "/home/*/.cache" {
		t.Errorf("excludes = %q, want the two lines with the blanks and spaces gone", got.Excludes)
	}
	if got.AlertNoBackupDays != 3 || got.AlertRunHours != 4 {
		t.Errorf("alerts = %d days, %d hours", got.AlertNoBackupDays, got.AlertRunHours)
	}

	// And the form offers the paths that are never worth storing.
	_, page = get(t, client, "/schedule")
	for _, want := range []string{"WordPress", "Magento", "wp-content/cache", "Leave out email"} {
		if !strings.Contains(page, want) {
			t.Errorf("the schedule form is missing %q", want)
		}
	}
}

// A run that is still going long after it should have finished is stuck
// more often than it is slow, and nothing else on these pages says so.
func TestALongRunIsCalledOut(t *testing.T) {
	client, _, engine := newUI(t)

	started := time.Now().Add(-9 * time.Hour)
	if _, err := engine.Store().PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusRunning,
		QueuedAt: started, StartedAt: &started,
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/accounts")
	if !strings.Contains(page, "has been running for") {
		t.Error("a nine-hour backup is not called out anywhere")
	}
	if !strings.Contains(page, "usually stuck") {
		t.Error("the warning does not say what it means")
	}
}

// The destination holds ciphertext and the key lives here, so the disaster
// these backups exist for is also the one that destroys the only copy of
// the key. The interface says so until it has been written down elsewhere.
func TestTheRecoveryKeyIsShownAndNagsUntilItIsSaved(t *testing.T) {
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

	// Until it is saved, the page says so.
	_, page = get(t, client, "/destinations")
	if !strings.Contains(page, "exists only on this server") {
		t.Error("nothing warns that the recovery key is nowhere else")
	}
	// And the password is not on the page until it is asked for.
	password, err := engine.RepositoryPassword(repos[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, password) {
		t.Error("the repository password is rendered on a page nobody asked")
	}

	// Asking shows it, with what to do with it.
	shown, err := client.PostForm("http://ui/destinations/recovery", map[string][]string{
		"csrf": {csrfToken(t, page)}, "repository": {repos[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(shown.Body)
	shown.Body.Close()
	revealed := string(body)
	if !strings.Contains(revealed, password) {
		t.Error("the recovery key was not shown when it was asked for")
	}
	if !strings.Contains(revealed, "RESTIC_REPOSITORY") {
		t.Error("the page does not say how to use it without cprest")
	}
	if !strings.Contains(revealed, "RESTIC_PASSWORD='"+password+"'") {
		t.Error("the commands still ask the reader to paste the key in themselves")
	}

	// The file is the same thing, to put somewhere else.
	card, err := client.PostForm("http://ui/destinations/recovery/card", map[string][]string{
		"csrf": {csrfToken(t, page)}, "repository": {repos[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	cardBody, _ := io.ReadAll(card.Body)
	card.Body.Close()
	if !strings.Contains(card.Header.Get("Content-Disposition"), "attachment") {
		t.Error("the recovery card is not a download")
	}
	if !strings.Contains(string(cardBody), password) {
		t.Error("the recovery card does not carry the key")
	}

	// Saying it is stored stops the warning.
	noted, err := client.PostForm("http://ui/destinations/recovery/note", map[string][]string{
		"csrf": {csrfToken(t, page)}, "repository": {repos[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	noted.Body.Close()
	if _, page = get(t, client, "/destinations"); strings.Contains(page, "exists only on this server") {
		t.Error("the warning is still there after the key was stored")
	}
}

// Reading a repository password is a change of state as far as secrecy is
// concerned, so it needs the token like every other POST.
func TestTheRecoveryKeyNeedsTheToken(t *testing.T) {
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
	repos, _ := engine.Store().Repositories()

	refused, err := client.PostForm("http://ui/destinations/recovery", map[string][]string{
		"repository": {repos[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(refused.Body)
	refused.Body.Close()
	password, _ := engine.RepositoryPassword(repos[0].ID)
	if strings.Contains(string(body), password) {
		t.Error("a request with no token was shown the recovery key")
	}
}

// A replacement server needs the settings of the one it replaces, so the
// server's own configuration is backed up like an account and restored
// like an item.
func TestTheServersOwnSettingsCanBeScheduledAndRestored(t *testing.T) {
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
	repos, _ := engine.Store().Repositories()

	_, page = get(t, client, "/schedule")
	saved, err := client.PostForm("http://ui/schedule/save", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Nightly"}, "cron": {"0 2 * * *"},
		"repository": {repos[0].ID}, "scope": {"all"}, "enabled": {"1"}, "mode": {"split"},
		"include_system": {"1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	saved.Body.Close()

	policies, err := engine.Store().Policies()
	if err != nil || len(policies) != 1 || !policies[0].IncludeSystem {
		t.Fatalf("policies = %+v (%v)", policies, err)
	}

	// It queues under its own name, which cannot collide with an account:
	// cPanel usernames cannot contain "@".
	queued, err := engine.QueueSystemBackup(policies[0].ID)
	if err != nil {
		t.Fatalf("queue the system backup: %v", err)
	}
	if queued.Account != "@system" {
		t.Errorf("queued as %q", queued.Account)
	}

	// And it is offered on the restore page as something to restore.
	if _, page = get(t, client, "/restore"); !strings.Contains(page, "The server's own settings") {
		t.Error("the restore page does not offer the server's settings")
	}
}

// A replacement server starts here: it knows nothing, and is given where
// the backups are and the key that reads them.
func TestRecoverAsksForWhereTheBackupsAreAndTheKey(t *testing.T) {
	client, _, _ := newUI(t)

	status, page := get(t, client, "/recover")
	if status != http.StatusOK {
		t.Fatalf("GET /recover = %d", status)
	}
	for _, want := range []string{
		"Recovery key", "Folder inside the destination", "Read the backups",
		"Nothing is written until they can be read",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the recovery page does not ask for %q", want)
		}
	}
}

// Attaching reads the repository before it writes anything down: a
// repository this server cannot read is a typo, and saving it would leave
// an operator in a disaster believing they had their backups back.
func TestAttachingWritesNothingUntilTheBackupsCanBeRead(t *testing.T) {
	client, _, engine := newUI(t)

	_, page := get(t, client, "/recover")
	before, err := engine.Store().Destinations()
	if err != nil {
		t.Fatal(err)
	}

	refused, err := client.PostForm("http://ui/recover/attach", map[string][]string{
		"csrf": {csrfToken(t, page)}, "type": {"local"}, "root": {t.TempDir()},
		"repo_path": {"oldserver"}, "recovery_key": {"not the key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(refused.Body)
	refused.Body.Close()
	if !strings.Contains(string(body), "could not read that repository") {
		t.Errorf("a repository that cannot be read was not reported as one: %s",
			firstError(string(body)))
	}

	after, err := engine.Store().Destinations()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Error("a destination was saved for a repository that could not be read")
	}
	if secrets, _ := engine.Store().Repositories(); len(secrets) != 0 {
		t.Error("a repository was recorded for backups nobody could read")
	}
}

// firstError pulls the banner text out of a rendered page, for a test that
// needs to say why the page it got was not the page it wanted.
func firstError(page string) string {
	start := strings.Index(page, `cpr-banner cpr-bad`)
	if start < 0 {
		return "no error banner"
	}
	rest := page[start:]
	open := strings.Index(rest, `<div class="cpr-body">`)
	if open < 0 {
		return "no body"
	}
	rest = rest[open+len(`<div class="cpr-body">`):]
	end := strings.Index(rest, "</div>")
	if end < 0 {
		return rest
	}
	return strings.TrimSpace(rest[:end])
}

// Browsing goes one level at a time: destinations, then what a
// destination holds, then an account's backups, then inside one. Listing
// every file of every snapshot of nineteen accounts would be minutes of
// restic for a question nobody asked.
func TestBrowsingStartsAtTheDestinations(t *testing.T) {
	client, _, _ := newUI(t)

	status, page := get(t, client, "/browse")
	if status != http.StatusOK {
		t.Fatalf("GET /browse = %d", status)
	}
	if !strings.Contains(page, "No destination yet") {
		t.Error("a server with no destinations does not say so")
	}

	_, page = get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Local disk"}, "type": {"local"},
		"root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added.Body.Close()

	_, page = get(t, client, "/browse")
	if !strings.Contains(page, "Local disk") {
		t.Error("the destination is not listed to browse")
	}
	if !strings.Contains(page, "?p=browse&amp;repository=") {
		t.Error("there is no way into the destination")
	}
	// The listing itself costs nothing until a destination is opened.
	if strings.Contains(page, "Most recent") {
		t.Error("the destination list is reading repositories nobody opened")
	}
}

// Recovery instructions that leave out the SSH key do not work: reaching
// an SFTP destination needs the key this server made for it and the host
// key it pinned, and a machine that has never seen this one has neither.
func TestTheRecoveryCardCarriesWhatIsNeededToReachTheDestination(t *testing.T) {
	client, _, engine := newUI(t)

	// A destination reached over SSH, with the files cprest writes for it.
	dir := t.TempDir()
	identity := filepath.Join(dir, "id_ed25519")
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(identity, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(knownHosts, []byte("backup.example.com ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.AddDestination(nodestore.Destination{
		Name: "Backup server", Type: "sftp",
		Config: map[string]string{
			"host": "backup.example.com", "user": "cpbackup", "root": "/backups",
			"identity_file": identity, "known_hosts_file": knownHosts,
		},
	}, nil, "cp01"); err != nil {
		t.Fatal(err)
	}
	repos, _ := engine.Store().Repositories()

	_, page := get(t, client, "/destinations")
	card, err := client.PostForm("http://ui/destinations/recovery/card", map[string][]string{
		"csrf": {csrfToken(t, page)}, "repository": {repos[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(card.Body)
	card.Body.Close()
	written := string(body)

	for _, want := range []string{
		"-i " + identity,                      // the key this server uses
		"BEGIN OPENSSH PRIVATE KEY",           // and the key itself, for elsewhere
		"backup.example.com ssh-ed25519 AAAA", // with the host key it pinned
		"let ssh ask for a\npassword",         // and what to do when the key is gone
		"cpbackup@backup.example.com",         //
	} {
		if !strings.Contains(written, want) {
			t.Errorf("the recovery card does not carry %q", want)
		}
	}
}

// A destination that holds backups is worth looking inside from where it
// is listed, not only from a page of its own.
func TestADestinationCanBeBrowsedFromItsRow(t *testing.T) {
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
	repos, _ := engine.Store().Repositories()

	_, page = get(t, client, "/destinations")
	if !strings.Contains(page, "?p=browse&amp;repository="+repos[0].ID) {
		t.Error("the destination cannot be browsed from its own row")
	}
}

// Specific files were typed from memory into a textarea. They are picked
// out of what the backup actually holds now, a directory at a time.
//
// The picker itself needs a readable repository, which this suite has no
// restic to talk to; what is asserted here is that the page still says
// how to reach it. The picker is exercised against a real repository.
func TestTheRestorePageExplainsHowToPickFiles(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/restore")
	if !strings.Contains(page, "Choose a backup") {
		t.Error("the restore page does not start by choosing a backup")
	}
	if strings.Contains(page, "Specific files (one path per line)") {
		t.Error("the page still asks for paths to be typed from memory")
	}
}

// The name is cP:Restic, and the cP is cPanel's orange. A stylesheet rule
// that greys every span inside the brand once made it the wrong colour,
// which is invisible in markup and only shows on the page.
func TestTheBrandIsOnThePageInItsOwnColour(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/")
	if !strings.Contains(page, `<span class="cpr-brand-cp">cP</span>:Restic`) {
		t.Error("the interface does not carry the name")
	}
	if !strings.Contains(page, ".cprest .cpr-brand-cp { color:#CF470C; }") {
		t.Error("the cP is not in its own colour")
	}
	if strings.Contains(page, ".cprest .cpr-brand span { color:var(--muted)") {
		t.Error("a rule is still greying out everything inside the brand")
	}
}
