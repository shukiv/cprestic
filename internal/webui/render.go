package webui

import (
	"bytes"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// page is the data every template receives.
type page struct {
	Title  string
	Nav    string
	CSRF   string
	Flash  *flash
	Data   any
	Assets assets
}

// flash is a one-shot message shown at the top of a page.
type flash struct {
	Kind    string // ok, warn, error
	Message string
}

// render writes a page, or an error page if the template fails.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name, title, nav string, data any) {
	view := page{
		Title: title, Nav: nav, CSRF: s.csrfToken,
		Flash: flashFrom(r), Data: data, Assets: s.assets,
	}

	// Rendered to a buffer first: a template that fails halfway through
	// would otherwise leave a half-written page with a 200 on it.
	set, known := s.templates[name]
	if !known {
		s.log.Error("no such template", "template", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var buffer bytes.Buffer
	if err := set.ExecuteTemplate(&buffer, "layout", view); err != nil {
		s.log.Error("render template", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The interface is served through WHM; nothing here should be framed
	// by anything else or sniffed into another type.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	_, _ = buffer.WriteTo(w)
}

// redirect sends the operator back to a page with a message.
//
// The target is a bare query string, so the browser resolves it against the
// current script. WHM serves the plugin behind a per-session token in the
// path, and cpsrvd will not route anything after the .cgi name — so every
// route travels in "p" rather than in the path. See docs/adr/0008.
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, path, kind, message string) {
	route := strings.TrimPrefix(path, "/")

	// A caller may pass "restore?account=x"; keep those parameters.
	query := url.Values{}
	if base, extra, found := strings.Cut(route, "?"); found {
		route = base
		if parsed, err := url.ParseQuery(extra); err == nil {
			query = parsed
		}
	}
	query.Set("p", route)
	query.Set("kind", kind)
	query.Set("msg", message)

	// Written directly rather than through http.Redirect, which resolves a
	// query-only reference against the request path and emits an absolute
	// one. WHM serves the plugin behind a /cpsessNNN token in the path, so
	// an absolute path drops the token and the browser lands on a 401.
	w.Header().Set("Location", "?"+query.Encode())
	w.WriteHeader(http.StatusSeeOther)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, err error) {
	s.log.Error("request failed", "path", r.URL.Path, "error", err)
	w.WriteHeader(status)
	s.render(w, r, "error.html", "Problem", "", err.Error())
}

func flashFrom(r *http.Request) *flash {
	message := r.URL.Query().Get("msg")
	if message == "" {
		return nil
	}
	kind := r.URL.Query().Get("kind")
	if kind != "ok" && kind != "warn" && kind != "error" {
		kind = "ok"
	}
	return &flash{Kind: kind, Message: message}
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"bytes": humanBytes,
		// barwidth is the CSS for a progress bar's filled part. It is
		// built here rather than interpolated in the template so the
		// value is a number this program produced, not markup.
		"barwidth": func(percent float64) template.CSS {
			switch {
			case percent < 0:
				percent = 0
			case percent > 100:
				percent = 100
			}
			return template.CSS(fmt.Sprintf("width:%.1f%%", percent))
		},
		"percent": func(value float64) string {
			// Half rounds up: 42.5% reads as 43%, not 42%, which is what
			// anyone watching a bar expects of the number beside it.
			return fmt.Sprintf("%.0f%%", math.Round(value))
		},
		"ago": humanAgo,
		"stamp": func(t time.Time) string {
			if t.IsZero() {
				return "never"
			}
			return t.Local().Format("2006-01-02 15:04")
		},
		"stampPtr": func(t *time.Time) string {
			if t == nil {
				return "—"
			}
			return t.Local().Format("2006-01-02 15:04")
		},
		"short": func(s string) string {
			if len(s) > 12 {
				return s[:12]
			}
			return s
		},
		"join":  strings.Join,
		"lower": strings.ToLower,
		"title": strings.Title, //nolint:staticcheck // ASCII labels only
	}
}

func humanBytes(value uint64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	size, exponent := float64(value), 0
	for size >= unit && exponent < 4 {
		size /= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", size, "KMGT"[exponent-1])
}

func humanAgo(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	elapsed := time.Since(*t)
	switch {
	case elapsed < time.Minute:
		return "just now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%d min ago", int(elapsed.Minutes()))
	case elapsed < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(elapsed.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(elapsed.Hours()/24))
	}
}
