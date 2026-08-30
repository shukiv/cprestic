package webui

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/destination"
	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/node"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/protocol"
	"github.com/shuki/cprest/internal/resticrun"
)

// --- dashboard ---

type dashboardView struct {
	Hostname     string
	Accounts     []accountView
	Destinations []destinationView
	Policies     []nodestore.Policy

	Protected      int
	Stale          int
	Unprotected    int
	ProtectedPct   int
	StalePct       int
	UnprotectedPct int

	// Attention lists the accounts worth acting on, worst first.
	Attention     []accountView
	AttentionMore int

	NextRun       string
	NextRunPolicy string
	NextRunIn     string

	StagingFree uint64
	SpaceTight  bool

	LastDrill *nodestore.Restore
}

// attentionLimit keeps the overview short: it is a prompt to act, not a
// second copy of the accounts page.
const attentionLimit = 3

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	destinations, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	accounts, _, err := s.accountViews(r)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	policies, err := s.engine.Store().Policies()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	view := dashboardView{
		Hostname:     s.engine.Settings().Hostname,
		Accounts:     accounts,
		Destinations: destinations,
		Policies:     policies,
	}

	for _, account := range accounts {
		switch {
		case account.Protected():
			view.Protected++
		case account.LastBackup == nil:
			view.Unprotected++
		default:
			view.Stale++
		}
	}
	if total := len(accounts); total > 0 {
		view.ProtectedPct = view.Protected * 100 / total
		view.StalePct = view.Stale * 100 / total
		// The remainder, so the bar always fills exactly.
		view.UnprotectedPct = 100 - view.ProtectedPct - view.StalePct
	}

	// Worst first: never backed up, then failing, then stale.
	for _, account := range accounts {
		if account.Protected() || account.Running {
			continue
		}
		view.Attention = append(view.Attention, account)
	}
	sort.SliceStable(view.Attention, func(i, j int) bool {
		return attentionRank(view.Attention[i]) < attentionRank(view.Attention[j])
	})
	if len(view.Attention) > attentionLimit {
		view.AttentionMore = len(view.Attention) - attentionLimit
		view.Attention = view.Attention[:attentionLimit]
	}

	view.NextRun, view.NextRunPolicy, view.NextRunIn = nextRun(policies, time.Now())

	settings := s.engine.Settings()
	if free, err := stagingFree(settings.StagingRoot); err == nil {
		view.StagingFree = free
		// Roughly: not enough room for a large account plus headroom.
		view.SpaceTight = free < 10<<30
	}

	if restores, err := s.engine.Store().Restores(0); err == nil {
		for i := range restores {
			if restores[i].Kind == node.KindVerify && restores[i].Status.Terminal() {
				view.LastDrill = &restores[i]
				break
			}
		}
	}

	s.render(w, r, "dashboard.html", "Overview", "dashboard", view)
}

func attentionRank(a accountView) int {
	switch {
	case a.LastBackup == nil:
		return 0
	case a.LastStatus == job.StatusFailed:
		return 1
	default:
		return 2
	}
}

// nextRun reports when the soonest enabled schedule fires.
func nextRun(policies []nodestore.Policy, now time.Time) (at, name, in string) {
	var (
		soonest time.Time
		which   string
	)
	for _, policy := range policies {
		if !policy.Enabled || len(policy.RepositoryIDs) == 0 {
			continue
		}
		schedule, err := cron.ParseStandard(policy.ScheduleCron)
		if err != nil {
			continue
		}
		next := schedule.Next(now)
		if soonest.IsZero() || next.Before(soonest) {
			soonest, which = next, policy.Name
		}
	}
	if soonest.IsZero() {
		return "", "", ""
	}
	return soonest.Local().Format("15:04"), which, "in " + humanUntil(soonest.Sub(now))
}

func humanUntil(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

// --- destinations ---

type destinationView struct {
	nodestore.Destination
	Repository nodestore.Repository
	Endpoint   string
	Status     string
	// PublicKey is the line to add to the remote account's
	// authorized_keys, shown so it can be copied at any time rather than
	// only once when the destination was created.
	PublicKey    string
	RemoteTarget string
}

func (s *Server) destinationViews() ([]destinationView, error) {
	destinations, err := s.engine.Store().Destinations()
	if err != nil {
		return nil, err
	}
	repositories, err := s.engine.Store().Repositories()
	if err != nil {
		return nil, err
	}

	views := make([]destinationView, 0, len(destinations))
	for _, dest := range destinations {
		view := destinationView{Destination: dest, Endpoint: endpointOf(dest)}
		for _, repo := range repositories {
			if repo.DestinationID == dest.ID {
				view.Repository = repo
				break
			}
		}
		switch {
		case dest.LastCheckError != "":
			view.Status = "error"
		case dest.LastCheckedAt == nil:
			view.Status = "unchecked"
		default:
			view.Status = "ok"
		}
		if dest.Type == string(destination.TypeSFTP) {
			view.PublicKey = s.engine.PublicKeyFor(dest)
			view.RemoteTarget = dest.Config["user"] + "@" + dest.Config["host"]
		}
		views = append(views, view)
	}
	return views, nil
}

// endpointOf renders the human-facing address of a destination.
func endpointOf(dest nodestore.Destination) string {
	switch destination.Type(dest.Type) {
	case destination.TypeLocal:
		return dest.Config["root"]
	case destination.TypeSFTP:
		host := dest.Config["host"]
		if port := dest.Config["port"]; port != "" && port != "22" {
			host += ":" + port
		}
		return dest.Config["user"] + "@" + host + ":" + dest.Config["root"]
	case destination.TypeREST:
		return dest.Config["base_url"]
	case destination.TypeS3:
		endpoint := dest.Config["endpoint"]
		if endpoint == "" {
			endpoint = "s3.amazonaws.com"
		}
		return endpoint + "/" + dest.Config["bucket"]
	default:
		return ""
	}
}

func (s *Server) handleDestinations(w http.ResponseWriter, r *http.Request) {
	views, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	view := struct {
		Destinations []destinationView
		Hostname     string
		Editing      *destinationView
	}{Destinations: views, Hostname: s.engine.Settings().Hostname}

	if id := r.URL.Query().Get("edit"); id != "" {
		for i := range views {
			if views[i].ID == id {
				view.Editing = &views[i]
				break
			}
		}
		if view.Editing == nil {
			s.redirect(w, r, "/destinations", "error", "That destination no longer exists.")
			return
		}
	}
	s.render(w, r, "destinations.html", "Backup destinations", "destinations", view)
}

func (s *Server) handleAddDestination(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	destType := r.PostFormValue("type")
	if name == "" {
		s.redirect(w, r, "/destinations", "error", "Give the destination a name.")
		return
	}

	config := map[string]string{}
	secrets := map[string]string{}
	switch destination.Type(destType) {
	case destination.TypeREST:
		config["base_url"] = strings.TrimSpace(r.PostFormValue("base_url"))
		if maintenance := strings.TrimSpace(r.PostFormValue("maintenance_base_url")); maintenance != "" {
			config["maintenance_base_url"] = maintenance
		}
		if r.PostFormValue("append_only") != "" {
			config["append_only"] = "true"
		}
		if bundle := strings.TrimSpace(r.PostFormValue("ca_bundle")); bundle != "" {
			config["ca_bundle"] = bundle
		}
		secrets["username"] = strings.TrimSpace(r.PostFormValue("username"))
		secrets["password"] = r.PostFormValue("password")
	case destination.TypeSFTP:
		s.addSFTPDestination(w, r, name, repositoryPathFrom(r, s.engine.Settings().Hostname))
		return
	case destination.TypeS3:
		config["bucket"] = strings.TrimSpace(r.PostFormValue("bucket"))
		if endpoint := strings.TrimSpace(r.PostFormValue("endpoint")); endpoint != "" {
			config["endpoint"] = endpoint
		}
		if region := strings.TrimSpace(r.PostFormValue("region")); region != "" {
			config["region"] = region
		}
		secrets["access_key_id"] = strings.TrimSpace(r.PostFormValue("access_key_id"))
		secrets["secret_access_key"] = r.PostFormValue("secret_access_key")
	case destination.TypeLocal:
		config["root"] = strings.TrimSpace(r.PostFormValue("root"))
	default:
		s.redirect(w, r, "/destinations", "error", "Choose a destination type.")
		return
	}

	repositoryPath := repositoryPathFrom(r, s.engine.Settings().Hostname)

	dest := nodestore.Destination{
		Name:       name,
		Type:       destType,
		Config:     config,
		AppendOnly: config["append_only"] == "true",
	}
	stored, _, err := s.engine.AddDestination(dest, secrets, repositoryPath)
	if err != nil {
		s.redirect(w, r, "/destinations", "error", err.Error())
		return
	}

	// Check it now, while the operator is still looking at the screen.
	if err := s.engine.TestDestination(r.Context(), stored.ID); err != nil {
		s.redirect(w, r, "/destinations", "warn",
			fmt.Sprintf("Saved %q, but it could not be reached: %v", name, err))
		return
	}
	if _, err := s.engine.EnsureProvisioned(r.Context()); err != nil {
		s.redirect(w, r, "/destinations", "warn",
			fmt.Sprintf("Saved %q and reached it, but creating the repository failed: %v", name, err))
		return
	}
	s.redirect(w, r, "/destinations", "ok",
		fmt.Sprintf("%q is reachable and its repository is ready.", name))
}

// addSFTPDestination sets up another Linux server as a destination.
//
// cprest generates its own key, reads the server's host key, and — if the
// operator supplied the remote password — installs the key and proves it
// works. The password is used for that and discarded.
func (s *Server) addSFTPDestination(w http.ResponseWriter, r *http.Request, name, repositoryPath string) {
	result, err := s.engine.AddSFTPDestination(node.SFTPRequest{
		Name:            name,
		Host:            r.PostFormValue("host"),
		Port:            atoiOr(r.PostFormValue("port"), 22),
		User:            r.PostFormValue("user"),
		RemoteDir:       r.PostFormValue("root"),
		RepositoryPath:  repositoryPath,
		Password:        r.PostFormValue("password"),
		ExistingKeyPath: strings.TrimSpace(r.PostFormValue("identity_file")),
	})
	if err != nil {
		s.redirect(w, r, "/destinations", "error", err.Error())
		return
	}

	switch {
	case result.Verified:
		if _, err := s.engine.EnsureProvisioned(r.Context()); err != nil {
			s.redirect(w, r, "/destinations", "warn", fmt.Sprintf(
				"Logged in to %s, but creating the repository failed: %v", result.Destination.Name, err))
			return
		}
		s.redirect(w, r, "/destinations", "ok", fmt.Sprintf(
			"%s is ready. cprest installed its own key and the repository is created.",
			result.Destination.Name))
	case result.Warning != "":
		s.redirect(w, r, "/destinations", "warn", result.Warning)
	default:
		s.redirect(w, r, "/destinations", "ok", "Saved.")
	}
}

// repositoryPathFrom keeps this server's backups in their own folder.
func repositoryPathFrom(r *http.Request, hostname string) string {
	path := strings.TrimSpace(r.PostFormValue("repo_path"))
	if path == "" {
		path = hostname
	}
	if path == "" {
		path = "cprest"
	}
	return path
}

// handleEditDestination updates a destination that already exists.
//
// Credentials left blank are kept, so an operator correcting a hostname is
// not made to retype an access key. The repository path is not editable
// once the repository exists: it is where the backups already are, and
// changing it would silently start a new empty repository elsewhere.
func (s *Server) handleEditDestination(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	dest, err := s.engine.Store().Destination(id)
	if err != nil {
		s.redirect(w, r, "/destinations", "error", "That destination no longer exists.")
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.redirect(w, r, "/destinations?edit="+id, "error", "Give the destination a name.")
		return
	}
	dest.Name = name

	config := map[string]string{}
	secrets := map[string]string{}
	switch destination.Type(dest.Type) {
	case destination.TypeREST:
		config["base_url"] = strings.TrimSpace(r.PostFormValue("base_url"))
		if maintenance := strings.TrimSpace(r.PostFormValue("maintenance_base_url")); maintenance != "" {
			config["maintenance_base_url"] = maintenance
		}
		if r.PostFormValue("append_only") != "" {
			config["append_only"] = "true"
		}
		if bundle := strings.TrimSpace(r.PostFormValue("ca_bundle")); bundle != "" {
			config["ca_bundle"] = bundle
		}
		if user := strings.TrimSpace(r.PostFormValue("username")); user != "" {
			secrets["username"] = user
			secrets["password"] = r.PostFormValue("password")
		}
	case destination.TypeSFTP:
		config["host"] = strings.TrimSpace(r.PostFormValue("host"))
		config["user"] = strings.TrimSpace(r.PostFormValue("user"))
		config["root"] = strings.TrimSpace(r.PostFormValue("root"))
		// The generated key and the learned host key belong to this
		// destination and are not something to retype.
		config["identity_file"] = dest.Config["identity_file"]
		config["known_hosts_file"] = dest.Config["known_hosts_file"]
		if port := strings.TrimSpace(r.PostFormValue("port")); port != "" && port != "22" {
			config["port"] = port
		}
	case destination.TypeS3:
		config["bucket"] = strings.TrimSpace(r.PostFormValue("bucket"))
		if endpoint := strings.TrimSpace(r.PostFormValue("endpoint")); endpoint != "" {
			config["endpoint"] = endpoint
		}
		if region := strings.TrimSpace(r.PostFormValue("region")); region != "" {
			config["region"] = region
		}
		if key := strings.TrimSpace(r.PostFormValue("access_key_id")); key != "" {
			secrets["access_key_id"] = key
			secrets["secret_access_key"] = r.PostFormValue("secret_access_key")
		}
	case destination.TypeLocal:
		config["root"] = strings.TrimSpace(r.PostFormValue("root"))
	}
	dest.Config = config
	dest.AppendOnly = config["append_only"] == "true"

	if len(secrets) > 0 {
		secretID, err := node.SealCredentials(s.engine.Store(), s.engine.Vault(), secrets)
		if err != nil {
			s.redirect(w, r, "/destinations?edit="+id, "error", err.Error())
			return
		}
		dest.CredentialsSecretID = secretID
	}

	if err := s.engine.SaveDestination(dest); err != nil {
		s.redirect(w, r, "/destinations?edit="+id, "error", err.Error())
		return
	}
	if err := s.engine.TestDestination(r.Context(), id); err != nil {
		s.redirect(w, r, "/destinations", "warn",
			fmt.Sprintf("Saved, but %s could not be reached: %v", name, err))
		return
	}
	s.redirect(w, r, "/destinations", "ok", name+" updated and reachable.")
}

func (s *Server) handleTestDestination(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if err := s.engine.TestDestination(r.Context(), id); err != nil {
		s.redirect(w, r, "/destinations", "error", err.Error())
		return
	}
	s.redirect(w, r, "/destinations", "ok", "Destination reachable.")
}

func (s *Server) handleDeleteDestination(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if err := s.engine.Store().DeleteDestination(id); err != nil {
		s.redirect(w, r, "/destinations", "error", "Could not remove it: "+err.Error())
		return
	}
	s.redirect(w, r, "/destinations", "ok",
		"Destination removed. Anything already stored there is untouched, "+
			"but cprest no longer knows how to read it.")
}

// --- schedules ---

func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	policies, err := s.engine.Store().Policies()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	destinations, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	accounts, _, err := s.accountViews(r)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	view := struct {
		Policies     []nodestore.Policy
		Destinations []destinationView
		Accounts     []accountView
		Editing      *nodestore.Policy
		Selected     map[string]bool
		Chosen       map[string]bool
	}{Policies: policies, Destinations: destinations, Accounts: accounts}

	if id := r.URL.Query().Get("edit"); id != "" {
		policy, err := s.engine.Store().Policy(id)
		if err != nil {
			s.redirect(w, r, "/schedule", "error", "That schedule no longer exists.")
			return
		}
		view.Editing = &policy
		view.Selected = map[string]bool{}
		for _, repositoryID := range policy.RepositoryIDs {
			view.Selected[repositoryID] = true
		}
		view.Chosen = map[string]bool{}
		for _, account := range policy.Accounts {
			view.Chosen[account] = true
		}
	}
	s.render(w, r, "schedule.html", "Schedules", "schedule", view)
}

func (s *Server) handleSaveSchedule(w http.ResponseWriter, r *http.Request) {
	policy := nodestore.Policy{
		ID:           r.PostFormValue("id"),
		Name:         strings.TrimSpace(r.PostFormValue("name")),
		ScheduleCron: strings.TrimSpace(r.PostFormValue("cron")),
		PayloadMode:  r.PostFormValue("mode"),
		Compression:  r.PostFormValue("compression"),
		Enabled:      r.PostFormValue("enabled") != "",
		Retention: nodestore.Retention{
			KeepDaily:   atoiOr(r.PostFormValue("keep_daily"), 7),
			KeepWeekly:  atoiOr(r.PostFormValue("keep_weekly"), 4),
			KeepMonthly: atoiOr(r.PostFormValue("keep_monthly"), 6),
		},
		RepositoryIDs: r.PostForm["repository"],
	}
	if policy.Name == "" || policy.ScheduleCron == "" {
		s.redirect(w, r, "/schedule", "error", "A schedule needs a name and a time.")
		return
	}
	if len(policy.RepositoryIDs) == 0 {
		s.redirect(w, r, "/schedule", "error",
			"Choose at least one destination, or nothing will be backed up.")
		return
	}
	// Empty means every account, resolved when the schedule fires so new
	// accounts are included without an edit.
	if r.PostFormValue("scope") == "selected" {
		policy.Accounts = r.PostForm["account"]
		if len(policy.Accounts) == 0 {
			s.redirect(w, r, "/schedule", "error", "Choose at least one account.")
			return
		}
	}
	if existing := policy.ID; existing != "" {
		previous, err := s.engine.Store().Policy(existing)
		if err == nil {
			policy.CreatedAt = previous.CreatedAt
			policy.LastRunAt = previous.LastRunAt
		}
	}

	editing := policy.ID != ""
	if _, err := s.engine.Store().PutPolicy(policy); err != nil {
		s.redirect(w, r, "/schedule", "error", err.Error())
		return
	}
	if editing {
		s.redirect(w, r, "/schedule", "ok", "Schedule updated.")
		return
	}
	s.redirect(w, r, "/schedule", "ok", "Schedule saved.")
}

// handleRunSchedule queues a schedule's accounts straight away, rather
// than waiting for its next cron time.
func (s *Server) handleRunSchedule(w http.ResponseWriter, r *http.Request) {
	queued, skipped, err := s.engine.RunPolicyNow(r.Context(), r.PostFormValue("id"))
	if err != nil {
		s.redirect(w, r, "/schedule", "error", err.Error())
		return
	}

	switch {
	case queued == 0:
		s.redirect(w, r, "/schedule", "warn", fmt.Sprintf(
			"Nothing queued: %s already being backed up.", strings.Join(skipped, ", ")))
	case len(skipped) > 0:
		s.redirect(w, r, "/schedule", "warn", fmt.Sprintf(
			"Queued %d account(s). Skipped %s, already being backed up.",
			queued, strings.Join(skipped, ", ")))
	default:
		s.redirect(w, r, "/schedule", "ok", fmt.Sprintf(
			"Queued %d account(s). Watch progress under History.", queued))
	}
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.Store().DeletePolicy(r.PostFormValue("id")); err != nil {
		s.redirect(w, r, "/schedule", "error", err.Error())
		return
	}
	s.redirect(w, r, "/schedule", "ok", "Schedule removed. Existing backups are untouched.")
}

// --- accounts ---

type accountView struct {
	cpanel.AccountInfo
	LastBackup *time.Time
	LastStatus job.Status
	Running    bool
}

// Stripe is the severity colour on the row's leading edge, so state reads
// without depending on colour alone.
func (a accountView) Stripe() string {
	switch {
	case a.Running:
		return "cpr-s-warn"
	case a.LastBackup == nil, a.LastStatus == job.StatusFailed:
		return "cpr-s-bad"
	case a.LastStatus == job.StatusPartialSuccess:
		return "cpr-s-warn"
	default:
		return "cpr-s-ok"
	}
}

// Filter is the value the client-side state filter matches on.
func (a accountView) Filter() string {
	if a.LastBackup == nil {
		return "bad"
	}
	if a.LastStatus == job.StatusSuccess {
		return "ok"
	}
	return "warn"
}

// Protected reports whether this account has a backup that worked.
func (a accountView) Protected() bool {
	return a.LastBackup != nil && a.LastStatus == job.StatusSuccess
}

func (s *Server) accountViews(r *http.Request) ([]accountView, []string, error) {
	accounts, err := s.engine.Accounts(r.Context())
	if err != nil {
		// A server whose accounts cannot be listed is still worth showing
		// a page for, with the reason on it.
		return nil, []string{"Could not list cPanel accounts: " + err.Error()}, nil
	}
	jobs, err := s.engine.Store().Jobs(0)
	if err != nil {
		return nil, nil, err
	}

	latest := map[string]nodestore.Job{}
	running := map[string]bool{}
	for _, stored := range jobs {
		if !stored.Status.Terminal() {
			running[stored.Account] = true
			continue
		}
		if previous, seen := latest[stored.Account]; !seen || stored.QueuedAt.After(previous.QueuedAt) {
			latest[stored.Account] = stored
		}
	}

	views := make([]accountView, 0, len(accounts))
	var warnings []string
	for _, account := range accounts {
		view := accountView{AccountInfo: account, Running: running[account.User]}
		if last, seen := latest[account.User]; seen {
			view.LastBackup = last.FinishedAt
			view.LastStatus = last.Status
			// A listing does not measure sizes, but a completed backup
			// did, so show what it read.
			for _, target := range last.Targets {
				if target.BytesProcessed > view.SizeBytes {
					view.SizeBytes = target.BytesProcessed
				}
			}
			if last.Status == job.StatusFailed {
				warnings = append(warnings,
					fmt.Sprintf("The last backup of %s failed.", account.User))
			}
		}
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].User < views[j].User })
	return views, warnings, nil
}

// handleAccounts lists what this server has, using local state only.
//
// Nothing here talks to a backup destination: listing snapshots means a
// round trip per repository, and doing that for every account would make
// the page as slow as the account walk this replaced. Snapshots are read
// when one account is opened.
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, warnings, err := s.accountViews(r)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	policies, err := s.engine.Store().Policies()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	view := struct {
		Accounts    []accountView
		Policies    []nodestore.Policy
		Warnings    []string
		Protected   int
		Unprotected int
	}{Accounts: accounts, Policies: policies, Warnings: warnings}
	for _, account := range accounts {
		switch {
		case account.Protected():
			view.Protected++
		case account.LastBackup == nil:
			view.Unprotected++
		}
	}
	s.render(w, r, "accounts.html", "Accounts", "accounts", view)
}

// activityRow is one thing that happened to an account.
type activityRow struct {
	When       time.Time
	What       string
	Status     job.Status
	Note       string
	Log        string
	Download   string
	Incomplete bool
}

func (a activityRow) Stripe() string {
	switch a.Status {
	case job.StatusSuccess:
		return "cpr-s-ok"
	case job.StatusFailed:
		return "cpr-s-bad"
	default:
		return "cpr-s-warn"
	}
}

// handleAccount shows one account: its snapshots, its rehearsals and what
// has happened to it.
//
// This is the page that reads from the backup destination, and it does so
// for one account only — which is why it is a page you open rather than
// something folded into the list.
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	user := r.URL.Query().Get("user")
	accounts, _, err := s.accountViews(r)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	var account accountView
	found := false
	for _, candidate := range accounts {
		if candidate.User == user {
			account, found = candidate, true
			break
		}
	}
	if !found {
		s.redirect(w, r, "/accounts", "error", "There is no account called "+user+" on this server.")
		return
	}

	view := struct {
		Account        accountView
		Snapshots      []resticrun.Snapshot
		RepositoryID   string
		RepositoryName string
		LookupError    string
		Activity       []activityRow
		LastDrill      *nodestore.Restore
	}{Account: account}

	// Read this account's snapshots from the first provisioned
	// destination. One account, one round trip.
	destinations, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	for _, destination := range destinations {
		if destination.Repository.InitialisedAt == nil {
			continue
		}
		view.RepositoryID = destination.Repository.ID
		view.RepositoryName = destination.Name
		snapshots, err := s.engine.Snapshots(r.Context(), destination.Repository.ID, user)
		if err != nil {
			view.LookupError = err.Error()
			break
		}
		sort.Slice(snapshots, func(i, j int) bool {
			return snapshots[i].Time.After(snapshots[j].Time)
		})
		view.Snapshots = snapshots
		break
	}

	names := map[string]string{}
	for _, destination := range destinations {
		names[destination.Repository.ID] = destination.Name
	}

	jobs, err := s.engine.Store().Jobs(0)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	for _, stored := range jobs {
		if stored.Account != user {
			continue
		}
		when := stored.QueuedAt
		if stored.FinishedAt != nil {
			when = *stored.FinishedAt
		}
		for _, target := range stored.Targets {
			row := activityRow{
				When:       when,
				What:       "Backed up to " + fallback(names[target.RepositoryID], "a destination"),
				Status:     stored.Status,
				Log:        target.Detail,
				Incomplete: target.Incomplete,
			}
			switch {
			case target.Error != "":
				row.Note = target.Error
			case target.Status == job.TargetSuccess:
				row.Note = fmt.Sprintf("%s stored of %s read",
					humanBytes(target.BytesAdded), humanBytes(target.BytesProcessed))
			}
			view.Activity = append(view.Activity, row)
		}
		if len(stored.Targets) == 0 {
			view.Activity = append(view.Activity, activityRow{
				When: when, What: "Backup", Status: stored.Status, Note: stored.StagingErr,
			})
		}
	}

	restores, err := s.engine.Store().Restores(0)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	for i := range restores {
		restore := restores[i]
		if restore.Account != user {
			continue
		}
		when := restore.QueuedAt
		if restore.FinishedAt != nil {
			when = *restore.FinishedAt
		}
		row := activityRow{When: when, Status: restore.Status, Note: restore.Error}
		switch restore.Kind {
		case node.KindVerify:
			row.What = "Restore rehearsed"
			if restore.Detail != "" {
				row.Note = restore.Detail
			}
			if view.LastDrill == nil && restore.Status.Terminal() {
				view.LastDrill = &restores[i]
			}
		case protocol.RestoreFiles:
			row.What = "Files recovered"
			if restore.RestoredTo != "" {
				row.Note = "into " + restore.RestoredTo
			}
		default:
			row.What = "Rebuilt for download"
			if restore.Applied {
				row.What = "Restored over the live account"
			}
			if restore.ArchivePath != "" && !restore.Applied {
				row.Download = restore.ID
				row.Note = ""
			}
		}
		view.Activity = append(view.Activity, row)
	}

	sort.Slice(view.Activity, func(i, j int) bool {
		return view.Activity[i].When.After(view.Activity[j].When)
	})

	s.render(w, r, "account.html", account.User, "accounts", view)
}

func fallback(value, whenEmpty string) string {
	if value == "" {
		return whenEmpty
	}
	return value
}

func (s *Server) handleBackupNow(w http.ResponseWriter, r *http.Request) {
	account := r.PostFormValue("account")
	policyID := r.PostFormValue("policy")
	if policyID == "" {
		policies, err := s.engine.Store().Policies()
		if err != nil || len(policies) == 0 {
			s.redirect(w, r, "/accounts", "error",
				"Create a schedule first: it decides where the backup goes and what is kept.")
			return
		}
		policyID = policies[0].ID
	}
	if _, err := s.engine.QueueBackup(policyID, account); err != nil {
		s.redirect(w, r, "/accounts", "error", err.Error())
		return
	}
	s.redirect(w, r, "/accounts", "ok", "Backup of "+account+" queued.")
}

// --- restore ---

type restoreView struct {
	Accounts     []accountView
	Repositories []destinationView
	Account      string
	RepositoryID string
	Snapshots    []resticrun.Snapshot
	Restores     []nodestore.Restore
	LookupError  string
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	accounts, _, err := s.accountViews(r)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	destinations, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	restores, err := s.engine.Store().Restores(10)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	view := restoreView{
		Accounts: accounts, Repositories: destinations, Restores: restores,
		Account:      r.URL.Query().Get("account"),
		RepositoryID: r.URL.Query().Get("repository"),
	}
	// Default to the first provisioned repository so the common case is
	// one click rather than two.
	if view.RepositoryID == "" {
		for _, dest := range destinations {
			if dest.Repository.InitialisedAt != nil {
				view.RepositoryID = dest.Repository.ID
				break
			}
		}
	}
	if view.Account != "" && view.RepositoryID != "" {
		snapshots, err := s.engine.Snapshots(r.Context(), view.RepositoryID, view.Account)
		if err != nil {
			view.LookupError = err.Error()
		} else {
			// Newest first: the snapshot someone wants is almost always
			// the most recent one.
			sort.Slice(snapshots, func(i, j int) bool {
				return snapshots[i].Time.After(snapshots[j].Time)
			})
			view.Snapshots = snapshots
		}
	}
	s.render(w, r, "restore.html", "Restore", "restore", view)
}

func (s *Server) handleStartRestore(w http.ResponseWriter, r *http.Request) {
	restore := nodestore.Restore{
		Account:      r.PostFormValue("account"),
		RepositoryID: r.PostFormValue("repository"),
		SnapshotID:   r.PostFormValue("snapshot"),
		Kind:         protocol.RestoreAccount,
		Apply:        r.PostFormValue("apply") != "",
	}

	if paths := strings.TrimSpace(r.PostFormValue("paths")); paths != "" {
		restore.Kind = protocol.RestoreFiles
		restore.Apply = false
		for _, line := range strings.Split(paths, "\n") {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				restore.IncludePaths = append(restore.IncludePaths, trimmed)
			}
		}
		restore.TargetDir = strings.TrimSpace(r.PostFormValue("target"))
	}

	queued, err := s.engine.QueueRestore(restore)
	if err != nil {
		s.redirect(w, r, "/restore?account="+restore.Account, "error", err.Error())
		return
	}
	message := "Restore queued. The archive will be left on this server; nothing is overwritten."
	if queued.Apply {
		message = "Restore queued. It WILL overwrite the live account when it runs."
	}
	s.redirect(w, r, "/restore?account="+restore.Account, "ok", message)
}

// handleVerifyRequest rehearses a restore of an account's newest backup.
func (s *Server) handleVerifyRequest(w http.ResponseWriter, r *http.Request) {
	account := r.PostFormValue("account")
	if _, err := s.engine.QueueDrill(r.Context(), account); err != nil {
		s.redirect(w, r, "/accounts", "error", err.Error())
		return
	}
	s.redirect(w, r, "/restore", "ok", fmt.Sprintf(
		"Rehearsing a restore of %s. It rebuilds the account in scratch space, checks the "+
			"result and throws it away — the live account is not touched.", account))
}

// handleDownloadRequest rebuilds an account's newest backup into an archive
// that can then be fetched.
func (s *Server) handleDownloadRequest(w http.ResponseWriter, r *http.Request) {
	account := r.PostFormValue("account")

	// Download should download. Rebuilding takes minutes, so if an archive
	// for this account is already sitting there, hand that over instead of
	// making the operator wait for a copy of what they already have.
	if ready, found := s.engine.ReadyDownload(account); found {
		// Written directly, not through http.Redirect, which resolves a
		// query-only reference against the request path and would turn
		// this into "?p=accounts/".
		w.Header().Set("Location", "?p=download&id="+url.QueryEscape(ready.ID))
		w.WriteHeader(http.StatusSeeOther)
		return
	}

	if _, err := s.engine.QueueDownload(r.Context(), account); err != nil {
		s.redirect(w, r, "/accounts", "error", err.Error())
		return
	}
	s.redirect(w, r, "/restore", "ok", fmt.Sprintf(
		"Rebuilding the newest backup of %s. It takes a minute or two; a Download button "+
			"appears beside it below when it is ready. Nothing on the live account is "+
			"touched.", account))
}

// handleDownload streams a rebuilt archive to the browser.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	file, filename, size, err := s.engine.OpenArchiveForDownload(r.URL.Query().Get("id"))
	if err != nil {
		s.redirect(w, r, "/restore", "error", err.Error())
		return
	}
	defer file.Close()

	// Content-Disposition is what makes the browser save rather than
	// render, and it is also the only place the filename survives: cpsrvd
	// strips Content-Type from what the plugin returns.
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if _, err := io.Copy(w, file); err != nil {
		// The response is already going out, so there is nothing to say
		// to the browser beyond stopping.
		s.log.Error("stream archive", "file", filename, "error", err)
	}
}

// --- jobs ---

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.engine.Store().Jobs(100)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	restores, err := s.engine.Store().Restores(50)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	destinations, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	names := map[string]string{}
	for _, dest := range destinations {
		names[dest.Repository.ID] = dest.Name
	}
	s.render(w, r, "jobs.html", "History", "jobs", struct {
		Jobs        []nodestore.Job
		Restores    []nodestore.Restore
		Destination map[string]string
	}{jobs, restores, names})
}

// --- settings ---

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	settings := s.engine.Settings()
	free := uint64(0)
	if info, err := os.Stat(settings.StagingRoot); err == nil && info.IsDir() {
		free, _ = stagingFree(settings.StagingRoot)
	}
	s.render(w, r, "settings.html", "Settings", "settings", struct {
		Settings    nodestore.Settings
		StagingFree uint64
	}{settings, free})
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	settings := s.engine.Settings()
	settings.MaxConcurrent = atoiOr(r.PostFormValue("max_concurrent"), 1)
	settings.ResticBinary = strings.TrimSpace(r.PostFormValue("restic"))
	settings.ResticCACert = strings.TrimSpace(r.PostFormValue("restic_cacert"))
	if hostname := strings.TrimSpace(r.PostFormValue("hostname")); hostname != "" {
		settings.Hostname = hostname
	}

	// The staging root is deliberately not editable here. Snapshot paths
	// embed it and restic groups retention by path, so changing it after
	// the first backup orphans every existing retention group.
	if err := s.engine.Store().SaveSettings(settings); err != nil {
		s.redirect(w, r, "/settings", "error", err.Error())
		return
	}
	s.redirect(w, r, "/settings", "ok", "Saved. Restart the service for it to take effect.")
}

func atoiOr(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func stagingFree(path string) (uint64, error) {
	return freeBytes(filepath.Clean(path))
}
