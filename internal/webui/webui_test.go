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
		if !strings.Contains(body, "cprest backups") {
			t.Errorf("GET %s did not render the layout", path)
		}
		// A template that fails halfway must not leave a partial page.
		if !strings.Contains(body, "</html>") {
			t.Errorf("GET %s produced a truncated page", path)
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
	if !strings.Contains(page, "generates its own SSH key") {
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
