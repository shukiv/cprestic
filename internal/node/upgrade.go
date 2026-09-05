package node

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shuki/cprest/internal/agent"
	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/update"
)

// upgradeGivesUp is when an upgrade that has said nothing is called
// failed. The installer downloads restic on a new server and copies a
// binary; minutes, not hours. Long enough that a slow machine is not
// called broken, short enough that a page does not spin for ever.
const upgradeGivesUp = 30 * time.Minute

// installerWrapper is what the transient unit runs. It takes no arguments
// and interpolates nothing: it works from its own directory, so the only
// thing that decides what it installs is where it was written.
const installerWrapper = `#!/bin/sh
# Written by cprest. Runs the installer of a release that has already been
# checked against the release key, and records what it said.
cd "$(dirname "$0")" || exit 1
exec >install.log 2>&1
sh cprest-plugin/install.sh
echo $? > status
`

// upgradeDir is where releases are unpacked, beside the staging root
// rather than in it: staging is swept.
func (e *Engine) upgradeDir() string {
	return filepath.Join(filepath.Dir(e.settings.StagingRoot), "upgrade")
}

// StartUpgrade installs a published release over this one.
//
// It is the one thing in this program that fetches something and runs it
// as root, so what may be installed is narrow: the release the daily check
// found, if it is newer than what is running. The version is passed in so
// that what an operator agreed to on the confirmation page is what gets
// installed -- it is checked against the stored release, never used in its
// place.
//
// The work happens on a goroutine and the installer runs outside this
// process, because installing restarts this service: a child of this
// process would be killed halfway through replacing the binary that is
// running.
func (e *Engine) StartUpgrade(version string) error {
	state, err := e.store.UpdateState()
	if err != nil {
		return err
	}
	switch {
	case !update.IsRelease(version):
		return fmt.Errorf("%q is not a released version", version)
	case version != state.Version:
		return fmt.Errorf("%s is not the release this server has been told about; check again first", version)
	case !update.Newer(agent.Version, version):
		return fmt.Errorf("this server already runs %s", agent.Version)
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return fmt.Errorf("systemd-run is not on this server, so an upgrade cannot be started " +
			"from here; install the release by hand instead")
	}
	if busy, err := e.workInFlight(); err != nil {
		return err
	} else if busy != "" {
		return fmt.Errorf("%s is being worked on now, and installing restarts the service, "+
			"which would interrupt it; try again when it has finished", busy)
	}

	running, err := e.UpgradeStatus()
	if err != nil {
		return err
	}
	if !running.StartedAt.IsZero() && running.FinishedAt.IsZero() {
		return fmt.Errorf("an upgrade to %s is already running", running.Version)
	}
	if !e.upgrading.CompareAndSwap(false, true) {
		return fmt.Errorf("an upgrade is already running")
	}

	begun := nodestore.UpgradeState{
		Version: version, From: agent.Version,
		Stage: "downloading", StartedAt: time.Now().UTC(),
	}
	if err := e.store.SaveUpgradeState(begun); err != nil {
		e.upgrading.Store(false)
		return err
	}
	e.log.Info("installing a newer release", "version", version, "running", agent.Version)

	go func() {
		defer e.upgrading.Store(false)
		if err := e.runUpgrade(begun); err != nil {
			e.log.Error("install a newer release", "version", version, "error", err)
			// Nothing was installed, so nothing downloaded is worth
			// keeping. What an installer that ran and failed left is kept
			// instead, because that is where the reason is.
			if err := os.RemoveAll(filepath.Join(e.upgradeDir(), version)); err != nil {
				e.log.Warn("remove what a failed upgrade left", "error", err)
			}
			begun.Stage = ""
			begun.FinishedAt = time.Now().UTC()
			begun.Failed = true
			begun.Error = err.Error()
			if err := e.store.SaveUpgradeState(begun); err != nil {
				e.log.Error("record a failed upgrade", "error", err)
			}
		}
	}()
	return nil
}

// runUpgrade fetches the release, checks it, unpacks it and hands it to
// systemd. It returns once the installer has been started, not once it has
// finished: by then this process is being restarted by it.
func (e *Engine) runUpgrade(state nodestore.UpgradeState) error {
	dir := filepath.Join(e.upgradeDir(), state.Version)
	// A directory left by an earlier attempt is not built on: what runs
	// as root here is what this download produced, whole.
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	tarball, err := update.DefaultSource("").Fetch(ctx, state.Version, dir)
	if err != nil {
		return err
	}
	if err := update.Unpack(tarball, dir); err != nil {
		return err
	}
	// The tarball has done its job and is 16 MB of what is now unpacked
	// beside it.
	if err := os.Remove(tarball); err != nil {
		e.log.Warn("remove the downloaded release", "error", err)
	}

	wrapper := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(wrapper, []byte(installerWrapper), 0o700); err != nil {
		return err
	}

	state.Stage = "installing"
	state.Dir = dir
	if err := e.store.SaveUpgradeState(state); err != nil {
		return err
	}

	// A transient unit, so the installer lives in a cgroup of its own and
	// survives the restart it performs. A child of this process would be
	// killed with it.
	unit := "cprest-upgrade-" + nodestore.NewID()[:8]
	run := exec.Command("systemd-run", "--collect", "--unit="+unit,
		"--description=cP:Restic upgrade to "+state.Version, "/bin/sh", wrapper)
	output, err := run.CombinedOutput()
	if err != nil {
		return fmt.Errorf("start the installer: %w: %s", err, strings.TrimSpace(string(output)))
	}
	e.log.Info("the installer is running", "unit", unit, "dir", dir)
	return nil
}

// UpgradeStatus is the upgrade in flight, or the last one that ran.
//
// The process that started an upgrade is not the process that reports on
// it: installing restarts this service. So what happened is read back off
// disk -- the exit status the installer wrote, and what it printed --
// rather than remembered.
func (e *Engine) UpgradeStatus() (nodestore.UpgradeState, error) {
	state, err := e.store.UpgradeState()
	if err != nil {
		return nodestore.UpgradeState{}, err
	}
	if state.StartedAt.IsZero() || !state.FinishedAt.IsZero() {
		return state, nil
	}

	finished := false
	if state.Dir != "" {
		if code, ok := exitStatus(filepath.Join(state.Dir, "status")); ok {
			finished = true
			state.Failed = code != 0
			if state.Failed {
				state.Error = fmt.Sprintf("the installer stopped with status %d", code)
			}
		}
	}
	if !finished && time.Since(state.StartedAt) > upgradeGivesUp {
		finished = true
		// The installer replaces this binary and restarts the service, so
		// a version that has changed to the one asked for is an upgrade
		// that worked whatever became of the file it should have written.
		if agent.Version != state.Version {
			state.Failed = true
			state.Error = "the installer did not finish, and nothing said why"
		}
	}
	if !finished {
		return state, nil
	}

	state.Stage = ""
	state.FinishedAt = time.Now().UTC()
	state.Log = tailFile(filepath.Join(state.Dir, "install.log"), 4000)
	if err := e.store.SaveUpgradeState(state); err != nil {
		e.log.Error("record how the upgrade ended", "error", err)
	}
	if !state.Failed {
		// What is left is an unpacked copy of what is now installed.
		if err := os.RemoveAll(state.Dir); err != nil {
			e.log.Warn("remove the unpacked release", "dir", state.Dir, "error", err)
		}
	}
	return state, nil
}

// workInFlight names an account something is being done to now, or an
// empty string. Installing restarts the service, and a restart fails a
// backup that is running.
func (e *Engine) workInFlight() (string, error) {
	jobs, err := e.store.Jobs(0)
	if err != nil {
		return "", err
	}
	for _, run := range jobs {
		if run.Status == job.StatusRunning {
			return run.Account, nil
		}
	}
	restores, err := e.store.Restores(0)
	if err != nil {
		return "", err
	}
	for _, run := range restores {
		if run.Status == job.StatusRunning {
			return run.Account, nil
		}
	}
	return "", nil
}

// exitStatus reads the status the installer wrote when it finished.
func exitStatus(path string) (int, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		return 0, false
	}
	return code, true
}

// tailFile is the last of a file, which for an installer's output is the
// part that says how it ended.
func tailFile(path string, limit int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	if info.Size() > limit {
		if _, err := file.Seek(-limit, 2); err != nil {
			return ""
		}
	}
	body, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}
