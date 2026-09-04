package webui

import (
	"fmt"
	"math"
	"sort"

	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/nodestore"
)

// runningWork is one backup or restore that is happening now, as the strip
// at the top of every page names it.
//
// A backup takes minutes and a restore can take longer, and until now the
// only way to know one was under way was to be on the page that lists them.
// Somebody who has just asked for a restore and sees nothing asks for it
// again.
type runningWork struct {
	Account string
	// What it is: "Backing up" or "Restoring".
	Doing string
	// Detail is what is being written or read back, where that is
	// something more specific than the account.
	Detail string
	// Percent is restic's own figure, and Known says whether there is
	// one: a restore reports none, and "0%" would read as stuck.
	Percent float64
	Known   bool
}

// Label is the whole line, for a banner that is one sentence per run.
func (w runningWork) Label() string {
	line := w.Doing + " " + w.Account
	if w.Detail != "" {
		line += " — " + w.Detail
	}
	if w.Known {
		// Half rounds up, as it does beside the bar on the history
		// page: two places showing the same run must agree.
		line += fmt.Sprintf(" (%.0f%%)", math.Round(w.Percent))
	}
	return line
}

// runningWorkFor lists what is in flight, newest first.
//
// account narrows it to one account's own work, which is what a customer is
// shown: another account's restore is not theirs to see. An empty account
// means every account, which is the operator's view.
func runningWorkFor(store *nodestore.Store, account string) ([]runningWork, error) {
	jobs, err := store.Jobs(0)
	if err != nil {
		return nil, err
	}
	restores, err := store.Restores(0)
	if err != nil {
		return nil, err
	}

	var running []runningWork
	for _, run := range jobs {
		if run.Status != job.StatusRunning || (account != "" && run.Account != account) {
			continue
		}
		work := runningWork{Account: run.Account, Doing: "Backing up"}
		if run.Progress != nil {
			work.Percent, work.Known = run.Progress.Percent, true
			work.Detail = run.Progress.Repository
		}
		running = append(running, work)
	}
	for _, run := range restores {
		if run.Status != job.StatusRunning || (account != "" && run.Account != account) {
			continue
		}
		running = append(running, runningWork{
			Account: run.Account, Doing: "Restoring", Detail: restoreRow{Restore: run}.Parts(),
		})
	}
	// A backup before a restore, and then by account, so the strip does
	// not reorder itself under somebody reading it.
	sort.SliceStable(running, func(i, j int) bool {
		if running[i].Doing != running[j].Doing {
			return running[i].Doing < running[j].Doing
		}
		return running[i].Account < running[j].Account
	})
	return running, nil
}
