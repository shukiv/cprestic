package node

import (
	"context"
	"time"

	"github.com/shuki/cprest/internal/agent"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/update"
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
		found := nodestore.UpdateState{CheckedAt: time.Now().UTC()}
		release, err := update.Latest(asked, nil, update.Repo)
		if err != nil {
			found.Error = err.Error()
			e.log.Warn("check for a newer release", "error", err)
		} else {
			found.Version = release.Version
			found.URL = release.URL
			found.Notes = release.Notes
			if update.Newer(agent.Version, release.Version) {
				e.log.Info("a newer release is available",
					"running", agent.Version, "released", release.Version)
			}
		}
		if err := e.store.SaveUpdateState(found); err != nil {
			e.log.Error("record the update check", "error", err)
		}
	}()
}
