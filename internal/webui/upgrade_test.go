package webui_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/agent"
	"github.com/shukiv/gniza/internal/nodestore"
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
		URL: "https://github.com/shukiv/gniza/releases/tag/v1.3.0",
	}); err != nil {
		t.Fatal(err)
	}

	status, page := get(t, client, "/?p=settings&tab=version")
	if status != http.StatusOK {
		t.Fatalf("settings answered %d", status)
	}
	for _, want := range []string{
		"v1.2.3", "Gniza v1.3.0 has been released",
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
	for _, want := range []string{"Install Gniza v1.3.0?", "Yes, install v1.3.0", "restarts the service"} {
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
	_, page := get(t, client, "/?p=settings&tab=version")
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

	status, page := get(t, client, "/?p=settings&tab=version")
	if status != http.StatusOK {
		t.Fatalf("settings answered %d", status)
	}
	for _, want := range []string{
		"Remove Gniza from this server", "?p=settings/uninstall",
		"sh /usr/local/share/gniza/uninstall.sh",
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
		"Remove Gniza from this server?", "Yes, remove Gniza from this server",
		"This page goes with it", "are not touched",
	} {
		if !strings.Contains(asked, want) {
			t.Errorf("the confirmation does not say %q", want)
		}
	}
	// Nothing ran: there is no uninstaller on a test machine, and the
	// confirmation is not the thing that would have run it anyway.
	if status, _ := get(t, client, "/?p=settings&tab=version"); status != http.StatusOK {
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
	_, page := get(t, client, "/?p=settings&tab=version")
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
	_, page = get(t, client, "/?p=settings&tab=version")
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

// TestSettingsTabsShowOnePartEach: the page was seven cards long, and the
// two anybody reaches for in a hurry were the two furthest down. Each tab
// has to hold its own cards, and only its own.
func TestSettingsTabsShowOnePartEach(t *testing.T) {
	client, _, _ := newUI(t)

	tabs := map[string]struct{ here, notHere []string }{
		"": { // no tab named: the first one
			here:    []string{"How backups run", "What a backup contains", ">Backups<"},
			notHere: []string{"Remove Gniza from this server", "Where problems are reported"},
		},
		"&tab=alerts": {
			here:    []string{"Where problems are reported"},
			notHere: []string{"How backups run", "Remove Gniza from this server"},
		},
		"&tab=storage": {
			here:    []string{"Staging", "Restored files waiting to be collected"},
			notHere: []string{"How backups run", "Where problems are reported"},
		},
		"&tab=version": {
			here:    []string{"This copy of Gniza", "Remove Gniza from this server"},
			notHere: []string{"How backups run", "Where problems are reported"},
		},
	}
	for suffix, want := range tabs {
		status, page := get(t, client, "/?p=settings"+suffix)
		if status != http.StatusOK {
			t.Fatalf("%q answered %d", suffix, status)
		}
		for _, text := range want.here {
			if !strings.Contains(page, text) {
				t.Errorf("%q does not show %q", suffix, text)
			}
		}
		for _, text := range want.notHere {
			if strings.Contains(page, text) {
				t.Errorf("%q shows %q, which belongs to another tab", suffix, text)
			}
		}
		// Every tab carries the whole tab bar, or there is no way back.
		for _, link := range []string{"?p=settings&amp;tab=alerts", "?p=settings&amp;tab=storage",
			"?p=settings&amp;tab=version"} {
			if !strings.Contains(page, link) {
				t.Errorf("%q does not link to %q", suffix, link)
			}
		}
	}

	// A tab nobody has heard of is the first tab, not an error: this is a
	// view of one page.
	if status, page := get(t, client, "/?p=settings&tab=nonsense"); status != http.StatusOK ||
		!strings.Contains(page, "How backups run") {
		t.Errorf("an unknown tab answered %d", status)
	}

	// The links that open the channel drawer were written before the page
	// had tabs, and they have to land where the channels are.
	if _, page := get(t, client, "/?p=settings&addchannel=1"); !strings.Contains(page, "Add a channel") ||
		!strings.Contains(page, "Where problems are reported") {
		t.Error("adding a channel does not land on the alerts tab")
	}
}

// TestAHandInstalledBuildIsToldWhyItIsNotOffered covers the page an
// operator lands on when nothing will update.
//
// A build that is not exactly a tag is never replaced from here: it came
// from somebody who knows what is in it, and installing a release over it
// would discard their work. The card said only "Nothing newer has been
// published" -- while the banner across the top of every page named the
// release that had been. So the page contradicted itself, and the way out
// was written down nowhere.
func TestAHandInstalledBuildIsToldWhyItIsNotOffered(t *testing.T) {
	client, _, engine := newUI(t)

	was := agent.Version
	agent.Version = "v0.1.0-48-g97f43df"
	t.Cleanup(func() { agent.Version = was })

	if err := engine.Store().SaveUpdateState(nodestore.UpdateState{
		CheckedAt: time.Now().UTC(), Version: "v0.1.2",
		URL: "https://github.com/shukiv/gniza/releases/tag/v0.1.2",
	}); err != nil {
		t.Fatal(err)
	}

	status, page := get(t, client, "/?p=settings&tab=version")
	if status != http.StatusOK {
		t.Fatalf("settings answered %d", status)
	}
	if strings.Contains(page, "Nothing newer has been published") {
		t.Error("a hand-installed build is told nothing newer exists, while the " +
			"banner on the same page names the release that does")
	}
	for _, want := range []string{
		"did not come from a published",
		"v0.1.2",
		"releases/latest/download/get.sh",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the settings page does not say %q", want)
		}
	}
	// And it still does not offer to install anything.
	if strings.Contains(page, "Install v0.1.2") {
		t.Error("a release is offered over a build that was not published")
	}
}
