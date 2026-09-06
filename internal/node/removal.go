package node

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
)

// RemovalSafety is the fast, local answer returned to cPanel's blocking
// account-removal hook. The hook must not start a backup: pkgacct can take
// hours, while cPanel is waiting synchronously for this answer.
type RemovalSafety struct {
	Enforced             bool
	Allowed              bool
	Detail               string
	MissingRepositoryIDs []string
}

// AccountRemovalSafety checks persisted job history only. A copy counts
// when it came from a policy that included the whole account, the whole job
// succeeded, and the repository target was neither failed nor incomplete.
func (e *Engine) AccountRemovalSafety(account string, now time.Time) (RemovalSafety, error) {
	decisions, err := e.AccountRemovalSafeties([]string{account}, now)
	if err != nil {
		return RemovalSafety{}, err
	}
	return decisions[account], nil
}

// AccountRemovalSafeties evaluates many accounts from one state snapshot.
// The Accounts page may contain hundreds of users and must not reread all job
// history separately for every row.
func (e *Engine) AccountRemovalSafeties(accounts []string, now time.Time) (map[string]RemovalSafety, error) {
	settings, err := e.store.Settings()
	if err != nil {
		return nil, err
	}
	decisions := make(map[string]RemovalSafety, len(accounts))
	if !settings.ProtectAccountRemoval {
		for _, account := range accounts {
			decisions[account] = RemovalSafety{Allowed: true, Detail: "termination protection is disabled"}
		}
		return decisions, nil
	}

	policies, err := e.store.Policies()
	if err != nil {
		return nil, err
	}
	jobs, err := e.store.Jobs(0)
	if err != nil {
		return nil, err
	}
	jobsByAccount := make(map[string][]nodestore.Job)
	for _, stored := range jobs {
		jobsByAccount[stored.Account] = append(jobsByAccount[stored.Account], stored)
	}
	names, err := e.repositoryDisplayNames()
	if err != nil {
		return nil, err
	}
	now = now.UTC()
	for _, account := range accounts {
		decision := accountRemovalSafety(account, now, policies, jobsByAccount[account], names)
		decision.Enforced = true
		decisions[account] = decision
	}
	return decisions, nil
}

func accountRemovalSafety(account string, now time.Time, policies []nodestore.Policy,
	jobs []nodestore.Job, names map[string]string) RemovalSafety {
	if account == "" {
		return RemovalSafety{Detail: "cPanel removal hook did not name an account"}
	}
	expected := map[string]time.Duration{}
	fullPolicies := map[string]bool{}
	for _, policy := range policies {
		if !fullAccountPolicy(policy, account) {
			continue
		}
		due, ok := policyFreshness(policy, now)
		if !ok {
			continue
		}
		fullPolicies[policy.ID] = true
		for _, repositoryID := range policy.RepositoryIDs {
			if current, exists := expected[repositoryID]; !exists || due < current {
				expected[repositoryID] = due
			}
		}
	}
	if len(expected) == 0 {
		return RemovalSafety{Detail: "no enabled full-account backup policy covers this account"}
	}

	latest := map[string]time.Time{}
	for _, stored := range jobs {
		if stored.Status != job.StatusSuccess || !stored.CompleteAccount || !fullPolicies[stored.PolicyID] {
			continue
		}
		at := stored.QueuedAt
		if stored.FinishedAt != nil {
			at = *stored.FinishedAt
		}
		for _, target := range stored.Targets {
			if target.Status != job.TargetSuccess || target.Incomplete {
				continue
			}
			if _, promised := expected[target.RepositoryID]; promised && at.After(latest[target.RepositoryID]) {
				latest[target.RepositoryID] = at
			}
		}
	}

	var never, stale []string
	var missingIDs []string
	for repositoryID, due := range expected {
		name := names[repositoryID]
		if name == "" {
			name = shortRepositoryID(repositoryID)
		}
		last, exists := latest[repositoryID]
		switch {
		case !exists:
			never = append(never, name)
			missingIDs = append(missingIDs, repositoryID)
		case now.Sub(last) > due:
			stale = append(stale, name)
			missingIDs = append(missingIDs, repositoryID)
		}
	}
	sort.Strings(never)
	sort.Strings(stale)
	sort.Strings(missingIDs)
	if len(never) > 0 || len(stale) > 0 {
		parts := make([]string, 0, 2)
		if len(never) > 0 {
			parts = append(parts, "no complete copy at "+strings.Join(never, ", "))
		}
		if len(stale) > 0 {
			parts = append(parts, "stale complete copy at "+strings.Join(stale, ", "))
		}
		return RemovalSafety{Detail: strings.Join(parts, "; "), MissingRepositoryIDs: missingIDs}
	}
	return RemovalSafety{Allowed: true, Detail: "recent complete copies exist at every promised destination"}
}

// QueueRemovalPreparation queues the smallest combination of enabled
// full-account policies that covers every copy currently blocking account
// termination. Jobs are stored together and the single standalone worker
// runs them sequentially, so their staging directories never overlap.
func (e *Engine) QueueRemovalPreparation(account string, now time.Time) ([]nodestore.Policy, error) {
	decision, err := e.AccountRemovalSafety(account, now)
	if err != nil {
		return nil, err
	}
	if !decision.Enforced {
		return nil, errors.New("node: account-removal protection is not enabled")
	}
	if decision.Allowed {
		return nil, errors.New("node: this account already has every recent complete copy required for removal")
	}
	if len(decision.MissingRepositoryIDs) == 0 {
		return nil, fmt.Errorf("node: cannot prepare this account: %s", decision.Detail)
	}
	policies, err := e.store.Policies()
	if err != nil {
		return nil, err
	}
	var candidates []nodestore.Policy
	for _, policy := range policies {
		if !fullAccountPolicy(policy, account) {
			continue
		}
		if _, ok := policyFreshness(policy, now); !ok {
			continue
		}
		candidates = append(candidates, policy)
	}
	selected := minimumPolicyCover(decision.MissingRepositoryIDs, candidates)
	if len(selected) == 0 {
		return nil, errors.New("node: no enabled full-account schedules can refresh every missing copy")
	}

	e.workMu.Lock()
	defer e.workMu.Unlock()
	busy, err := e.store.RunningJobFor(account)
	if err != nil {
		return nil, err
	}
	if busy {
		return nil, fmt.Errorf("node: account %s already has work in flight", account)
	}
	jobs := make([]nodestore.Job, 0, len(selected))
	for _, policy := range selected {
		jobs = append(jobs, nodestore.Job{
			PolicyID: policy.ID, Account: account, Status: job.StatusPending,
		})
	}
	if _, err := e.store.PutJobs(jobs); err != nil {
		return nil, err
	}
	return selected, nil
}

type policyCover struct {
	policy       nodestore.Policy
	repositories []string
}

// minimumPolicyCover solves the small set-cover problem exactly. Backup
// installations usually have two or three destinations; branch-and-bound
// starts from a valid greedy cover and only searches for smaller answers.
func minimumPolicyCover(missing []string, policies []nodestore.Policy) []nodestore.Policy {
	wanted := make(map[string]bool, len(missing))
	for _, repositoryID := range missing {
		wanted[repositoryID] = true
	}
	var candidates []policyCover
	for _, policy := range policies {
		seen := map[string]bool{}
		var repositories []string
		for _, repositoryID := range policy.RepositoryIDs {
			if wanted[repositoryID] && !seen[repositoryID] {
				seen[repositoryID] = true
				repositories = append(repositories, repositoryID)
			}
		}
		if len(repositories) > 0 {
			candidates = append(candidates, policyCover{policy: policy, repositories: repositories})
		}
	}
	best := greedyPolicyCover(wanted, candidates)
	if len(best) == 0 {
		return nil
	}

	covered := map[string]int{}
	chosen := make([]bool, len(candidates))
	var selected []int
	var search func()
	search = func() {
		all := true
		for repositoryID := range wanted {
			if covered[repositoryID] == 0 {
				all = false
				break
			}
		}
		if all {
			if len(selected) < len(best) {
				best = best[:0]
				for _, index := range selected {
					best = append(best, candidates[index].policy)
				}
			}
			return
		}
		if len(selected)+1 >= len(best) {
			return
		}

		// Branch on the uncovered repository with the fewest remaining
		// policies. This rejects impossible branches early.
		pick := ""
		var options []int
		for repositoryID := range wanted {
			if covered[repositoryID] > 0 {
				continue
			}
			var available []int
			for index, candidate := range candidates {
				if chosen[index] || !contains(candidate.repositories, repositoryID) {
					continue
				}
				available = append(available, index)
			}
			if len(available) == 0 {
				return
			}
			if pick == "" || len(available) < len(options) {
				pick, options = repositoryID, available
			}
		}
		for _, index := range options {
			chosen[index] = true
			selected = append(selected, index)
			for _, repositoryID := range candidates[index].repositories {
				covered[repositoryID]++
			}
			search()
			for _, repositoryID := range candidates[index].repositories {
				covered[repositoryID]--
			}
			selected = selected[:len(selected)-1]
			chosen[index] = false
		}
	}
	search()
	return best
}

func greedyPolicyCover(wanted map[string]bool, candidates []policyCover) []nodestore.Policy {
	covered := map[string]bool{}
	used := make([]bool, len(candidates))
	var selected []nodestore.Policy
	for len(covered) < len(wanted) {
		best, gain := -1, 0
		for index, candidate := range candidates {
			if used[index] {
				continue
			}
			added := 0
			for _, repositoryID := range candidate.repositories {
				if !covered[repositoryID] {
					added++
				}
			}
			if added > gain {
				best, gain = index, added
			}
		}
		if best < 0 {
			return nil
		}
		used[best] = true
		selected = append(selected, candidates[best].policy)
		for _, repositoryID := range candidates[best].repositories {
			covered[repositoryID] = true
		}
	}
	return selected
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func fullAccountPolicy(policy nodestore.Policy, account string) bool {
	if !policy.Enabled || len(policy.RepositoryIDs) == 0 ||
		policy.SkipHomedir || policy.SkipDatabases || policy.SkipEmail {
		return false
	}
	if policy.AllAccounts() {
		return true
	}
	for _, covered := range policy.Accounts {
		if covered == account {
			return true
		}
	}
	return false
}

func policyFreshness(policy nodestore.Policy, now time.Time) (time.Duration, bool) {
	schedule, err := cron.ParseStandard(policy.ScheduleCron)
	if err != nil {
		return 0, false
	}
	first := schedule.Next(now)
	interval := schedule.Next(first).Sub(first)
	if interval <= 0 {
		return 0, false
	}
	due := 2 * interval
	if policy.AlertNoBackupDays > 0 {
		due = time.Duration(policy.AlertNoBackupDays) * 24 * time.Hour
	}
	return due, due > 0
}

func (e *Engine) repositoryDisplayNames() (map[string]string, error) {
	repositories, err := e.store.Repositories()
	if err != nil {
		return nil, err
	}
	destinations, err := e.store.Destinations()
	if err != nil {
		return nil, err
	}
	destinationNames := make(map[string]string, len(destinations))
	for _, destination := range destinations {
		destinationNames[destination.ID] = destination.Name
	}
	names := make(map[string]string, len(repositories))
	for _, repository := range repositories {
		names[repository.ID] = destinationNames[repository.DestinationID]
	}
	return names, nil
}

func shortRepositoryID(id string) string {
	if len(id) > 8 {
		return fmt.Sprintf("repository %.8s", id)
	}
	return "repository " + id
}
