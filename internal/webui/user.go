package webui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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

	server := &http.Server{
		Handler:           s.UserHandler(),
		ReadHeaderTimeout: 10 * time.Second,
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

// UserHandler is what a cPanel account sees: its own backups, and nothing
// else on the server.
func (s *Server) UserHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.userPage(s.handleUserHome))
	mux.HandleFunc("GET /browse", s.userPage(s.handleUserBrowse))
	mux.HandleFunc("POST /restore", s.userPage(s.guard(s.handleUserRestore)))
	mux.HandleFunc("GET /download", s.userPage(s.handleUserDownload))
	return mux
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

// KindTitle lets the template name a kind.
func (v userView) KindTitle(k granular.Kind) string { return k.Title() }

func (s *Server) handleUserHome(w http.ResponseWriter, r *http.Request) {
	view := userView{Account: accountOf(r), Kinds: userKinds}
	destinations, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	for _, dest := range destinations {
		if dest.Repository.ID == "" {
			continue
		}
		row := userRepository{ID: dest.Repository.ID, Name: dest.Name}
		snapshots, err := s.engine.Snapshots(r.Context(), dest.Repository.ID, view.Account)
		if err != nil {
			view.Err = err.Error()
		}
		row.Snapshots = len(snapshots)
		for _, snapshot := range snapshots {
			if snapshot.Time.After(row.Latest) {
				row.Latest = snapshot.Time
			}
		}
		view.Repositories = append(view.Repositories, row)
	}

	// Only this account's own restores, and only the ones with something
	// to collect.
	restores, err := s.engine.Store().Restores(50)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	for _, restore := range restores {
		if restore.Account != view.Account {
			continue
		}
		view.Restores = append(view.Restores, restoreRow{
			Restore:     restore,
			Collectable: restore.ArchivePath != "" && onDisk(restore.ArchivePath),
		})
	}
	s.renderUser(w, r, "user_home.html", view)
}

// userKinds are the parts of their own account a customer can ask for.
// The server's own settings are not among them: they are not this
// account's, and no account may see them.
var userKinds = []granular.Kind{
	granular.KindFiles, granular.KindWebsite, granular.KindMailbox,
	granular.KindDatabase, granular.KindDBUsers, granular.KindDNS,
	granular.KindSSL, granular.KindCron, granular.KindFTP,
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

	snapshots, err := s.engine.Snapshots(r.Context(), view.Repository, view.Account)
	if err != nil {
		view.Err = err.Error()
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Time.After(snapshots[j].Time) })
	view.Snapshots = snapshots
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

	parts, err := reassemble.Classify(snapshot.Paths)
	if err != nil {
		view.Err = err.Error()
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	// A customer browses their own files. Everything else they can ask
	// for is a whole item, chosen on the page rather than walked into.
	root := parts.Homedir
	if root == "" {
		view.Err = "This backup holds no files to look through."
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	view.Path = root
	if asked := filepath.Clean(r.URL.Query().Get("path")); asked != "." && asked != "" {
		if asked == root || len(asked) > len(root) && asked[:len(root)+1] == root+"/" {
			view.Path = asked
		}
	}
	view.Up = parentWithin(view.Path, root)

	entries, err := s.engine.Browse(r.Context(), view.Repository, snapshot.ID, view.Path)
	if err != nil {
		view.Err = err.Error()
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	for _, entry := range entries {
		if entry.Path == view.Path {
			continue
		}
		view.Entries = append(view.Entries, browseEntry{
			Name: entry.Name, Path: entry.Path, Size: entry.Size,
			Dir: entry.IsDir(), Item: entry.Path,
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

// handleUserRestore queues a restore of part of the account that asked for
// it. The account comes from the socket, never from the form, so a request
// cannot be pointed at anybody else.
func (s *Server) handleUserRestore(w http.ResponseWriter, r *http.Request) {
	account := accountOf(r)
	restore := nodestore.Restore{
		Account:      account,
		RepositoryID: r.PostFormValue("repository"),
		SnapshotID:   r.PostFormValue("snapshot"),
		Kind:         protocol.RestoreItems,
	}
	// What an account may ask for is the list the page offers, checked
	// here rather than only rendered there. The parts of an account this
	// list leaves out are the ones nobody may pull out of a backup on
	// their own: the settings archive carries shadow, digestshadow and
	// cPanel's own metadata for the account.
	asked := granular.Kind(r.PostFormValue("item"))
	if !isUserKind(asked) {
		s.redirect(w, r, "/", "error", "That is not something you can restore here.")
		return
	}
	restore.ItemKind = string(asked)
	for _, name := range r.PostForm["name"] {
		if trimmed := name; trimmed != "" {
			restore.ItemNames = append(restore.ItemNames, trimmed)
		}
	}
	if _, err := s.engine.QueueRestore(restore); err != nil {
		s.redirect(w, r, "/", "error", err.Error())
		return
	}
	s.redirect(w, r, "/", "ok",
		"Started. What it recovers will appear below to download; nothing on your account "+
			"is changed.")
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
