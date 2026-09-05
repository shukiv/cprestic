package webui

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/shuki/cprest/internal/agent"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/update"
)

// updatePanel is the settings card about the program itself: what is
// running here, what has been released, and how installing it went.
type updatePanel struct {
	// Running is this build, and Latest is the newest release the daily
	// check found, whether or not it is newer than this one.
	Running    string
	Latest     string
	ReleaseURL string
	// Checked is how long ago the last ask was, in words.
	Checked string
	// Newer says a release later than this build has been published, and
	// so whether there is anything to install.
	Newer bool
	// Installing says an upgrade is in flight now. The card refreshes
	// itself while it is, including across the restart that finishes it.
	Installing bool
	Upgrade    nodestore.UpgradeState
}

// Stage is what is happening now, in words.
func (p updatePanel) Stage() string {
	switch p.Upgrade.Stage {
	case "downloading":
		return "Downloading " + p.Upgrade.Version + " and checking its signature"
	case "installing":
		return "Installing " + p.Upgrade.Version + "; the service restarts on its way through"
	default:
		return "Installing " + p.Upgrade.Version
	}
}

// Installed says the last upgrade finished and this is the build it
// installed, which is what makes the card say so rather than showing an
// old result for ever.
func (p updatePanel) Installed() bool {
	return !p.Upgrade.FinishedAt.IsZero() && !p.Upgrade.Failed &&
		p.Upgrade.Version == p.Running
}

// Failed says the last upgrade did not finish.
func (p updatePanel) Failed() bool {
	return !p.Upgrade.FinishedAt.IsZero() && p.Upgrade.Failed
}

// handleCheckUpdate asks GitHub now rather than waiting for the daily ask.
//
// Somebody who has read that a release exists should not have to wait a
// day for this server to agree, and an operator whose check has been
// failing needs a way to see the reason change.
func (s *Server) handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	state, err := s.engine.CheckForUpdateNow(ctx)
	switch {
	case err != nil:
		s.redirect(w, r, "/settings", "error", "Could not ask about newer versions: "+err.Error())
	case state.Version == "":
		s.redirect(w, r, "/settings", "warn", "GitHub answered, but named no release.")
	case update.Newer(agent.Version, state.Version):
		s.redirect(w, r, "/settings", "ok", "cP:Restic "+state.Version+" has been released.")
	default:
		s.redirect(w, r, "/settings", "ok",
			"This server runs "+agent.Version+", and the newest release is "+state.Version+".")
	}
}

// handleUpgrade installs a released version over this one, once it has
// been agreed to.
//
// The confirmation is not ceremony. This replaces the program on the
// server and restarts it, which fails whatever backup is running; and it
// is the one action here that fetches something from the internet and runs
// it as root, so it is worth somebody deciding to do rather than
// discovering they have.
func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	version := r.PostFormValue("version")
	if !update.IsRelease(version) {
		// Nothing that is not a release version reaches the engine, and
		// nothing reaches a page either: this is what the button carries,
		// so an empty one is a request that did not come from it.
		s.redirect(w, r, "/settings", "error", "That is not a released version of cP:Restic.")
		return
	}
	if !confirmed(r) {
		s.askFirst(w, r, confirmation{
			Title:   "Install cP:Restic " + version + "?",
			Section: "settings",
			Warning: fmt.Sprintf(
				"This downloads %s, checks that the cP:Restic release key signed it, "+
					"installs it over %s and restarts the service. Backups already queued "+
					"stay queued and run afterwards.", version, agent.Version),
			Detail: []string{
				"The program on this server is replaced; destinations, schedules and history are not touched.",
				"The service restarts, so the interface is unavailable for a few seconds.",
				"Nothing installs unless the release key signed it: an unsigned release stops here.",
			},
			Tick:   "Yes, install " + version,
			Button: "Install " + version,
			Cancel: linkTo("/settings"),
		})
		return
	}
	if err := s.engine.StartUpgrade(version); err != nil {
		s.redirect(w, r, "/settings", "error", "Could not start the upgrade: "+err.Error())
		return
	}
	s.redirect(w, r, "/settings", "ok",
		"Installing "+version+". This page says how it goes; the service restarts on its way through.")
}
