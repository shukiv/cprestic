package webui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shuki/cprest/internal/granular"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/protocol"
	"github.com/shuki/cprest/internal/reassemble"
	"github.com/shuki/cprest/internal/resticrun"
)

// accountKey carries the account a request belongs to, taken from the
// socket rather than from anything the browser said.
type accountKey struct{}

// accountOf is whose backups this request may see. Empty means the
// connection could not be attributed, which is refused.
func accountOf(r *http.Request) string {
	account, _ := r.Context().Value(accountKey{}).(string)
	return account
}

// ListenUser serves the account-facing interface.
//
// It is a second socket, not a second port: cpsrvd runs a cPanel user's
// plugin as that user, so the kernel can say who is connecting and this
// program never has to take the browser's word for it. The socket is
// writable by anyone, and every request is then confined to the account
// that opened it.
func (s *Server) ListenUser(ctx context.Context, socketPath string) error {
	// This one has to be reachable by accounts, so it lives in its own
	// directory. The operator's socket is in a different one, at 0700,
	// and neither listener can now change the other's boundary.
	dir := filepath.Dir(socketPath)
	if err := prepareSocketDir(dir, 0o755); err != nil {
		return err
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("webui: remove stale socket: %w", err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("webui: listen on %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o666); err != nil {
		_ = listener.Close()
		return fmt.Errorf("webui: open socket: %w", err)
	}
	// The per-account request limit below starts only after HTTP headers
	// arrive. Bound connections before parsing too, or a local account can
	// open sockets, send nothing, and consume the root service's file
	// descriptors until ReadHeaderTimeout expires them.
	listener = &peerLimitedListener{
		Listener: listener,
		budget: connectionBudget{
			maxTotal:  128,
			maxPerUID: 8,
			byUID:     map[uint32]int{},
		},
	}

	server := &http.Server{
		Handler:           s.UserHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    16 << 10,
		// A body has to arrive as well as start. Without this a local
		// process can open connections and dribble a form into them for
		// as long as it likes, and this service runs as root.
		ReadTimeout:  60 * time.Second,
		IdleTimeout:  60 * time.Second,
		WriteTimeout: 5 * time.Minute,
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			account, err := peerAccount(conn)
			if err != nil {
				s.log.Warn("user interface: unattributed connection", "error", err)
				return ctx
			}
			return context.WithValue(ctx, accountKey{}, account)
		},
	}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	s.log.Info("user interface listening", "socket", socketPath)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// peerLimitedListener admits only a bounded number of Unix connections in
// total and from one UID. Rejected connections are closed before net/http
// allocates a request for them.
type peerLimitedListener struct {
	net.Listener
	budget connectionBudget
}

func (l *peerLimitedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		creds, err := peerCredentials(conn)
		if err != nil || !l.budget.acquire(creds.Uid) {
			_ = conn.Close()
			continue
		}
		return &budgetedConn{
			Conn: conn,
			release: func() {
				l.budget.release(creds.Uid)
			},
		}, nil
	}
}

type budgetedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *budgetedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

type connectionBudget struct {
	mu        sync.Mutex
	total     int
	byUID     map[uint32]int
	maxTotal  int
	maxPerUID int
}

func (b *connectionBudget) acquire(uid uint32) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.byUID == nil {
		b.byUID = map[uint32]int{}
	}
	if b.total >= b.maxTotal || b.byUID[uid] >= b.maxPerUID {
		return false
	}
	b.total++
	b.byUID[uid]++
	return true
}

func (b *connectionBudget) release(uid uint32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.byUID[uid] <= 1 {
		delete(b.byUID, uid)
	} else {
		b.byUID[uid]--
	}
	if b.total > 0 {
		b.total--
	}
}

// UserHandler is what a cPanel account sees: its own backups, and nothing
// else on the server.
func (s *Server) UserHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.userPage(s.handleUserHome))
	mux.HandleFunc("GET /browse", s.userPage(s.handleUserBrowse))
	mux.HandleFunc("POST /restore", s.userPage(s.userGuard(s.handleUserRestore)))
	mux.HandleFunc("GET /download", s.userPage(s.handleUserDownload))

	outer := http.NewServeMux()
	outer.HandleFunc("POST "+capabilityEndpoint, s.issueUserCapability)
	outer.Handle("/", s.requireUserCapability(mux))
	return outer
}

// userPage refuses anything that cannot be attributed to an account, and
// holds one account to one request at a time.
//
// Listing a repository or walking a snapshot runs restic against the
// destination. Without a limit, anything running as the account — a cron
// job, a compromised site — could keep this server and the backup server
// busy on its behalf indefinitely.
func (s *Server) userPage(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account := accountOf(r)
		if account == "" {
			http.Error(w, "cP:Restic could not tell which account this is. "+
				"Open it from inside cPanel.", http.StatusForbidden)
			return
		}
		if !s.userBusy.enter(account) {
			http.Error(w, "cP:Restic is already working on a request for this account. "+
				"Wait for it to finish.", http.StatusTooManyRequests)
			return
		}
		defer s.userBusy.leave(account)
		allowed, err := s.userFeatures.allowed(r.Context(), account)
		if err != nil {
			s.log.Error("account feature check failed", "account", account, "error", err)
			http.Error(w, "cP:Restic could not verify that this feature is enabled. "+
				"Ask your host to check cPanel Feature Manager.", http.StatusServiceUnavailable)
			return
		}
		if !allowed {
			http.Error(w, "cP:Restic is not enabled for this account.", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// inFlight counts what each account has running.
type inFlight struct {
	mu    sync.Mutex
	count map[string]int
	// limit is how many at once. One: a person clicking through a file
	// tree makes one request at a time, and anything making more is not
	// a person.
	limit int
}

func (f *inFlight) enter(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.count == nil {
		f.count = map[string]int{}
	}
	limit := f.limit
	if limit <= 0 {
		limit = 1
	}
	if f.count[key] >= limit {
		return false
	}
	f.count[key]++
	return true
}

func (f *inFlight) leave(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.count[key] <= 1 {
		delete(f.count, key)
		return
	}
	f.count[key]--
}

// userView is one account's own page.
type userView struct {
	Account      string
	Repositories []userRepository
	Restores     []restoreRow
	Kinds        []granular.Kind
	Err          string
	TotalPoints  int
	Ready        int
	Latest       time.Time

	// The browser, when one is open.
	Repository string
	Snapshot   string
	SnapshotAt time.Time
	Snapshots  []resticrun.Snapshot
	Path       string
	Up         string
	Entries    []browseEntry
	Kind       granular.Kind
}

// userRepository is one destination as an account sees it: how many
// backups of theirs are in it, and when the last one was taken.
type userRepository struct {
	ID        string
	Name      string
	Snapshots int
	Latest    time.Time
}

// userKindAccount is a UI choice rather than a granular plan: it rebuilds
// the complete cpmove archive, still as a download and never as an automatic
// overwrite of the live account.
const userKindAccount granular.Kind = "account"

// KindTitle names account recovery choices in customer-facing language.
func (v userView) KindTitle(k granular.Kind) string {
	switch k {
	case userKindAccount:
		return "Full account"
	case granular.KindFiles:
		return "Home directory"
	case granular.KindMailbox:
		return "Email accounts"
	case granular.KindDatabase:
		return "Databases"
	case granular.KindDBUsers:
		return "Database users"
	}
	return k.Title()
}

// KindDescription gives each recovery choice enough context to be selected
// without opening it first.
func (v userView) KindDescription(k granular.Kind) string {
	switch k {
	case userKindAccount:
		return "Everything in this backup as one cPanel account archive."
	case granular.KindFiles:
		return "The whole home directory, or only selected files and folders."
	case granular.KindCron:
		return "The account's scheduled cron jobs."
	case granular.KindDatabase:
		return "Choose one or more MySQL or MariaDB databases."
	case granular.KindDBUsers:
		return "Database users, authentication data, and grants."
	case granular.KindDomains:
		return "Domains, DNS zones, and their web-server configuration."
	case granular.KindDNS:
		return "DNS zone records for the account's domains."
	case granular.KindSSL:
		return "Certificates, private keys, and AutoSSL metadata."
	case granular.KindMailbox:
		return "Choose a mailbox or all mail for a domain."
	case granular.KindFTP:
		return "The account's FTP users and access configuration."
	}
	return "Recover this part of the account."
}

func (v userView) Selected(k granular.Kind) bool { return v.Kind == k }
func (v userView) NeedsNames() bool              { return v.Kind.NeedsNames() }
func (v userView) RestoreTitle(row restoreRow) string {
	if row.ItemKind != "" {
		return v.KindTitle(granular.Kind(row.ItemKind))
	}
	if row.Kind == protocol.RestoreAccount {
		return v.KindTitle(userKindAccount)
	}
	return "Recovery package"
}

func (s *Server) handleUserHome(w http.ResponseWriter, r *http.Request) {
	view := userView{Account: accountOf(r), Kinds: userKinds}
	destinations, err := s.destinationViews()
	if err != nil {
		s.failUser(w, r, http.StatusInternalServerError, err)
		return
	}
	for _, dest := range destinations {
		if dest.Repository.ID == "" {
			continue
		}
		row := userRepository{ID: dest.Repository.ID, Name: dest.Name}
		snapshots, err := s.engine.UserSnapshots(r.Context(), dest.Repository.ID, view.Account)
		if err != nil {
			s.log.Error("list account snapshots", "account", view.Account,
				"repository", dest.Repository.ID, "error", err)
			view.Err = "One of your backup destinations could not be read. Ask your host to check it."
		}
		row.Snapshots = len(snapshots)
		view.TotalPoints += len(snapshots)
		for _, snapshot := range snapshots {
			if snapshot.Time.After(row.Latest) {
				row.Latest = snapshot.Time
			}
			if snapshot.Time.After(view.Latest) {
				view.Latest = snapshot.Time
			}
		}
		view.Repositories = append(view.Repositories, row)
	}

	// Only this account's own restores, and only the ones with something
	// to collect.
	restores, err := s.engine.Store().Restores(50)
	if err != nil {
		s.failUser(w, r, http.StatusInternalServerError, err)
		return
	}
	for _, restore := range restores {
		if restore.Account != view.Account {
			continue
		}
		view.Restores = append(view.Restores, restoreRow{
			Restore:     accountSafeRestore(restore),
			Collectable: restore.ArchivePath != "" && onDisk(restore.ArchivePath),
		})
		if restore.ArchivePath != "" && onDisk(restore.ArchivePath) {
			view.Ready++
		}
	}
	s.renderUser(w, r, "user_home.html", view)
}

// userKinds are the parts of their own account a customer can ask for.
// The server's own settings are not among them: they are not this
// account's, and no account may see them.
var userKinds = []granular.Kind{
	userKindAccount,
	granular.KindFiles,
	granular.KindCron,
	granular.KindDatabase,
	granular.KindDBUsers,
	granular.KindDomains,
	granular.KindDNS,
	granular.KindSSL,
	granular.KindMailbox,
	granular.KindFTP,
}

func (s *Server) handleUserBrowse(w http.ResponseWriter, r *http.Request) {
	view := userView{
		Account:    accountOf(r),
		Kinds:      userKinds,
		Repository: r.URL.Query().Get("repository"),
		Snapshot:   r.URL.Query().Get("snapshot"),
		Path:       r.URL.Query().Get("path"),
		Kind:       granular.Kind(r.URL.Query().Get("item")),
	}
	if view.Repository == "" {
		destinations, err := s.destinationViews()
		if err != nil {
			s.failUser(w, r, http.StatusInternalServerError, err)
			return
		}
		for _, destination := range destinations {
			if destination.Repository.ID != "" && destination.Repository.InitialisedAt != nil {
				view.Repository = destination.Repository.ID
				break
			}
		}
	}
	if view.Repository == "" {
		view.Err = "No backup destination is ready for your account yet."
		s.renderUser(w, r, "user_browse.html", view)
		return
	}

	snapshots, err := s.engine.UserSnapshots(r.Context(), view.Repository, view.Account)
	if err != nil {
		s.log.Error("browse account snapshots", "account", view.Account,
			"repository", view.Repository, "error", err)
		view.Err = "This backup destination could not be read. Ask your host to check it."
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Time.After(snapshots[j].Time) })
	view.Snapshots = snapshots
	if len(snapshots) == 0 {
		view.Err = "No restore points are available for your account in this destination yet."
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	if view.Snapshot == "" && len(snapshots) > 0 {
		view.Snapshot = snapshots[0].ID
	}

	snapshot, found := findSnapshot(snapshots, view.Snapshot)
	if !found {
		view.Err = "That backup is not in this destination."
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	view.SnapshotAt = snapshot.Time
	if view.Kind == "" {
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	if !isUserKind(view.Kind) {
		view.Err = "That recovery option is not available for your account."
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	// Full-account and metadata-only choices have no child item to pick.
	if view.Kind == userKindAccount || !view.Kind.NeedsNames() {
		s.renderUser(w, r, "user_browse.html", view)
		return
	}

	parts, err := reassemble.Classify(snapshot.Paths)
	if err != nil {
		s.log.Error("classify account snapshot", "account", view.Account,
			"repository", view.Repository, "snapshot", snapshot.ID, "error", err)
		view.Err = "This backup has an unexpected layout. Ask your host to check it."
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	root := userPickerRoot(view.Kind, parts)
	if root == "" {
		view.Err = "This backup does not contain that part of your account."
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	view.Path = root
	if asked := path.Clean(r.URL.Query().Get("path")); asked != "." && asked != "" {
		if asked == root || strings.HasPrefix(asked, root+"/") {
			view.Path = asked
		}
	}
	view.Up = parentWithin(view.Path, root)

	entries, err := s.engine.Browse(r.Context(), view.Repository, snapshot.ID, view.Path)
	if err != nil {
		s.log.Error("browse account backup", "account", view.Account,
			"repository", view.Repository, "snapshot", snapshot.ID, "error", err)
		view.Err = "That part of the backup could not be read. Ask your host to check it."
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	if view.Kind == granular.KindMailbox && view.Path == root {
		view.Entries = append(view.Entries, browseEntry{
			Name: "The account mailbox and all addresses",
			Path: root, Item: ".",
		})
	}
	for _, entry := range entries {
		if entry.Path == view.Path {
			continue
		}
		if view.Kind == granular.KindMailbox && view.Path == root &&
			(!entry.IsDir() || maildirInternal(entry.Name)) {
			continue
		}
		item := itemName(view.Kind, entry, parts)
		if view.Kind == granular.KindDatabase && item == "" {
			continue
		}
		view.Entries = append(view.Entries, browseEntry{
			Name: entry.Name, Path: entry.Path, Size: entry.Size,
			Dir: entry.IsDir(), Item: item,
		})
	}
	sort.Slice(view.Entries, func(i, j int) bool {
		if view.Entries[i].Dir != view.Entries[j].Dir {
			return view.Entries[i].Dir
		}
		return view.Entries[i].Name < view.Entries[j].Name
	})
	s.renderUser(w, r, "user_browse.html", view)
}

func userPickerRoot(kind granular.Kind, parts reassemble.Parts) string {
	switch kind {
	case granular.KindFiles:
		return parts.Homedir
	case granular.KindMailbox:
		if parts.Homedir == "" {
			return ""
		}
		return path.Join(parts.Homedir, "mail")
	case granular.KindDatabase:
		return parts.Databases
	default:
		return ""
	}
}

// handleUserRestore queues a restore of part of the account that asked for
// it. The account comes from the socket, never from the form, so a request
// cannot be pointed at anybody else.
func (s *Server) handleUserRestore(w http.ResponseWriter, r *http.Request) {
	account := accountOf(r)
	// What an account may ask for is the list the page offers, checked
	// here rather than only rendered there. Raw panel settings are not a
	// customer-facing granular choice: that subset carries shadow,
	// digestshadow and cPanel internals without the context of a complete
	// cPanel account archive. A complete archive is handled explicitly.
	asked := granular.Kind(r.PostFormValue("item"))
	restore, err := userRestoreRequest(account, r.PostFormValue("repository"),
		r.PostFormValue("snapshot"), asked, r.PostForm["name"])
	if err != nil {
		if errors.Is(err, errUserRestoreNeedsNames) {
			back := "/browse?repository=" + url.QueryEscape(r.PostFormValue("repository")) +
				"&snapshot=" + url.QueryEscape(r.PostFormValue("snapshot")) +
				"&item=" + url.QueryEscape(string(asked))
			redirectUser(w, back, "error", "Choose at least one item to recover.")
			return
		}
		redirectUser(w, "/", "error", "That is not something you can restore here.")
		return
	}

	// The backup has to be one of theirs. A name that has changed hands
	// still has the previous owner's snapshots in the repository, and
	// hiding them from the page while restoring them on request would be
	// no protection at all.
	if err := s.engine.OwnsSnapshot(r.Context(), restore.RepositoryID, account,
		restore.SnapshotID); err != nil {
		redirectUser(w, "/", "error", "That backup is not one of yours.")
		return
	}
	if _, err := s.engine.QueueRestore(restore); err != nil {
		s.log.Error("queue account restore", "account", account, "error", err)
		message := "The restore could not be started. Ask your host to check it."
		if strings.Contains(err.Error(), "already has work in flight") {
			message = "A backup or restore is already running for your account. Wait for it to finish."
		}
		redirectUser(w, "/", "error", message)
		return
	}
	redirectUser(w, "/", "ok",
		"Started. What it recovers will appear below to download; nothing on your account "+
			"is changed.")
}

// redirectUser keeps account-side navigation in cPanel's .live.php entry
// points. The WHM interface uses a single CGI and a ?p= route; using that
// redirect here would send a GET to restore.live.php after a POST, which is
// not a valid account capability and strands the user on a 403 page.
func redirectUser(w http.ResponseWriter, route, kind, message string) {
	route = strings.TrimPrefix(route, "/")
	query := url.Values{}
	if base, extra, found := strings.Cut(route, "?"); found {
		route = base
		if parsed, err := url.ParseQuery(extra); err == nil {
			query = parsed
		}
	}
	entry := "index.live.php"
	if route == "browse" {
		entry = "browse.live.php"
	}
	query.Set("kind", kind)
	query.Set("msg", message)
	w.Header().Set("Location", entry+"?"+query.Encode())
	w.WriteHeader(http.StatusSeeOther)
}

var errUserRestoreNeedsNames = errors.New("account recovery needs at least one item")

// userRestoreRequest converts the account page's narrow vocabulary into an
// engine request. In particular, "account" is the full archive flow rather
// than an unrecognised granular kind, and no account-side request can turn on
// Apply: recovery remains a downloadable copy until an operator takes an
// explicit action outside this interface.
func userRestoreRequest(account, repository, snapshot string, asked granular.Kind, names []string) (nodestore.Restore, error) {
	if !isUserKind(asked) {
		return nodestore.Restore{}, fmt.Errorf("account recovery kind %q is not allowed", asked)
	}
	restore := nodestore.Restore{
		Account: account, RepositoryID: repository, SnapshotID: snapshot,
	}
	if asked == userKindAccount {
		restore.Kind = protocol.RestoreAccount
		return restore, nil
	}
	restore.Kind = protocol.RestoreItems
	restore.ItemKind = string(asked)
	for _, name := range names {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			restore.ItemNames = append(restore.ItemNames, trimmed)
		}
	}
	if asked.NeedsNames() && len(restore.ItemNames) == 0 {
		return nodestore.Restore{}, errUserRestoreNeedsNames
	}
	return restore, nil
}

// accountSafeRestore removes root-side diagnostics before a restore record
// is rendered to a customer. WHM retains the original record and the logs
// retain its error; repository addresses and staging paths do not belong in
// an account-facing page.
func accountSafeRestore(restore nodestore.Restore) nodestore.Restore {
	restore.ArchivePath = ""
	restore.RestoredTo = ""
	if restore.Error == "" {
		return restore
	}
	restore.Error = "The restore failed. Ask your host to check the backup service."
	restore.Detail = ""
	return restore
}

// handleUserDownload hands over a restore this account asked for.
// isUserKind reports whether a customer may ask for this part of their
// own account.
func isUserKind(kind granular.Kind) bool {
	for _, offered := range userKinds {
		if offered == kind {
			return true
		}
	}
	return false
}

func (s *Server) handleUserDownload(w http.ResponseWriter, r *http.Request) {
	restore, err := s.engine.Store().Restore(r.URL.Query().Get("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if restore.Account != accountOf(r) {
		// Not theirs. Say nothing about whose it is.
		http.NotFound(w, r)
		return
	}
	s.handleDownload(w, r)
}
