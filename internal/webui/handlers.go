package webui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/destination"
	"github.com/shuki/cprest/internal/granular"
	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/node"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/pkgacct"
	"github.com/shuki/cprest/internal/protocol"
	"github.com/shuki/cprest/internal/reassemble"
	"github.com/shuki/cprest/internal/resticrun"
	"github.com/shuki/cprest/internal/staging"
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
		case account.Current():
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
		if account.Current() || account.Running {
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

// TypeName is the destination's type in the words the form used to offer
// it, so the table and the form agree with each other.
func (d destinationView) TypeName() string {
	switch destination.Type(d.Type) {
	case destination.TypeSFTP:
		return "Another Linux server (SFTP)"
	case destination.TypeREST:
		return "Backup server (restic REST)"
	case destination.TypeS3:
		return "S3 or S3-compatible"
	case destination.TypeLocal:
		return "Local disk or mounted NAS"
	}
	return d.Type
}

// Stripe is the severity colour on the row's leading edge.
func (d destinationView) Stripe() string {
	switch d.Status {
	case "ok":
		return "cpr-s-ok"
	case "error":
		return "cpr-s-bad"
	}
	return "cpr-s-warn"
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

// unnoted names the destinations whose recovery key has never been
// written down anywhere but this server.
func unnoted(views []destinationView) []string {
	var names []string
	for _, view := range views {
		if view.Repository.ID != "" && view.Repository.RecoveryNotedAt == nil {
			names = append(names, view.Name)
		}
	}
	return names
}

// handleRecoveryKey shows the password for one repository, once, because
// it was asked for.
//
// It is not on the page by default: a secret that is always rendered is a
// secret in every proxy log and every screenshot. This is a POST with the
// token, so it cannot be reached by a link someone was sent.
func (s *Server) handleRecoveryKey(w http.ResponseWriter, r *http.Request) {
	card, err := s.engine.Recovery(r.PostFormValue("repository"))
	if err != nil {
		s.redirect(w, r, "/destinations", "error", err.Error())
		return
	}
	views, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	s.render(w, r, "destinations.html", "Backup destinations", "destinations", destinationsView{
		Destinations: views,
		Hostname:     s.engine.Settings().Hostname,
		Revealed:     &card,
		RevealedID:   r.PostFormValue("repository"),
		Unnoted:      unnoted(views),
	})
}

// handleNoteRecovery records that the password has been written down
// somewhere off this server, which is what stops the warning.
func (s *Server) handleNoteRecovery(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.NoteRecoveryKey(r.PostFormValue("repository")); err != nil {
		s.redirect(w, r, "/destinations", "error", err.Error())
		return
	}
	s.redirect(w, r, "/destinations", "ok",
		"Noted. Keep it somewhere that survives this server — a password manager, "+
			"not a file on this machine.")
}

// handleRecoveryCard hands over the same thing as a file, for putting
// somewhere else. It is a download rather than a page so it does not sit
// in browser history.
func (s *Server) handleRecoveryCard(w http.ResponseWriter, r *http.Request) {
	card, err := s.engine.Recovery(r.PostFormValue("repository"))
	if err != nil {
		s.redirect(w, r, "/destinations", "error", err.Error())
		return
	}
	var body strings.Builder
	fmt.Fprintf(&body, `cprest recovery key
===================

Server        %s
Destination   %s
Repository    %s
Address       %s

Repository password
    %s

This password is the only thing that can read those backups. The
destination holds nothing but ciphertext, and the key that unlocks it
lives on %s. If that server is lost and this password is not written down
somewhere else, the backups it made cannot be read by anyone, including
cprest.

`, card.Hostname, card.Destination, card.Repository, card.URI,
		card.Password, card.Hostname)

	switch {
	case card.SSHPrivateKey != "":
		// Reaching an SFTP destination needs the key this server made for
		// it and the host key it pinned. A restore run anywhere else needs
		// both of those as well as the password.
		fmt.Fprintf(&body, `Reaching it from %s
------------------------------------------------------------------
    export RESTIC_REPOSITORY='%s'
    export RESTIC_PASSWORD='%s'

    restic -o sftp.args="%s" snapshots
    restic -o sftp.args="%s" restore <snapshot-id> --target /somewhere

Reaching it from anywhere else
------------------------------------------------------------------
That server's own SSH key is below. Write it to a file, make it readable
only by you, and point restic at it:

    install -m 600 /dev/null /root/cprest-key
    cat > /root/cprest-key <<'KEY'
%s
KEY
    printf '%%s\n' '%s' > /root/cprest-known-hosts

    export RESTIC_REPOSITORY='%s'
    export RESTIC_PASSWORD='%s'
    restic -o sftp.args="-i /root/cprest-key -o UserKnownHostsFile=/root/cprest-known-hosts -o StrictHostKeyChecking=yes -o IdentitiesOnly=yes" snapshots

If that key no longer works — the account was removed, or its
authorized_keys was rebuilt — any account with SSH access to
%s@%s will do. Drop the -o sftp.args and let ssh ask for a
password:

    export RESTIC_REPOSITORY='%s'
    restic snapshots

`, card.Hostname, card.URI, card.Password, card.ResticOptions, card.ResticOptions,
			card.SSHPrivateKey, card.SSHHostKey, card.URI, card.Password,
			card.SSHUser, card.SSHHost, card.URI)

	default:
		fmt.Fprintf(&body, `To restore without cprest, on any machine with restic installed:

    export RESTIC_REPOSITORY='%s'
    export RESTIC_PASSWORD='%s'

    restic snapshots
    restic restore <snapshot-id> --target /somewhere

`, card.URI, card.Password)
	}

	fmt.Fprintf(&body, `Keep this file somewhere that survives the loss of %s. It carries
everything needed to read those backups, which is the point of it and
also the reason it belongs in a password manager rather than on a disk.
Written %s
`, card.Hostname, time.Now().UTC().Format("2006-01-02 15:04 MST"))

	filename := fmt.Sprintf("cprest-recovery-%s-%s.txt", card.Hostname, card.Repository)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(body.String()))
}

// destinationsView is the destinations page.
type destinationsView struct {
	Destinations []destinationView
	Hostname     string
	Editing      *destinationView
	// Adding renders the form as a page of its own rather than as a
	// drawer over the list. That is what a browser with no JavaScript
	// follows the button to, and what a bookmarked link opens.
	Adding bool
	// FormError is why the last attempt was refused, shown inside the
	// drawer with the form still filled in. A destination that could not
	// be reached used to send the operator back to an empty form with the
	// reason in a banner above it, which is a way of asking them to type
	// it all again.
	FormError string
	// Submitted is what they typed, so a rejected form comes back as they
	// left it.
	Submitted map[string]string
	// Revealed is a repository password an operator has just asked to
	// see, shown on this render and on no other.
	Revealed *node.RecoveryCard
	// RevealedID is the repository it belongs to, for the buttons beside
	// it. The password itself is never put in a form field.
	RevealedID string
	// Unnoted are the destinations whose recovery key exists nowhere but
	// this server.
	Unnoted []string
}

// Field is what an input should show: what was submitted, else what is
// stored for the destination being edited.
func (v destinationsView) Field(name string) string {
	if value, typed := v.Submitted[name]; typed {
		return value
	}
	if v.Editing == nil {
		return ""
	}
	switch name {
	case "name":
		return v.Editing.Name
	default:
		return v.Editing.Config[name]
	}
}

// FieldOr is Field with a default for the fields that have one.
func (v destinationsView) FieldOr(name, fallback string) string {
	if value := v.Field(name); value != "" {
		return value
	}
	return fallback
}

func (s *Server) handleDestinations(w http.ResponseWriter, r *http.Request) {
	views, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	view := destinationsView{
		Destinations: views,
		Hostname:     s.engine.Settings().Hostname,
		Adding:       r.URL.Query().Get("add") != "",
		Unnoted:      unnoted(views),
	}

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

// refuseDestination hands the form back with what went wrong and what was
// typed, rather than sending the operator to an empty one with the reason
// in a banner above it.
func (s *Server) refuseDestination(w http.ResponseWriter, r *http.Request, cause error) {
	views, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	view := destinationsView{
		Destinations: views,
		Hostname:     s.engine.Settings().Hostname,
		FormError:    cause.Error(),
		Submitted:    map[string]string{},
	}
	for name, values := range r.PostForm {
		// Secrets are not handed back: they would then live in a page
		// rather than only in the vault, and the operator retyping one is
		// a smaller cost than that.
		switch name {
		case "csrf", "password", "secret_access_key", "admin_password":
			continue
		}
		if len(values) > 0 {
			view.Submitted[name] = values[0]
		}
	}
	if id := r.PostFormValue("id"); id != "" {
		for i := range views {
			if views[i].ID == id {
				view.Editing = &views[i]
				break
			}
		}
	}
	s.render(w, r, "destinations.html", "Backup destinations", "destinations", view)
}

func (s *Server) handleAddDestination(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	destType := r.PostFormValue("type")
	if name == "" {
		s.refuseDestination(w, r, fmt.Errorf("Give the destination a name."))
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
		s.refuseDestination(w, r, fmt.Errorf("Choose a destination type."))
		return
	}

	repositoryPath := repositoryPathFrom(r, s.engine.Settings().Hostname)

	s.saveDestination(w, r, nodestore.Destination{
		Name:       name,
		Type:       destType,
		Config:     config,
		AppendOnly: config["append_only"] == "true",
	}, secrets, repositoryPath)
}

// saveDestination stores a destination, then proves it works while the
// operator is still looking at the screen.
func (s *Server) saveDestination(w http.ResponseWriter, r *http.Request,
	dest nodestore.Destination, secrets map[string]string, repositoryPath string) {

	name := dest.Name
	stored, _, err := s.engine.AddDestination(dest, secrets, repositoryPath)
	if err != nil {
		s.refuseDestination(w, r, err)
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
	s.finishSFTP(w, r, node.SFTPRequest{
		Name:            name,
		Host:            r.PostFormValue("host"),
		Port:            atoiOr(r.PostFormValue("port"), 22),
		User:            r.PostFormValue("user"),
		RemoteDir:       r.PostFormValue("root"),
		RepositoryPath:  repositoryPath,
		Password:        r.PostFormValue("password"),
		AdminUser:       strings.TrimSpace(r.PostFormValue("admin_user")),
		AdminPassword:   r.PostFormValue("admin_password"),
		ExistingKeyPath: strings.TrimSpace(r.PostFormValue("identity_file")),
	})
}

// finishSFTP runs a prepared request and says what happened.
func (s *Server) finishSFTP(w http.ResponseWriter, r *http.Request, request node.SFTPRequest) {
	result, err := s.engine.AddSFTPDestination(request)
	if err != nil {
		s.refuseDestination(w, r, err)
		return
	}

	switch {
	case result.Verified:
		if _, err := s.engine.EnsureProvisioned(r.Context()); err != nil {
			s.redirect(w, r, "/destinations", "warn", fmt.Sprintf(
				"Logged in to %s, but creating the repository failed: %v", result.Destination.Name, err))
			return
		}
		if result.Created {
			s.redirect(w, r, "/destinations", "ok", fmt.Sprintf(
				"%s is ready. cprest created %s on that server, gave it a locked password so "+
					"the key is the only way in, made the backup directory and created the "+
					"repository.", result.Destination.Name, request.User))
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

// excludePresets are directories that are rebuilt from what is already
// backed up: caches, compiled templates, session files. Storing them costs
// space every night and gives a restore nothing.
var excludePresets = map[string][]string{
	"Caches and temporary files": {
		"/home/*/tmp",
		"/home/*/.cpanel/caches",
		"/home/*/.cphorde",
		"/home/*/public_html/*/tmp",
		"/home/*/.cache",
		"**/node_modules",
		"**/.git",
	},
	"WordPress": {
		"/home/*/public_html/wp-content/cache",
		"/home/*/public_html/wp-content/uploads/cache",
		"/home/*/public_html/wp-content/backup*",
		"/home/*/public_html/wp-content/updraft",
		"/home/*/public_html/wp-content/ai1wm-backups",
	},
	"Magento": {
		"/home/*/public_html/var/cache",
		"/home/*/public_html/var/page_cache",
		"/home/*/public_html/var/session",
		"/home/*/public_html/generated",
		"/home/*/public_html/pub/static",
	},
	"PrestaShop": {
		"/home/*/public_html/var/cache",
		"/home/*/public_html/cache/smarty",
		"/home/*/public_html/img/tmp",
	},
	"Moodle": {
		"/home/*/moodledata/cache",
		"/home/*/moodledata/localcache",
		"/home/*/moodledata/sessions",
		"/home/*/moodledata/temp",
		"/home/*/moodledata/trashdir",
	},
}

// policyView is a schedule as the schedules page states it: the stored
// policy, plus the things the page says in words rather than in cron.
type policyView struct {
	nodestore.Policy
	// AccountCount is how many accounts this schedule covers right now.
	AccountCount int
	// Next is when it fires next, zero when it never will.
	Next time.Time
}

// Stripe is the severity colour on the row's leading edge.
func (p policyView) Stripe() string {
	switch {
	case !p.Enabled:
		return "cpr-s-warn"
	case len(p.RepositoryIDs) == 0:
		return "cpr-s-bad"
	default:
		return "cpr-s-ok"
	}
}

// When reads the schedule back in words. A hand-written expression that
// does not fit one of the shapes the form offers returns empty, and the
// page shows the expression alone.
func (p policyView) When() string { return humanCron(p.ScheduleCron) }

// NextIn is how long until this schedule fires, empty when it never will.
func (p policyView) NextIn() string {
	if p.Next.IsZero() {
		return ""
	}
	return "next in " + humanUntil(time.Until(p.Next))
}

// Covers names what the schedule backs up.
func (p policyView) Covers() string {
	if p.AllAccounts() {
		return "Every account"
	}
	return strings.Join(p.Accounts, ", ")
}

// CoversDetail counts what that comes to today, which matters for a
// schedule that says "every account" on a server where accounts come
// and go.
func (p policyView) CoversDetail() string {
	if !p.AllAccounts() {
		return ""
	}
	return fmt.Sprintf("%d account%s", p.AccountCount, plural(p.AccountCount))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

var weekdays = [7]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

// humanCron reads back the shapes the schedule form offers. Anything else
// returns empty rather than a wrong reading of someone's own expression.
func humanCron(expr string) string {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return ""
	}
	minute, hour, dayOfMonth, month, dayOfWeek := fields[0], fields[1], fields[2], fields[3], fields[4]
	if month != "*" {
		return ""
	}
	m, minuteIsNumber := atoi(minute)
	h, hourIsNumber := atoi(hour)
	switch {
	case !minuteIsNumber:
		return ""
	case hour == "*" && dayOfMonth == "*" && dayOfWeek == "*":
		return fmt.Sprintf("Every hour at :%02d", m)
	case !hourIsNumber:
		return ""
	case dayOfMonth == "*" && dayOfWeek == "*":
		return fmt.Sprintf("Every day at %02d:%02d", h, m)
	case dayOfMonth == "*":
		if d, ok := atoi(dayOfWeek); ok && d >= 0 && d <= 7 {
			return fmt.Sprintf("Every %s at %02d:%02d", weekdays[d%7], h, m)
		}
	}
	return ""
}

func atoi(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil
}

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
	now := time.Now()
	views := make([]policyView, 0, len(policies))
	for _, policy := range policies {
		view := policyView{Policy: policy, AccountCount: len(accounts)}
		if !policy.AllAccounts() {
			view.AccountCount = len(policy.Accounts)
		}
		if policy.Enabled && len(policy.RepositoryIDs) > 0 {
			if schedule, err := cron.ParseStandard(policy.ScheduleCron); err == nil {
				view.Next = schedule.Next(now)
			}
		}
		views = append(views, view)
	}

	view := struct {
		Policies     []policyView
		Destinations []destinationView
		Accounts     []accountView
		Editing      *nodestore.Policy
		Selected     map[string]bool
		Chosen       map[string]bool
		// ExcludePresets are the paths worth never storing, by the thing
		// that puts them there. They are suggestions the operator can
		// edit, not rules this program applies on its own.
		ExcludePresets map[string][]string
	}{
		Policies: views, Destinations: destinations, Accounts: accounts,
		ExcludePresets: excludePresets,
	}

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

// linesOf reads a textarea into a list, dropping blank lines and the
// spaces around each one.
func linesOf(raw string) []string {
	var lines []string
	for _, line := range strings.Split(raw, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
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
		// A schedule that leaves something out says so here; everything
		// it does not mention travels.
		IncludeSystem:     r.PostFormValue("include_system") != "",
		SkipHomedir:       r.PostFormValue("skip_homedir") != "",
		SkipDatabases:     r.PostFormValue("skip_databases") != "",
		SkipEmail:         r.PostFormValue("skip_email") != "",
		RetryFailed:       r.PostFormValue("retry_failed") != "",
		Excludes:          linesOf(r.PostFormValue("excludes")),
		AlertNoBackupDays: atoiOr(r.PostFormValue("alert_no_backup_days"), 0),
		AlertRunHours:     atoiOr(r.PostFormValue("alert_run_hours"), 0),
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
	// Progress is how far the running backup of this account has got,
	// nil when nothing is running for it.
	Progress *nodestore.JobProgress
	// ExpectedEvery is how often the schedules covering this account fire.
	// Zero means no enabled schedule covers it: whatever backup it has is
	// the last one it will ever get.
	ExpectedEvery time.Duration
	// AlertAfter is how long without a good backup this account's
	// schedule considers worth saying out loud.
	AlertAfter time.Duration
	// Runs and Succeeded are this account's record: how many backups have
	// finished, and how many of those worked. One good backup says little
	// on its own — a run that succeeds nine times in ten is a different
	// account to look after than one that has never failed.
	Runs      int
	Succeeded int
	// Verified is when a rehearsal last proved this account's backup
	// rebuilds, with VerifiedOK saying whether it passed.
	Verified   *time.Time
	VerifiedOK bool
	// Stored is what the last backup actually cost across its
	// destinations, after restic deduplicated it, and Took is how long the
	// whole run lasted — staging the account included, which is most of it
	// for a small account.
	Stored uint64
	Took   time.Duration
	// LastError is why the last backup failed, in the words it failed
	// with. A row that says only "the last run failed" sends the operator
	// to another page to find out what every row already knows.
	LastError string
	// Copies is how many backups of this account each destination has
	// taken. Counted from this server's own records, so it says what was
	// written rather than what survives: retention prunes old snapshots
	// in the repository, and nothing tells this server when it does.
	Copies []copyCount
}

// copyCount is one destination's share of an account's backups.
type copyCount struct {
	Destination string
	Written     int
}

// Record is the account's history in one line.
func (a accountView) Record() string {
	if a.Runs == 0 {
		return ""
	}
	return fmt.Sprintf("%d of %d succeeded", a.Succeeded, a.Runs)
}

// Failures is how many runs did not work, for the pages that show it as
// its own number rather than as part of a ratio.
func (a accountView) Failures() int { return a.Runs - a.Succeeded }

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
	switch a.State() {
	case StateProtected:
		return "ok"
	case StateNever:
		return "bad"
	default:
		return "warn"
	}
}

// State is what this account's backups amount to right now.
//
// "Protected" has to mean something an operator can rely on, so it is not
// merely "a backup once worked". A copy that succeeded six months ago and
// is refreshed by nothing protects a site that has changed every day
// since; a copy nothing will refresh again is worth saying out loud.
type State string

const (
	StateWorking     State = "working"
	StateProtected   State = "protected"
	StateOutOfDate   State = "out-of-date"
	StateUnscheduled State = "unscheduled"
	StatePartial     State = "partial"
	StateFailed      State = "failed"
	StateNever       State = "never"
)

func (a accountView) State() State {
	switch {
	case a.Running:
		return StateWorking
	case a.LastBackup == nil:
		return StateNever
	case a.LastStatus == job.StatusFailed:
		return StateFailed
	case a.LastStatus == job.StatusPartialSuccess:
		return StatePartial
	case a.ExpectedEvery == 0:
		// It has a good copy and nothing will ever take another.
		return StateUnscheduled
	case time.Since(*a.LastBackup) > a.overdueAfter():
		return StateOutOfDate
	default:
		return StateProtected
	}
}

// Current reports whether this account has a backup worth relying on: the
// last run succeeded, and a schedule has produced one since it was due.
func (a accountView) Current() bool { return a.State() == StateProtected }

// overdueAfter is how long without a good backup counts as out of date:
// what the schedule covering this account asked for, or two of its own
// runs, which is the smallest gap a slow night cannot explain.
func (a accountView) overdueAfter() time.Duration {
	if a.AlertAfter > 0 {
		return a.AlertAfter
	}
	return 2 * a.ExpectedEvery
}

// Why explains the state in the words the operator needs, shown under the
// pill rather than left to be inferred from a colour.
func (a accountView) Why() string {
	switch a.State() {
	case StateProtected:
		return ""
	case StateOutOfDate:
		return "the schedule has not produced one since"
	case StateUnscheduled:
		return "no schedule covers it, so it will not be taken again"
	case StatePartial:
		return "some files could not be read"
	case StateFailed:
		if a.LastError != "" {
			return a.LastError
		}
		return "the last run failed"
	case StateNever:
		return "nothing has ever been backed up"
	}
	return ""
}

// shortID is a repository id an operator can still recognise, for the
// case where its destination has been removed since.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// failureOf is why a job failed, taken from wherever it went wrong: the
// staging step, which stops the job before restic runs, or the first
// destination that refused it.
func failureOf(stored nodestore.Job) string {
	if stored.Status != job.StatusFailed {
		return ""
	}
	if stored.StagingErr != "" {
		return stored.StagingErr
	}
	for _, target := range stored.Targets {
		if target.Error != "" {
			return target.Error
		}
	}
	return ""
}

// cost is what one backup came to: the bytes it added across the
// destinations that took it, and how long the run took from end to end.
//
// Added bytes rather than processed: what an operator is paying for is
// what restic actually had to store, which on an unchanged account is a
// small fraction of what it read.
func cost(stored nodestore.Job) (uint64, time.Duration) {
	var bytes uint64
	var targets time.Duration
	for _, target := range stored.Targets {
		if target.Status != job.TargetSuccess {
			continue
		}
		bytes += target.BytesAdded
		targets += time.Duration(target.DurationSecs * float64(time.Second))
	}
	if stored.StartedAt != nil && stored.FinishedAt != nil {
		// The wall clock covers staging too, which is most of the time
		// spent on a small account.
		if wall := stored.FinishedAt.Sub(*stored.StartedAt); wall > 0 {
			return bytes, wall
		}
	}
	return bytes, targets
}

// longRunThreshold is how long a single run may take before it is worth
// saying so, taken from the shortest any schedule asked for.
func (s *Server) longRunThreshold() (time.Duration, error) {
	policies, err := s.engine.Store().Policies()
	if err != nil {
		return 0, err
	}
	threshold := 6 * time.Hour
	for _, policy := range policies {
		if policy.AlertRunHours > 0 {
			if asked := time.Duration(policy.AlertRunHours) * time.Hour; asked < threshold {
				threshold = asked
			}
		}
	}
	return threshold, nil
}

// expectedIntervals is how often each account is backed up, by the
// shortest interval among the enabled schedules covering it.
//
// The interval is read off the cron expression by asking it when it fires
// twice, rather than by interpreting the fields: the parser is the
// authority on what an expression means.
func (s *Server) expectedIntervals(accounts []cpanel.AccountInfo) (map[string]time.Duration, map[string]time.Duration, error) {
	policies, err := s.engine.Store().Policies()
	if err != nil {
		return nil, nil, err
	}
	intervals := map[string]time.Duration{}
	// How long without a good backup is worth saying out loud. A schedule
	// that named a number means it; one that did not means two of its own
	// runs, which is the smallest gap that cannot be explained by a slow
	// night.
	alerts := map[string]time.Duration{}
	now := time.Now()
	for _, policy := range policies {
		if !policy.Enabled || len(policy.RepositoryIDs) == 0 {
			continue
		}
		schedule, err := cron.ParseStandard(policy.ScheduleCron)
		if err != nil {
			continue
		}
		first := schedule.Next(now)
		interval := schedule.Next(first).Sub(first)
		if interval <= 0 {
			continue
		}

		covered := policy.Accounts
		if policy.AllAccounts() {
			covered = nil
			for _, account := range accounts {
				covered = append(covered, account.User)
			}
		}
		alertAfter := 2 * interval
		if policy.AlertNoBackupDays > 0 {
			alertAfter = time.Duration(policy.AlertNoBackupDays) * 24 * time.Hour
		}
		for _, name := range covered {
			if current, seen := intervals[name]; !seen || interval < current {
				intervals[name] = interval
			}
			if current, seen := alerts[name]; !seen || alertAfter < current {
				alerts[name] = alertAfter
			}
		}
	}
	return intervals, alerts, nil
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
	stuckAfter, err := s.longRunThreshold()
	if err != nil {
		return nil, nil, err
	}
	var warnings []string

	// How often each account is due a backup, taken from the schedules
	// that actually cover it. An account no schedule covers has no
	// expectation to fall behind, which is its own kind of exposure.
	expected, alertAfter, err := s.expectedIntervals(accounts)
	if err != nil {
		return nil, nil, err
	}

	latest := map[string]nodestore.Job{}
	running := map[string]bool{}
	progress := map[string]*nodestore.JobProgress{}
	runs := map[string]int{}
	succeeded := map[string]int{}
	for _, stored := range jobs {
		if !stored.Status.Terminal() {
			running[stored.Account] = true
			if stored.Progress != nil {
				progress[stored.Account] = stored.Progress
			}
			// A run still going long after it should have finished is
			// stuck more often than it is slow, and nothing else here
			// would ever say so.
			if stored.StartedAt != nil && time.Since(*stored.StartedAt) > stuckAfter {
				warnings = append(warnings, fmt.Sprintf(
					"The backup of %s has been running for %s. A run that long is usually stuck.",
					stored.Account, humanUntil(time.Since(*stored.StartedAt))))
			}
			continue
		}
		runs[stored.Account]++
		if stored.Status == job.StatusSuccess {
			succeeded[stored.Account]++
		}
		if previous, seen := latest[stored.Account]; !seen || stored.QueuedAt.After(previous.QueuedAt) {
			latest[stored.Account] = stored
		}
	}

	// Destination names, so a count can say where the copies went rather
	// than quoting a repository id.
	destinations, err := s.destinationViews()
	if err != nil {
		return nil, nil, err
	}
	names := map[string]string{}
	for _, dest := range destinations {
		names[dest.Repository.ID] = dest.Name
	}
	copies := map[string]map[string]int{}
	for _, stored := range jobs {
		if !stored.Status.Terminal() {
			continue
		}
		for _, target := range stored.Targets {
			if target.Status != job.TargetSuccess {
				continue
			}
			name := names[target.RepositoryID]
			if name == "" {
				name = shortID(target.RepositoryID)
			}
			if copies[stored.Account] == nil {
				copies[stored.Account] = map[string]int{}
			}
			copies[stored.Account][name]++
		}
	}

	// A rehearsal is the only thing that says a backup rebuilds, so when
	// one last ran belongs beside the account it was run for.
	restores, err := s.engine.Store().Restores(0)
	if err != nil {
		return nil, nil, err
	}
	type drill struct {
		at time.Time
		ok bool
	}
	verified := map[string]drill{}
	for _, restore := range restores {
		if restore.Kind != node.KindVerify || restore.FinishedAt == nil {
			continue
		}
		if previous, seen := verified[restore.Account]; seen && previous.at.After(*restore.FinishedAt) {
			continue
		}
		verified[restore.Account] = drill{
			at: *restore.FinishedAt,
			ok: restore.Status == job.StatusSuccess,
		}
	}

	views := make([]accountView, 0, len(accounts))
	for _, account := range accounts {
		view := accountView{
			AccountInfo:   account,
			Running:       running[account.User],
			Progress:      progress[account.User],
			ExpectedEvery: expected[account.User],
			AlertAfter:    alertAfter[account.User],
			Runs:          runs[account.User],
			Succeeded:     succeeded[account.User],
		}
		if drilled, seen := verified[account.User]; seen {
			at := drilled.at
			view.Verified, view.VerifiedOK = &at, drilled.ok
		}
		if last, seen := latest[account.User]; seen {
			view.Stored, view.Took = cost(last)
			view.LastError = failureOf(last)
		}
		for name, written := range copies[account.User] {
			view.Copies = append(view.Copies, copyCount{Destination: name, Written: written})
		}
		sort.Slice(view.Copies, func(i, j int) bool {
			return view.Copies[i].Destination < view.Copies[j].Destination
		})
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
		case account.Current():
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
				if onDisk(restore.ArchivePath) {
					row.Download = restore.ID
					row.Note = ""
				} else {
					row.Note = "no longer on this server"
				}
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

// restoreRow is a restore as the history shows it: the record, plus
// whether what it produced is still on this server.
//
// Output does not live for ever — it is swept once nobody has collected
// it, and a newer restore of the same account supersedes an older one — so
// whether a download still works is a question about the disk, asked when
// the page is rendered rather than assumed from the record.
type restoreRow struct {
	nodestore.Restore
	Collectable bool
}

// collectableRows pairs each restore with the state of what it left behind.
func collectableRows(restores []nodestore.Restore) []restoreRow {
	rows := make([]restoreRow, 0, len(restores))
	for _, restore := range restores {
		rows = append(rows, restoreRow{
			Restore:     restore,
			Collectable: restore.ArchivePath != "" && onDisk(restore.ArchivePath),
		})
	}
	return rows
}

// onDisk reports whether a path is still a readable file.
func onDisk(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

type restoreView struct {
	Accounts     []accountView
	Repositories []destinationView
	Account      string
	RepositoryID string
	Snapshots    []resticrun.Snapshot
	Restores     []restoreRow
	LookupError  string

	// The file picker for a whole-account restore: what the chosen
	// snapshot holds at the path being looked at, so files are picked
	// rather than typed from memory.
	FileSnapshot string
	FilePath     string
	FileUp       string
	FileEntries  []browseEntry
	FileErr      string

	// Granular restore: which part of the account is being picked, from
	// which snapshot, and what that snapshot holds at the path being
	// looked at. Populated only when a kind is chosen, so listing a
	// snapshot costs nothing until someone asks.
	Kinds      []granular.Kind
	Kind       granular.Kind
	SnapshotID string
	Path       string
	Up         string
	Entries    []browseEntry
	BrowseErr  string
}

// browseEntry is one line of a snapshot, as the picker shows it.
type browseEntry struct {
	Name string
	Path string
	Size uint64
	Dir  bool
	// Name of the item this entry would restore, in the form the kind
	// expects: a path for files, "domain/box" for a mailbox, a bare name
	// for a database.
	Item string
}

// KindTitle lets the template name a kind without knowing the package.
func (v restoreView) KindTitle(k granular.Kind) string { return k.Title() }

// Selected reports whether a kind is the one being picked.
func (v restoreView) Selected(k granular.Kind) bool { return v.Kind == k }

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
		Accounts: accounts, Repositories: destinations,
		Restores:     collectableRows(restores),
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

	// The picker over the chosen snapshot. It is only listed when a
	// snapshot is chosen, and only one directory at a time.
	if len(view.Snapshots) > 0 {
		view.FileSnapshot = r.URL.Query().Get("files")
		if view.FileSnapshot != "" {
			s.fillFilePicker(r, &view)
		}
	}

	view.Kinds = granular.Kinds
	view.Kind = granular.Kind(r.URL.Query().Get("item"))
	view.SnapshotID = r.URL.Query().Get("snapshot")
	if view.SnapshotID == "" && len(view.Snapshots) > 0 {
		view.SnapshotID = view.Snapshots[0].ID
	}
	if view.Kind != "" && view.SnapshotID != "" {
		s.fillPicker(r, &view)
	}
	s.render(w, r, "restore.html", "Restore", "restore", view)
}

// fillFilePicker lists one directory of the snapshot being restored from,
// so specific files are picked out of what is actually in there rather
// than typed from memory.
func (s *Server) fillFilePicker(r *http.Request, view *restoreView) {
	snapshot, ok := findSnapshot(view.Snapshots, view.FileSnapshot)
	if !ok {
		view.FileErr = "That backup is not in this destination."
		return
	}
	parts, err := reassemble.Classify(snapshot.Paths)
	if err != nil {
		view.FileErr = err.Error()
		return
	}
	root := parts.Homedir
	if root == "" {
		view.FileErr = "This backup has no home directory in it to pick files from."
		return
	}

	view.FilePath = root
	if asked := path.Clean(r.URL.Query().Get("path")); asked != "." && asked != "" {
		if asked == root || strings.HasPrefix(asked, root+"/") {
			view.FilePath = asked
		}
	}
	view.FileUp = parentWithin(view.FilePath, root)

	entries, err := s.engine.Browse(r.Context(), view.RepositoryID, snapshot.ID, view.FilePath)
	if err != nil {
		view.FileErr = err.Error()
		return
	}
	for _, entry := range entries {
		if entry.Path == view.FilePath {
			continue
		}
		view.FileEntries = append(view.FileEntries, browseEntry{
			Name: entry.Name, Path: entry.Path, Size: entry.Size,
			Dir: entry.IsDir(), Item: entry.Path,
		})
	}
	sort.Slice(view.FileEntries, func(i, j int) bool {
		if view.FileEntries[i].Dir != view.FileEntries[j].Dir {
			return view.FileEntries[i].Dir
		}
		return view.FileEntries[i].Name < view.FileEntries[j].Name
	})
}

// fillPicker lists the part of the snapshot the chosen kind is picked
// from. Only kinds that need a name are listed: the rest restore one known
// thing and have nothing to choose.
func (s *Server) fillPicker(r *http.Request, view *restoreView) {
	snapshot, ok := findSnapshot(view.Snapshots, view.SnapshotID)
	if !ok {
		view.BrowseErr = "That backup is not in this destination."
		return
	}
	parts, err := reassemble.Classify(snapshot.Paths)
	if err != nil {
		view.BrowseErr = err.Error()
		return
	}

	var root string
	switch view.Kind {
	case granular.KindFiles:
		root = parts.Homedir
	case granular.KindMailbox:
		root = path.Join(parts.Homedir, "mail")
	case granular.KindDatabase:
		root = parts.Databases
	default:
		return
	}
	if root == "" {
		view.BrowseErr = "This backup does not contain that part of the account."
		return
	}

	view.Path = view.Path0(r, root)
	entries, err := s.engine.Browse(r.Context(), view.RepositoryID, snapshot.ID, view.Path)
	if err != nil {
		view.BrowseErr = err.Error()
		return
	}
	view.Up = parentWithin(view.Path, root)
	if view.Kind == granular.KindMailbox && view.Path == root {
		// cPanel keeps the account's own mailbox directly in ~/mail and
		// every additional address under its domain, so the two are
		// offered separately rather than mixed into one listing.
		view.Entries = append(view.Entries, browseEntry{
			Name: "the account's own mailbox and every address on it",
			Path: root, Dir: false, Item: ".",
		})
	}
	for _, entry := range entries {
		// restic lists the directory itself alongside its contents.
		if entry.Path == view.Path {
			continue
		}
		if view.Kind == granular.KindMailbox && view.Path == root &&
			(!entry.IsDir() || maildirInternal(entry.Name)) {
			// The rest of ~/mail is the account's own maildir — folders,
			// index files, quota state — which is offered above as one
			// thing rather than a file at a time. Only domains are listed.
			continue
		}
		view.Entries = append(view.Entries, browseEntry{
			Name: entry.Name,
			Path: entry.Path,
			Size: entry.Size,
			Dir:  entry.IsDir(),
			Item: itemName(view.Kind, entry, parts),
		})
	}
	sort.Slice(view.Entries, func(i, j int) bool {
		if view.Entries[i].Dir != view.Entries[j].Dir {
			return view.Entries[i].Dir
		}
		return view.Entries[i].Name < view.Entries[j].Name
	})
}

// Path0 keeps the browse path inside the root it was rooted at, so a
// crafted query cannot list another account's home directory.
func (v restoreView) Path0(r *http.Request, root string) string {
	asked := r.URL.Query().Get("path")
	if asked == "" {
		return root
	}
	clean := path.Clean(asked)
	if clean != root && !strings.HasPrefix(clean, root+"/") {
		return root
	}
	return clean
}

// itemName is what the restore form sends for an entry, which differs by
// kind: a database is a name, a mailbox is domain/box, a file is its path.
func itemName(kind granular.Kind, entry resticrun.Entry, parts reassemble.Parts) string {
	switch kind {
	case granular.KindDatabase:
		if entry.IsDir() || !strings.HasSuffix(entry.Name, ".sql") {
			return ""
		}
		return strings.TrimSuffix(entry.Name, ".sql")
	case granular.KindMailbox:
		mail := path.Join(parts.Homedir, "mail")
		rel := strings.TrimPrefix(entry.Path, mail+"/")
		if rel == entry.Path || rel == "" {
			return ""
		}
		return rel
	default:
		return entry.Path
	}
}

// maildirInternal reports whether a name in ~/mail is part of the
// account's own maildir rather than a domain.
func maildirInternal(name string) bool {
	switch name {
	case "cur", "new", "tmp":
		return true
	}
	return strings.HasPrefix(name, ".")
}

func findSnapshot(snapshots []resticrun.Snapshot, id string) (resticrun.Snapshot, bool) {
	for _, snapshot := range snapshots {
		if snapshot.ID == id || snapshot.ShortID == id {
			return snapshot, true
		}
	}
	return resticrun.Snapshot{}, false
}

// parentWithin is the directory above path, or empty at the root.
func parentWithin(current, root string) string {
	if current == root || current == "" {
		return ""
	}
	parent := path.Dir(current)
	if parent != root && !strings.HasPrefix(parent, root+"/") {
		return root
	}
	return parent
}

// handleRestoreItems queues a granular restore: one mailbox, one database,
// the DNS records. What comes back is left on this server to collect.
func (s *Server) handleRestoreItems(w http.ResponseWriter, r *http.Request) {
	account := r.PostFormValue("account")
	back := "/restore?account=" + url.QueryEscape(account) +
		"&repository=" + url.QueryEscape(r.PostFormValue("repository")) +
		"&item=" + url.QueryEscape(r.PostFormValue("item"))

	restore := nodestore.Restore{
		Account:      account,
		RepositoryID: r.PostFormValue("repository"),
		SnapshotID:   r.PostFormValue("snapshot"),
		Kind:         protocol.RestoreItems,
		ItemKind:     r.PostFormValue("item"),
	}
	for _, name := range r.PostForm["name"] {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			restore.ItemNames = append(restore.ItemNames, trimmed)
		}
	}

	if _, err := s.engine.QueueRestore(restore); err != nil {
		s.redirect(w, r, back, "error", err.Error())
		return
	}
	s.redirect(w, r, back, "ok",
		"Queued. What it recovers is left on this server to collect — "+
			"nothing on the live account is touched.")
}

func (s *Server) handleStartRestore(w http.ResponseWriter, r *http.Request) {
	if r.PostFormValue("account") == cpanel.SystemAccount &&
		strings.TrimSpace(r.PostFormValue("paths")) == "" {
		s.redirect(w, r, "/restore?account="+url.QueryEscape(cpanel.SystemAccount), "error",
			"The server's settings are not an account, so there is no account archive to "+
				"rebuild. Use Restore one thing → Server settings, which puts the files "+
				"somewhere you can read them.")
		return
	}
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
		Restores    []restoreRow
		Destination map[string]string
	}{jobs, collectableRows(restores), names})
}

// --- settings ---

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	settings := s.engine.Settings()
	free := uint64(0)
	if info, err := os.Stat(settings.StagingRoot); err == nil && info.IsDir() {
		free, _ = stagingFree(settings.StagingRoot)
	}
	outputs, err := s.engine.RetainedOutput()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	var held uint64
	for _, output := range outputs {
		held += output.Bytes
	}

	// What a backup contains depends on the payload mode the schedules
	// use, so the page states it rather than describing the default.
	policies, err := s.engine.Store().Policies()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	split, monolithic := 0, 0
	for _, policy := range policies {
		if policy.PayloadMode == string(pkgacct.ModeMonolithic) {
			monolithic++
			continue
		}
		split++
	}

	s.render(w, r, "settings.html", "Settings", "settings", struct {
		Settings    nodestore.Settings
		StagingFree uint64
		Outputs     []staging.Output
		OutputBytes uint64
		KeepDays    int
		Split       int
		Monolithic  int
	}{settings, free, outputs, held, keepDays(settings), split, monolithic})
}

// keepDays is the retention shown in the form, with the default spelt out
// rather than left as a zero the operator has to interpret.
func keepDays(settings nodestore.Settings) int {
	if settings.KeepOutputDays == 0 {
		return nodestore.DefaultKeepOutputDays
	}
	return settings.KeepOutputDays
}

// handleDeleteOutput removes one finished restore's files from the work
// directory, when an operator has finished with them.
func (s *Server) handleDeleteOutput(w http.ResponseWriter, r *http.Request) {
	key := r.PostFormValue("key")
	if err := s.engine.DeleteOutput(key); err != nil {
		s.redirect(w, r, "/settings", "error", err.Error())
		return
	}
	s.redirect(w, r, "/settings", "ok", "Removed. The backups themselves are untouched.")
}

// handleClearOutput empties the work directory of everything collected.
func (s *Server) handleClearOutput(w http.ResponseWriter, r *http.Request) {
	outputs, err := s.engine.RetainedOutput()
	if err != nil {
		s.redirect(w, r, "/settings", "error", err.Error())
		return
	}
	var freed uint64
	for _, output := range outputs {
		if err := s.engine.DeleteOutput(output.Key); err != nil {
			s.redirect(w, r, "/settings", "error", err.Error())
			return
		}
		freed += output.Bytes
	}
	s.redirect(w, r, "/settings", "ok",
		fmt.Sprintf("Removed %s of restored files. The backups themselves are untouched.",
			humanBytes(freed)))
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	settings := s.engine.Settings()
	settings.MaxConcurrent = atoiOr(r.PostFormValue("max_concurrent"), 1)
	settings.ResticBinary = strings.TrimSpace(r.PostFormValue("restic"))
	settings.ResticCACert = strings.TrimSpace(r.PostFormValue("restic_cacert"))
	// A restore's files are kept so they can be collected; this is how
	// long before they are swept. Negative keeps them for ever, which is
	// what this server did before it swept anything at all.
	settings.KeepOutputDays = atoiOr(r.PostFormValue("keep_output_days"),
		nodestore.DefaultKeepOutputDays)
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

// --- fonts ---

// fontFiles is the allowlist. A request names a face, never a path, so
// nothing outside this map can be read however the name is spelt.
var fontFiles = map[string]string{
	"fira-sans-400.woff2": "fonts/fira-sans-400.woff2",
	"fira-sans-500.woff2": "fonts/fira-sans-500.woff2",
	"fira-sans-600.woff2": "fonts/fira-sans-600.woff2",
	"fira-sans-700.woff2": "fonts/fira-sans-700.woff2",
	"fira-code.woff2":     "fonts/fira-code.woff2",
}

// handleFont serves one vendored typeface. Content-Disposition is what
// gets a non-HTML response past cpsrvd intact; a browser ignores it for a
// subresource, and if any of this fails the stylesheet's fallback stack
// renders exactly as it did before the fonts were vendored.
func (s *Server) handleFont(w http.ResponseWriter, r *http.Request) {
	path, ok := fontFiles[r.URL.Query().Get("name")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, err := fontFS.ReadFile(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "font/woff2")
	w.Header().Set("Content-Disposition", `inline; filename="`+filepath.Base(path)+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(body)
}

// --- disaster recovery ---

// recoverView is the page a replacement server starts from.
type recoverView struct {
	// Destinations already attached, so an operator who has done this can
	// see what is in them without doing it again.
	Repositories []recoverRepository
	Hostname     string
	Error        string
}

// recoverRepository is one attached repository and what it holds.
type recoverRepository struct {
	ID       string
	Name     string
	Path     string
	Contents node.Contents
	Err      string
}

func (s *Server) handleRecover(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "recover.html", "Recover a server", "recover",
		s.recoverContents(r.Context(), ""))
}

// recoverContents reads every attached repository so the page can say what
// is actually in them, rather than what this server remembers.
func (s *Server) recoverContents(ctx context.Context, failure string) recoverView {
	view := recoverView{Hostname: s.engine.Settings().Hostname, Error: failure}
	destinations, err := s.destinationViews()
	if err != nil {
		view.Error = err.Error()
		return view
	}
	for _, dest := range destinations {
		if dest.Repository.ID == "" {
			continue
		}
		row := recoverRepository{
			ID:   dest.Repository.ID,
			Name: dest.Name,
			Path: dest.Repository.Path,
		}
		contents, err := s.engine.Contents(ctx, dest.Repository.ID)
		if err != nil {
			row.Err = err.Error()
		} else {
			row.Contents = contents
		}
		view.Repositories = append(view.Repositories, row)
	}
	return view
}

// handleAttach points this server at backups that already exist.
func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	config := map[string]string{}
	secrets := map[string]string{}
	kind := destination.Type(r.PostFormValue("type"))

	switch kind {
	case destination.TypeSFTP:
		config["host"] = strings.TrimSpace(r.PostFormValue("host"))
		config["user"] = strings.TrimSpace(r.PostFormValue("user"))
		config["root"] = strings.TrimSpace(r.PostFormValue("root"))
		config["identity_file"] = strings.TrimSpace(r.PostFormValue("identity_file"))
		config["known_hosts_file"] = strings.TrimSpace(r.PostFormValue("known_hosts_file"))
		if port := strings.TrimSpace(r.PostFormValue("port")); port != "" && port != "22" {
			config["port"] = port
		}
	case destination.TypeREST:
		config["base_url"] = strings.TrimSpace(r.PostFormValue("base_url"))
		secrets["username"] = strings.TrimSpace(r.PostFormValue("username"))
		secrets["password"] = r.PostFormValue("password")
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
		s.render(w, r, "recover.html", "Recover a server", "recover",
			s.recoverContents(r.Context(), "Choose where the old backups are."))
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = "Recovered backups"
	}
	contents, err := s.engine.Attach(r.Context(), node.AttachRequest{
		Destination: nodestore.Destination{
			Name: name, Type: string(kind), Config: config,
		},
		Secrets:        secrets,
		RepositoryPath: strings.TrimSpace(r.PostFormValue("repo_path")),
		Password:       r.PostFormValue("recovery_key"),
	})
	if err != nil {
		s.render(w, r, "recover.html", "Recover a server", "recover",
			s.recoverContents(r.Context(), err.Error()))
		return
	}

	s.redirect(w, r, "/recover", "ok", fmt.Sprintf(
		"Attached. %d backups are readable from here, covering %d account%s.",
		contents.Snapshots, len(contents.Accounts), plural(len(contents.Accounts))))
}

// handleRecoverAccount queues the restore of one account from an attached
// repository, onto this server.
func (s *Server) handleRecoverAccount(w http.ResponseWriter, r *http.Request) {
	account := r.PostFormValue("account")
	apply := r.PostFormValue("apply") != ""
	queued, err := s.engine.QueueRestore(nodestore.Restore{
		Account:      account,
		RepositoryID: r.PostFormValue("repository"),
		Kind:         protocol.RestoreAccount,
		Apply:        apply,
	})
	if err != nil {
		s.redirect(w, r, "/recover", "error", err.Error())
		return
	}
	if queued.Apply {
		s.redirect(w, r, "/recover", "ok", fmt.Sprintf(
			"Restoring %s onto this server. It will be handed to cPanel's own restore when the "+
				"archive is rebuilt.", account))
		return
	}
	s.redirect(w, r, "/recover", "ok", fmt.Sprintf(
		"Rebuilding %s. The archive will be left on this server to inspect; nothing is "+
			"created until you ask for that.", account))
}

// --- browsing what is stored ---

// browseView is one level of what a destination holds: the destinations
// themselves, then the accounts in one, then that account's backups, then
// what is inside one of them.
//
// Each level is fetched when it is asked for. Listing every file of every
// snapshot of nineteen accounts would be minutes of restic and megabytes
// of page for a question nobody asked.
type browseView struct {
	Destinations []destinationView
	Repository   string
	Destination  string
	Account      string
	Accounts     []node.AccountBackups
	SystemBackup bool
	Snapshot     string
	SnapshotAt   time.Time
	Snapshots    []resticrun.Snapshot
	Path         string
	Up           string
	Crumbs       []crumb
	Entries      []browseEntry
	Err          string
}

// crumb is one step of the path back out.
type crumb struct {
	Label string
	URL   string
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	destinations, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	query := r.URL.Query()
	view := browseView{
		Destinations: destinations,
		Repository:   query.Get("repository"),
		Account:      query.Get("account"),
		Snapshot:     query.Get("snapshot"),
		Path:         query.Get("path"),
	}
	for _, dest := range destinations {
		if dest.Repository.ID == view.Repository {
			view.Destination = dest.Name
		}
	}

	switch {
	case view.Repository == "":
		// Nothing chosen: the destinations themselves.
	case view.Snapshot != "":
		s.browseSnapshot(r, &view)
	case view.Account != "":
		s.browseSnapshots(r, &view)
	default:
		s.browseAccounts(r, &view)
	}
	view.Crumbs = browseCrumbs(view)
	s.render(w, r, "browse.html", "Browse backups", "browse", view)
}

// browseAccounts lists what a repository holds, from its snapshots.
func (s *Server) browseAccounts(r *http.Request, view *browseView) {
	contents, err := s.engine.Contents(r.Context(), view.Repository)
	if err != nil {
		view.Err = err.Error()
		return
	}
	view.Accounts = contents.Accounts
	view.SystemBackup = contents.System
}

// browseSnapshots lists one account's backups, newest first.
func (s *Server) browseSnapshots(r *http.Request, view *browseView) {
	snapshots, err := s.engine.Snapshots(r.Context(), view.Repository, view.Account)
	if err != nil {
		view.Err = err.Error()
		return
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Time.After(snapshots[j].Time) })
	view.Snapshots = snapshots
}

// browseSnapshot lists one directory inside one backup.
func (s *Server) browseSnapshot(r *http.Request, view *browseView) {
	snapshots, err := s.engine.Snapshots(r.Context(), view.Repository, view.Account)
	if err != nil {
		view.Err = err.Error()
		return
	}
	snapshot, found := findSnapshot(snapshots, view.Snapshot)
	if !found {
		view.Err = "That backup is not in this destination."
		return
	}
	view.SnapshotAt = snapshot.Time

	// With no path, the snapshot's own roots: the home directory, the
	// databases, the metadata archive — whatever it was told to store.
	if view.Path == "" {
		for _, root := range snapshot.Paths {
			view.Entries = append(view.Entries, browseEntry{
				Name: root, Path: root, Dir: true, Item: root,
			})
		}
		sort.Slice(view.Entries, func(i, j int) bool {
			return view.Entries[i].Name < view.Entries[j].Name
		})
		return
	}

	entries, err := s.engine.Browse(r.Context(), view.Repository, snapshot.ID, view.Path)
	if err != nil {
		view.Err = err.Error()
		return
	}
	view.Up = path.Dir(view.Path)
	if view.Up == view.Path {
		view.Up = ""
	}
	for _, entry := range entries {
		if entry.Path == view.Path {
			continue
		}
		view.Entries = append(view.Entries, browseEntry{
			Name: entry.Name,
			Path: entry.Path,
			Size: entry.Size,
			Dir:  entry.IsDir(),
			Item: entry.Path,
		})
	}
	sort.Slice(view.Entries, func(i, j int) bool {
		if view.Entries[i].Dir != view.Entries[j].Dir {
			return view.Entries[i].Dir
		}
		return view.Entries[i].Name < view.Entries[j].Name
	})
}

// browseCrumbs is the way back out of wherever the operator has got to.
func browseCrumbs(view browseView) []crumb {
	crumbs := []crumb{{Label: "Destinations", URL: "?p=browse"}}
	if view.Repository == "" {
		return crumbs
	}
	name := view.Destination
	if name == "" {
		name = "this destination"
	}
	crumbs = append(crumbs, crumb{
		Label: name,
		URL:   "?p=browse&repository=" + url.QueryEscape(view.Repository),
	})
	if view.Account == "" {
		return crumbs
	}
	crumbs = append(crumbs, crumb{
		Label: view.Account,
		URL: "?p=browse&repository=" + url.QueryEscape(view.Repository) +
			"&account=" + url.QueryEscape(view.Account),
	})
	if view.Snapshot == "" {
		return crumbs
	}
	base := "?p=browse&repository=" + url.QueryEscape(view.Repository) +
		"&account=" + url.QueryEscape(view.Account) +
		"&snapshot=" + url.QueryEscape(view.Snapshot)
	crumbs = append(crumbs, crumb{Label: shortID(view.Snapshot), URL: base})

	if view.Path != "" {
		crumbs = append(crumbs, crumb{Label: view.Path, URL: base + "&path=" + url.QueryEscape(view.Path)})
	}
	return crumbs
}
