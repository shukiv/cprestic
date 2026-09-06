package webui_test

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/nodestore"
)

// post submits a form and insists it was accepted, which for this
// interface means a redirect back to the page that carried it.
func post(t *testing.T, client *http.Client, path string, form map[string][]string) {
	t.Helper()
	resp, err := client.PostForm("http://ui"+path, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s: %d %s", path, resp.StatusCode, body)
	}
}

// journalFixture is a service log with one line at each level and two
// accounts in it, which is enough to tell every filter apart.
const journalFixture = `2026-09-07T01:14:29+03:00 level=DEBUG msg="restic argv" account=thelittleprinced
2026-09-07T01:14:30+03:00 level=INFO msg="backup started" account=thelittleprinced
2026-09-07T01:14:30+03:00 level=WARN msg="staging is tight" account=studio
2026-09-07T01:14:30+03:00 level=ERROR msg="stage payload" account=thelittleprinced error="cpanel: mysqldump thelittleprinced_fwpl1: exit status 2"
2026-09-07T01:14:31+03:00 level=INFO msg="backup finished" account=studio status=success
2026-09-07T01:14:32+03:00 level=INFO msg="backup finished" account=studio2 status=success
`

// TestTheServiceLogIsOnThePage is the whole point of the tab: the lines the
// service wrote, on the page, without an operator needing a shell.
func TestTheServiceLogIsOnThePage(t *testing.T) {
	client, _, _ := newUIWithJournal(t, journalFixture)
	_, page := get(t, client, "/logs?tab=service")
	for _, want := range []string{"stage payload", "backup finished", "restic argv"} {
		if !strings.Contains(page, want) {
			t.Errorf("the service log does not carry %q", want)
		}
	}
}

// TestTheServiceLogNarrowsToWhatWasAsked covers the two filters that make a
// long log readable: a level, and one account.
func TestTheServiceLogNarrowsToWhatWasAsked(t *testing.T) {
	client, _, _ := newUIWithJournal(t, journalFixture)

	_, warn := get(t, client, "/logs?tab=service&level=warn")
	if !strings.Contains(warn, "stage payload") || !strings.Contains(warn, "staging is tight") {
		t.Error("a warning filter dropped a warning or an error")
	}
	if strings.Contains(warn, "backup finished") || strings.Contains(warn, "restic argv") {
		t.Error("a warning filter kept lines below it")
	}

	_, account := get(t, client, "/logs?tab=service&account=studio")
	if !strings.Contains(account, "staging is tight") || strings.Contains(account, "stage payload") {
		t.Error("an account filter did not narrow to that account")
	}
	// An account whose name starts another account's name is a different
	// account, and its lines are not this one's.
	if strings.Contains(account, "account=studio2") {
		t.Error("an account filter kept a longer name that starts the same way")
	}
}

// TestFollowingTheLogMakesItRefreshItself reuses what a running backup
// already does: the page marks itself live, and the script that keeps a
// running page current swaps the region.
func TestFollowingTheLogMakesItRefreshItself(t *testing.T) {
	client, _, _ := newUIWithJournal(t, journalFixture)

	// The marker, not the word: the script that watches for it is inlined
	// into every page, so the word is on all of them.
	const marker = `<span data-running="1" hidden></span>`
	_, still := get(t, client, "/logs?tab=service")
	if strings.Contains(still, marker) {
		t.Error("a log nobody asked to follow refreshes anyway")
	}
	if !strings.Contains(still, `data-live="servicelog"`) {
		t.Error("the log is not a region the refresh can swap")
	}
	_, following := get(t, client, "/logs?tab=service&follow=1")
	if !strings.Contains(following, marker) {
		t.Error("following the log does not keep it current")
	}
}

// TestTheWholeLogComesBackAsAFile is what "past full logs" needs: the tail
// on the page is bounded, so the whole thing has to be collectable.
func TestTheWholeLogComesBackAsAFile(t *testing.T) {
	client, _, _ := newUIWithJournal(t, journalFixture)
	resp, err := client.Get("http://ui/logs/download")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download: %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, "gniza-log-") {
		t.Errorf("the log does not come back as a file: %q", got)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("the log came back as %q", got)
	}
}

// TestChangingTheLogLevelTakesEffectWithoutARestart is why the setting is
// worth having: an operator turns debug on to watch something that is
// happening now, and a service that had to be restarted first would have
// stopped doing it.
func TestChangingTheLogLevelTakesEffectWithoutARestart(t *testing.T) {
	client, _, engine := newUI(t)
	level := engine.LogLevel()
	if level == nil {
		t.Fatal("the engine does not carry a log level")
	}
	if level.Level() != slog.LevelInfo {
		t.Fatalf("a new server starts at %s", level.Level())
	}

	_, page := get(t, client, "/settings")
	form := map[string][]string{"csrf": {csrfToken(t, page)}, "log_level": {"debug"}}
	post(t, client, "/settings/save", form)

	if level.Level() != slog.LevelDebug {
		t.Fatalf("the running service is still at %s", level.Level())
	}
	settings, err := engine.Store().Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.LogLevel != "debug" {
		t.Fatalf("the level was not stored: %q", settings.LogLevel)
	}

	// Something unreadable must leave the service where it is rather than
	// silently turn the log off.
	form["log_level"] = []string{"chatty"}
	post(t, client, "/settings/save", form)
	if level.Level() != slog.LevelDebug {
		t.Fatalf("an unreadable level moved the service to %s", level.Level())
	}
}

// TestTheSettingsPageOffersEveryLevel keeps the page and the parser in step.
func TestTheSettingsPageOffersEveryLevel(t *testing.T) {
	client, _, _ := newUI(t)
	_, page := get(t, client, "/settings")
	for _, want := range nodestore.LogLevels {
		if !strings.Contains(page, `value="`+want+`"`) {
			t.Errorf("the settings page does not offer %q", want)
		}
	}
}
