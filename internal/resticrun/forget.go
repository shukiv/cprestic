package resticrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ForgetPlan is what a retention run would remove, or did.
//
// It exists so an operator can read the answer before agreeing to it.
// Deleting backups is the one thing this program does that cannot be
// undone, and "keep seven daily" is easy to write and hard to picture.
type ForgetPlan struct {
	Groups []ForgetGroup
	// Kept and Removed are the totals across every group.
	Kept    int
	Removed int
}

// ForgetGroup is one account's backups on one server: the unit a keep
// count actually counts.
type ForgetGroup struct {
	Host    string
	Account string
	Tags    []string
	Kept    int
	Removed int
	// Oldest and Newest of what would go, so the operator can see the
	// span rather than only the count.
	Oldest time.Time
	Newest time.Time
}

// ParseForgetPlan reads what "restic forget --json" said.
//
// restic writes two different things to stdout under --json: the plan, as
// an array of groups, or a single object describing why it could not run
// at all. The second is what a stale lock looks like, and reading it as
// an empty plan would report "nothing to remove" for a repository whose
// retention has in fact never run.
func ParseForgetPlan(stdout []byte) (ForgetPlan, error) {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return ForgetPlan{}, nil
	}
	if trimmed[0] == '{' {
		var failure struct {
			MessageType string `json:"message_type"`
			Code        int    `json:"code"`
			Message     string `json:"message"`
		}
		if err := json.Unmarshal(trimmed, &failure); err == nil && failure.Message != "" {
			return ForgetPlan{}, fmt.Errorf("resticrun: forget: %s", failure.Message)
		}
		return ForgetPlan{}, fmt.Errorf("resticrun: forget said something unreadable")
	}

	var groups []struct {
		Host   string     `json:"host"`
		Tags   []string   `json:"tags"`
		Keep   []Snapshot `json:"keep"`
		Remove []Snapshot `json:"remove"`
	}
	if err := json.Unmarshal(trimmed, &groups); err != nil {
		return ForgetPlan{}, fmt.Errorf("resticrun: read the retention plan: %w", err)
	}

	var plan ForgetPlan
	for _, group := range groups {
		if len(group.Remove) == 0 && len(group.Keep) == 0 {
			continue
		}
		summary := ForgetGroup{
			Host: group.Host, Tags: group.Tags,
			Kept: len(group.Keep), Removed: len(group.Remove),
		}
		for _, tag := range group.Tags {
			if account, found := strings.CutPrefix(tag, "account:"); found {
				summary.Account = account
				break
			}
		}
		for _, snapshot := range group.Remove {
			if summary.Oldest.IsZero() || snapshot.Time.Before(summary.Oldest) {
				summary.Oldest = snapshot.Time
			}
			if snapshot.Time.After(summary.Newest) {
				summary.Newest = snapshot.Time
			}
		}
		plan.Kept += summary.Kept
		plan.Removed += summary.Removed
		plan.Groups = append(plan.Groups, summary)
	}
	// Whatever would lose the most, first: that is what an operator is
	// deciding about.
	sort.Slice(plan.Groups, func(i, j int) bool {
		if plan.Groups[i].Removed != plan.Groups[j].Removed {
			return plan.Groups[i].Removed > plan.Groups[j].Removed
		}
		return plan.Groups[i].Account < plan.Groups[j].Account
	})
	return plan, nil
}

// ForgetPlanned runs forget and reports what it did, or in a dry run what
// it would do.
func (r *Runner) ForgetPlanned(ctx context.Context, repo Repository, spec ForgetSpec) (ForgetPlan, error) {
	args, err := ForgetArgs(spec)
	if err != nil {
		return ForgetPlan{}, err
	}
	result, err := r.run(ctx, repo, args, secondary{}, nil)
	if err != nil {
		return ForgetPlan{}, err
	}
	if err := classifyExit(result.ExitCode, result.Stderr, false); err != nil {
		return ForgetPlan{}, err
	}
	if result.Truncated {
		// The plan is what an operator agrees to and what gets applied.
		// A cut-off one reads as a smaller, complete answer.
		return ForgetPlan{}, fmt.Errorf(
			"resticrun: the retention plan was too large to read in full")
	}
	return ParseForgetPlan(result.Stdout)
}

// Prune reclaims the space snapshots were holding.
//
// It is separate from forget because it is the expensive half: forget
// rewrites a little metadata, prune walks the whole repository. Running
// it only when forget actually removed something is the difference
// between a nightly job that costs nothing and one that reads every pack
// file for no reason.
func (r *Runner) Prune(ctx context.Context, repo Repository) error {
	result, err := r.run(ctx, repo, []string{"prune"}, secondary{}, nil)
	if err != nil {
		return err
	}
	return classifyExit(result.ExitCode, result.Stderr, false)
}
