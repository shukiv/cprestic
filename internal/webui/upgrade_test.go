package webui_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/agent"
	"github.com/shuki/cprest/internal/nodestore"
)

// TestSettingsOffersTheRelease covers what the operator sees: the version
// running, the release published, and a button that asks before it does
// anything.
func TestSettingsOffersTheRelease(t *testing.T) {
	client, _, engine := newUI(t)

	was := agent.Version
	agent.Version = "v1.2.3"
	t.Cleanup(func() { agent.Version = was })

	if err := engine.Store().SaveUpdateState(nodestore.UpdateState{
		CheckedAt: time.Now().UTC(), Version: "v1.3.0",
		URL: "https://github.com/shukiv/cprestic/releases/tag/v1.3.0",
	}); err != nil {
		t.Fatal(err)
	}

	status, page := get(t, client, "/?p=settings")
	if status != http.StatusOK {
		t.Fatalf("settings answered %d", status)
	}
	for _, want := range []string{
		"v1.2.3", "cP:Restic v1.3.0 has been released",
		"Install v1.3.0", "?p=settings/update/install", "?p=settings/update/check",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the settings page does not say %q", want)
		}
	}

	// Pressing it asks first, and asking installs nothing.
	token := csrfToken(t, page)
	form := url.Values{"csrf": {token}, "version": {"v1.3.0"}}
	resp, err := client.PostForm("http://ui/?p=settings/update/install", form)
	if err != nil {
		t.Fatal(err)
	}
	asked := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the confirmation answered %d: %s", resp.StatusCode, asked)
	}
	for _, want := range []string{"Install cP:Restic v1.3.0?", "Yes, install v1.3.0", "restarts the service"} {
		if !strings.Contains(asked, want) {
			t.Errorf("the confirmation does not say %q", want)
		}
	}
	state, err := engine.Store().UpgradeState()
	if err != nil {
		t.Fatal(err)
	}
	if !state.StartedAt.IsZero() {
		t.Errorf("asking started an upgrade: %+v", state)
	}
}

// TestUpgradeRefusesAVersionNobodyWasToldAbout is the guard that matters
// if the button is ever not where the request came from.
func TestUpgradeRefusesAVersionNobodyWasToldAbout(t *testing.T) {
	client, _, engine := newUI(t)

	was := agent.Version
	agent.Version = "v1.2.3"
	t.Cleanup(func() { agent.Version = was })

	if err := engine.Store().SaveUpdateState(nodestore.UpdateState{
		CheckedAt: time.Now().UTC(), Version: "v1.3.0",
	}); err != nil {
		t.Fatal(err)
	}
	_, page := get(t, client, "/?p=settings")
	token := csrfToken(t, page)

	for _, version := range []string{"v9.9.9", "v1.2.3", "not-a-version", ""} {
		resp, err := client.PostForm("http://ui/?p=settings/update/install",
			url.Values{"csrf": {token}, "version": {version}, "confirm": {"1"}})
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("%q answered %d, want a redirect with the reason: %s", version, resp.StatusCode, body)
		}
	}
	state, err := engine.Store().UpgradeState()
	if err != nil {
		t.Fatal(err)
	}
	if !state.StartedAt.IsZero() {
		t.Errorf("an upgrade was started: %+v", state)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	body := make([]byte, 0, 64<<10)
	buffer := make([]byte, 32<<10)
	for {
		read, err := resp.Body.Read(buffer)
		body = append(body, buffer[:read]...)
		if err != nil {
			break
		}
	}
	return string(body)
}

// TestUninstallAsksFirst: the control removes the interface it is on, so
// what it must not do is remove it on one click.
func TestUninstallAsksFirst(t *testing.T) {
	client, _, _ := newUI(t)

	status, page := get(t, client, "/?p=settings")
	if status != http.StatusOK {
		t.Fatalf("settings answered %d", status)
	}
	for _, want := range []string{
		"Remove cP:Restic from this server", "?p=settings/uninstall",
		"sh /usr/local/share/cprest/uninstall.sh",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the settings page does not offer %q", want)
		}
	}

	resp, err := client.PostForm("http://ui/?p=settings/uninstall",
		url.Values{"csrf": {csrfToken(t, page)}})
	if err != nil {
		t.Fatal(err)
	}
	asked := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the confirmation answered %d: %s", resp.StatusCode, asked)
	}
	for _, want := range []string{
		"Remove cP:Restic from this server?", "Yes, remove cP:Restic from this server",
		"This page goes with it", "are not touched",
	} {
		if !strings.Contains(asked, want) {
			t.Errorf("the confirmation does not say %q", want)
		}
	}
	// Nothing ran: there is no uninstaller on a test machine, and the
	// confirmation is not the thing that would have run it anyway.
	if status, _ := get(t, client, "/?p=settings"); status != http.StatusOK {
		t.Errorf("the interface stopped answering: %d", status)
	}
}

// TestDismissClearsTheLastUpgrade: a failed upgrade stays on the page
// until somebody has read it, and then has to be able to go.
func TestDismissClearsTheLastUpgrade(t *testing.T) {
	client, _, engine := newUI(t)

	if err := engine.Store().SaveUpgradeState(nodestore.UpgradeState{
		Version: "v1.3.0", From: "v1.2.3",
		StartedAt: time.Now().UTC().Add(-time.Hour), FinishedAt: time.Now().UTC().Add(-time.Hour),
		Failed: true, Error: "the installer stopped with status 1",
	}); err != nil {
		t.Fatal(err)
	}
	_, page := get(t, client, "/?p=settings")
	if !strings.Contains(page, "v1.3.0 was not installed") {
		t.Fatal("the settings page does not say the upgrade failed")
	}

	resp, err := client.PostForm("http://ui/?p=settings/update/dismiss",
		url.Values{"csrf": {csrfToken(t, page)}})
	if err != nil {
		t.Fatal(err)
	}
	_ = readBody(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("dismissing answered %d", resp.StatusCode)
	}
	_, page = get(t, client, "/?p=settings")
	if strings.Contains(page, "was not installed") {
		t.Error("the failed upgrade is still on the page")
	}
	state, err := engine.Store().UpgradeState()
	if err != nil {
		t.Fatal(err)
	}
	if !state.StartedAt.IsZero() {
		t.Errorf("it is still stored: %+v", state)
	}
}
