package node

import (
	"context"
	"time"

	"github.com/shukiv/gniza/internal/agent"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/update"
)

// updateCheckEvery is a day. A release is not published often, and this
// runs on somebody else's server against somebody else's rate limit.
const updateCheckEvery = 24 * time.Hour

// checkForUpdate asks GitHub what the newest release is, once a day.
//
// It reads a version number and stores it. Nothing is downloaded and
// nothing is installed: what to do about a newer release is a decision
// taken in the interface by somebody who can see what it is.
//
// The ask happens on its own goroutine. It is a network call to another
// company's service, and the scheduler it is called from has backups to
// start.
func (e *Engine) checkForUpdate(ctx context.Context, now time.Time) {
	settings, err := e.store.Settings()
	if err != nil || settings.NoUpdateCheck {
		return
	}
	state, err := e.store.UpdateState()
	if err != nil {
		e.log.Error("read the last update check", "error", err)
		return
	}
	if !state.CheckedAt.IsZero() && now.Sub(state.CheckedAt) < updateCheckEvery {
		return
	}
	if !e.checkingUpdate.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer e.checkingUpdate.Store(false)

		asked, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		// The time is recorded whatever happened, so a server with no
		// route to GitHub asks once a day rather than every fifteen
		// seconds for ever.
		found, err := e.askWhatIsPublished(asked, Channel(settings))
		found.CheckedAt = time.Now().UTC()
		if err != nil {
			found.Error = err.Error()
			e.log.Warn("check for a newer build", "error", err)
		} else if e.UpdateOffered(found) {
			e.log.Info("a newer build is available",
				"running", agent.Version, "published", found.Version)
		}
		if err := e.store.SaveUpdateState(found); err != nil {
			e.log.Error("record the update check", "error", err)
		}
	}()
}

// CheckForUpdateNow asks GitHub straight away and records the answer.
//
// The daily check is what keeps a server current on its own; this is for
// somebody standing at the page who has just been told a release exists,
// and for an operator watching a failing check to see the reason change.
func (e *Engine) CheckForUpdateNow(ctx context.Context) (nodestore.UpdateState, error) {
	settings, err := e.store.Settings()
	if err != nil {
		return nodestore.UpdateState{}, err
	}
	found, err := e.askWhatIsPublished(ctx, Channel(settings))
	found.CheckedAt = time.Now().UTC()
	if err != nil {
		found.Error = err.Error()
		if saveErr := e.store.SaveUpdateState(found); saveErr != nil {
			e.log.Error("record the update check", "error", saveErr)
		}
		return found, err
	}
	if err := e.store.SaveUpdateState(found); err != nil {
		return found, err
	}
	return found, nil
}

// Channel is where this server takes its updates from.
func Channel(settings nodestore.Settings) update.Channel {
	if update.Channel(settings.UpdateChannel) == update.ChannelDist {
		return update.ChannelDist
	}
	return update.ChannelReleases
}

// askWhatIsPublished reads what the chosen channel is offering.
//
// A release comes from GitHub's own API, which knows about drafts and
// pre-releases and has notes attached. The dist branch is three files: the
// manifest says what the build is, and the release key's signature is what
// makes it worth reading at all.
func (e *Engine) askWhatIsPublished(ctx context.Context, channel update.Channel) (nodestore.UpdateState, error) {
	found := nodestore.UpdateState{Channel: string(channel)}
	if channel == update.ChannelDist {
		manifest, err := update.DistSource(update.Repo).Published(ctx, "")
		if err != nil {
			return found, err
		}
		found.Version = manifest.Version
		found.BuiltAt = manifest.BuiltAt
		found.URL = "https://github.com/" + update.Repo + "/tree/" + update.DistBranch
		found.Notes = "The build published on the " + update.DistBranch +
			" branch, signed with the same release key."
		return found, nil
	}
	release, err := update.Latest(ctx, nil, update.Repo)
	if err != nil {
		return found, err
	}
	found.Version, found.URL, found.Notes = release.Version, release.URL, release.Notes
	return found, nil
}

// UpdateOffered says whether what was found is worth installing over what
// is running.
//
// Releases are compared by version number: a build of somebody's own is
// never told to replace itself with a release. A branch has no version
// numbers that mean anything -- v0.1.0-18-gabc1234 is not later than
// v0.1.0-9-gdef5678 in any order a computer can see -- so what is compared
// is the commit each was built from.
func (e *Engine) UpdateOffered(state nodestore.UpdateState) bool {
	if state.Version == "" {
		return false
	}
	if storedChannel(state) != update.ChannelDist {
		return update.Newer(agent.Version, state.Version)
	}
	if state.BuiltAt.IsZero() {
		return false
	}
	running, known := agent.Built()
	if !known {
		// This build says nothing about where it came from, so anything
		// that does say is worth offering. A server on this channel asked
		// to follow the work.
		return true
	}
	return state.BuiltAt.After(running)
}
