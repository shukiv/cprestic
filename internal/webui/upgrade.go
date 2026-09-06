package webui

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/shuki/cprest/internal/agent"
	"github.com/shuki/cprest/internal/node"
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
	// Newer says a build later than this one has been published, and so
	// whether there is anything to install.
	Newer bool
	// Unreleased says this build is not a published version -- a build
	// made from a working tree, or one copied onto the server by hand.
	// Nothing is ever offered to replace one, so the card has to say that
	// rather than let "nothing newer" stand for it.
	Unreleased bool
	// Channel is where updates are read from, and Dist says that is the
	// branch rather than the published releases.
	Channel string
	Dist    bool
	// BuiltAt is the commit the published build was made from, in words.
	// It is what orders two builds of a branch; a release has a version
	// number instead.
	BuiltAt string
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
		s.redirect(w, r, settingsTab("version"), "error", "Could not ask about newer versions: "+err.Error())
	case state.Version == "":
		s.redirect(w, r, settingsTab("version"), "warn", "GitHub answered, but named no release.")
	case s.engine.UpdateOffered(state):
		s.redirect(w, r, settingsTab("version"), "ok", "cP:Restic "+state.Version+" is available to install.")
	default:
		s.redirect(w, r, settingsTab("version"), "ok",
			"This server runs "+agent.Version+", and what is published is "+state.Version+".")
	}
}

// handleChooseChannel sets where this server reads updates from.
//
// It is a form of its own rather than a field in the settings form,
// because that form is the whole of the settings: a field missing from it
// is a setting being cleared, and this one lives on a different card.
func (s *Server) handleChooseChannel(w http.ResponseWriter, r *http.Request) {
	settings, err := s.engine.Store().Settings()
	if err != nil {
		s.redirect(w, r, settingsTab("version"), "error", "Could not read the settings: "+err.Error())
		return
	}
	chosen := update.ChannelReleases
	if update.Channel(r.PostFormValue("update_channel")) == update.ChannelDist {
		chosen = update.ChannelDist
	}
	if node.Channel(settings) == chosen {
		s.redirect(w, r, settingsTab("version"), "ok", "That is already where updates come from.")
		return
	}
	settings.UpdateChannel = string(chosen)
	if err := s.engine.Store().SaveSettings(settings); err != nil {
		s.redirect(w, r, settingsTab("version"), "error", "Could not save that: "+err.Error())
		return
	}
	// What was found on the old channel says nothing about the new one,
	// and leaving it there would offer a release to a server now
	// following the branch.
	if err := s.engine.Store().SaveUpdateState(nodestore.UpdateState{}); err != nil {
		s.log.Error("clear what the last check found", "error", err)
	}
	if chosen == update.ChannelDist {
		s.redirect(w, r, settingsTab("version"), "ok",
			"Updates now come from the dist branch. Press Check now to see what is on it.")
		return
	}
	s.redirect(w, r, settingsTab("version"), "ok",
		"Updates now come from published releases. Press Check now to see the newest.")
}

// handleUninstall removes cP:Restic from this server.
//
// The command in a root shell is what this ran before, and it still works;
// what it did not do was exist anywhere somebody looking at the interface
// would find it. An administrator deciding to remove something should not
// have to go and look up how.
//
// It asks first, and says the two things somebody about to press it needs
// to know: this page goes with it, and the backups do not.
func (s *Server) handleUninstall(w http.ResponseWriter, r *http.Request) {
	if !confirmed(r) {
		s.askFirst(w, r, confirmation{
			Title:   "Remove cP:Restic from this server?",
			Section: "settings",
			Warning: "This stops the service and unregisters the WHM plugin, the cPanel hooks " +
				"and the account tile. This page goes with it: nothing here will answer " +
				"afterwards, and what is left to run is the installer, in a root shell.",
			Detail: []string{
				"Backups already on their destinations are not touched, and neither is anything on this server outside cP:Restic.",
				"/etc/cprest/master.key and /var/lib/cprest/state.db stay, so reinstalling comes back with the same destinations, schedules and history.",
				"Nothing here will take backups afterwards: whatever this server was backing up stops being backed up.",
			},
			Tick:   "Yes, remove cP:Restic from this server",
			Button: "Remove cP:Restic",
			Cancel: linkTo("/settings"),
		})
		return
	}
	if err := s.engine.StartUninstall(); err != nil {
		s.redirect(w, r, settingsTab("version"), "error", "Could not remove it: "+err.Error())
		return
	}
	s.redirect(w, r, settingsTab("version"), "warn",
		"Removing cP:Restic. This page stops answering in a few seconds. "+
			"Reinstalling brings back the same destinations, schedules and history.")
}

// handleDismissUpgrade clears what the last upgrade left on the card.
//
// A failed upgrade stays on the page, because an operator who was not
// looking when it happened still needs to find out. Once they have read
// it, it is a line about something that is over, and the only thing that
// would ever have replaced it is another upgrade.
func (s *Server) handleDismissUpgrade(w http.ResponseWriter, r *http.Request) {
	state, err := s.engine.UpgradeStatus()
	if err != nil {
		s.redirect(w, r, settingsTab("version"), "error", "Could not read the last upgrade: "+err.Error())
		return
	}
	if !state.StartedAt.IsZero() && state.FinishedAt.IsZero() {
		// Nothing is cleared out from under an upgrade that is running:
		// the card is the only thing saying it is.
		s.redirect(w, r, settingsTab("version"), "warn", "That upgrade has not finished yet.")
		return
	}
	if err := s.engine.Store().SaveUpgradeState(nodestore.UpgradeState{}); err != nil {
		s.redirect(w, r, settingsTab("version"), "error", "Could not clear it: "+err.Error())
		return
	}
	s.redirect(w, r, settingsTab("version"), "ok", "Cleared.")
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
	if version == "" || strings.ContainsAny(version, " \t/\\") {
		// What the button carries is a version this server was just told
		// about; anything shaped otherwise did not come from the button.
		// Which versions are installable is the engine's to say, since
		// that depends on the channel: a release has a version number, a
		// branch build has whatever git describe called it.
		s.redirect(w, r, settingsTab("version"), "error", "That is not a build of cP:Restic.")
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
		s.redirect(w, r, settingsTab("version"), "error", "Could not start the upgrade: "+err.Error())
		return
	}
	s.redirect(w, r, settingsTab("version"), "ok",
		"Installing "+version+". This page says how it goes; the service restarts on its way through.")
}
