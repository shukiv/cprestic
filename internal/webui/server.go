// Package webui serves the standalone node's interface.
//
// It listens on a unix domain socket rather than a TCP port. cPanel servers
// are multi-tenant, the untrusted users are already on the box, and this
// interface can read every destination credential the server holds — a
// loopback port would be reachable by all of them. A WHM plugin CGI, which
// runs as root, proxies to the socket.
// See docs/adr/0007-standalone-mode.md.
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/shuki/cprest/internal/node"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// The typefaces the interface is set in travel with the plugin. Asking a
// font host for them would tell a third party whenever a root WHM session
// is open, and would let one serve styling into a privileged page.
//
//go:embed fonts/*.woff2
var fontFS embed.FS

// Server renders the node's interface.
type Server struct {
	engine *node.Engine
	log    *slog.Logger
	// templates holds one set per page. Every page defines "content", so
	// parsing them into a single set would leave only the last one.
	templates map[string]*template.Template
	// csrfToken is minted once per process and embedded in every mutating
	// form. There are no sessions here — WHM has already authenticated the
	// operator — but a token still stops another page in the browser
	// posting to this one.
	csrfToken string
	// assets are inlined into every page. cpsrvd strips Content-Type from
	// what the plugin proxies back and sets X-Content-Type-Options:
	// nosniff, so a stylesheet fetched as its own request arrives with no
	// type and the browser refuses to apply it. Inlining sidesteps that,
	// and saves two round trips through the CGI besides.
	assets assets
}

// assets is the stylesheet and script, embedded in the page.
type assets struct {
	CSS template.CSS
	JS  template.JS
}

// New builds the interface.
func New(engine *node.Engine, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	templates, err := parseTemplates()
	if err != nil {
		return nil, err
	}

	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		return nil, fmt.Errorf("webui: read stylesheet: %w", err)
	}
	script, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		return nil, fmt.Errorf("webui: read script: %w", err)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("webui: generate csrf token: %w", err)
	}
	return &Server{
		engine: engine, log: log, templates: templates,
		csrfToken: hex.EncodeToString(raw),
		assets:    assets{CSS: template.CSS(css), JS: template.JS(script)},
	}, nil
}

// parseTemplates builds one template set per page, each containing the
// layout, the shared fragments and that page's own content.
func parseTemplates() (map[string]*template.Template, error) {
	pages, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("webui: list templates: %w", err)
	}

	sets := map[string]*template.Template{}
	for _, page := range pages {
		name := path.Base(page)
		if name == "layout.html" || name == "partials.html" {
			continue
		}
		set, err := template.New(name).Funcs(templateFuncs()).ParseFS(templateFS,
			"templates/layout.html", "templates/partials.html", page)
		if err != nil {
			return nil, fmt.Errorf("webui: parse %s: %w", name, err)
		}
		sets[name] = set
	}
	return sets, nil
}

// Handler returns the routed interface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(staticFS))

	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /destinations", s.handleDestinations)
	mux.HandleFunc("POST /destinations/add", s.guard(s.handleAddDestination))
	mux.HandleFunc("POST /destinations/edit", s.guard(s.handleEditDestination))
	mux.HandleFunc("POST /destinations/test", s.guard(s.handleTestDestination))
	mux.HandleFunc("POST /destinations/delete", s.guard(s.handleDeleteDestination))

	mux.HandleFunc("GET /schedule", s.handleSchedule)
	mux.HandleFunc("POST /schedule/save", s.guard(s.handleSaveSchedule))
	mux.HandleFunc("POST /schedule/run", s.guard(s.handleRunSchedule))
	mux.HandleFunc("POST /schedule/delete", s.guard(s.handleDeleteSchedule))

	mux.HandleFunc("GET /accounts", s.handleAccounts)
	mux.HandleFunc("GET /account", s.handleAccount)
	mux.HandleFunc("POST /accounts/backup", s.guard(s.handleBackupNow))
	mux.HandleFunc("POST /accounts/download", s.guard(s.handleDownloadRequest))
	mux.HandleFunc("POST /accounts/verify", s.guard(s.handleVerifyRequest))
	mux.HandleFunc("GET /download", s.handleDownload)
	mux.HandleFunc("GET /font", s.handleFont)

	mux.HandleFunc("GET /restore", s.handleRestore)
	mux.HandleFunc("POST /restore/start", s.guard(s.handleStartRestore))

	mux.HandleFunc("GET /jobs", s.handleJobs)
	mux.HandleFunc("GET /settings", s.handleSettings)
	mux.HandleFunc("POST /settings/save", s.guard(s.handleSaveSettings))

	return s.recoverPanics(s.route(mux))
}

// route turns the "p" query parameter into a request path.
//
// cpsrvd will not route anything after a CGI's name — both
// ".../cprest.cgi/" and ".../cprest.cgi/accounts" are 404s, verified
// against cPanel 136 — so every route travels in a query parameter. It is
// translated here rather than in the plugin so the rule is one thing, in
// the language the rest of this package is written in, and can be tested.
// See docs/adr/0008.
func (s *Server) route(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if _, given := query["p"]; !given {
			next.ServeHTTP(w, r)
			return
		}

		route := strings.TrimPrefix(query.Get("p"), "/")
		query.Del("p")
		if !safeRoute(route) {
			s.fail(w, r, http.StatusBadRequest, errors.New("that address is not part of cprest"))
			return
		}

		r.URL.Path = "/" + route
		r.URL.RawQuery = query.Encode()
		next.ServeHTTP(w, r)
	})
}

// safeRoute accepts only the shapes this interface serves, so a crafted "p"
// cannot be used to reach somewhere else.
func safeRoute(route string) bool {
	if route == "" {
		return true
	}
	if strings.Contains(route, "..") || strings.Contains(route, "//") {
		return false
	}
	for _, r := range route {
		isAllowed := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '/' || r == '-' || r == '_' || r == '.'
		if !isAllowed {
			return false
		}
	}
	return true
}

// Listen serves the interface on a unix socket.
//
// The socket sits in an owner-only directory and is itself owner-only, so
// an unprivileged user on a shared server cannot reach it.
func (s *Server) Listen(ctx context.Context, socketPath string) error {
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("webui: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("webui: secure %s: %w", dir, err)
	}
	// A socket left by a killed process would block the listen.
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("webui: remove stale socket: %w", err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("webui: listen on %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		return fmt.Errorf("webui: secure socket: %w", err)
	}

	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Browsing a snapshot or listing a remote repository waits on
		// restic, which waits on the network.
		WriteTimeout: 5 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	s.log.Info("interface listening", "socket", socketPath)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// guard rejects a mutating request that does not carry this process's CSRF
// token.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			s.fail(w, r, http.StatusBadRequest, fmt.Errorf("unreadable form: %w", err))
			return
		}
		supplied := r.PostFormValue("csrf")
		if subtle.ConstantTimeCompare([]byte(supplied), []byte(s.csrfToken)) != 1 {
			// Usually a stale page after a restart, occasionally something
			// worse. Either way the operator should reload and retry.
			s.fail(w, r, http.StatusForbidden,
				errors.New("this page expired when the service restarted; reload and try again"))
			return
		}
		next(w, r)
	}
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.Error("panic serving request",
					"path", r.URL.Path, "panic", recovered)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
