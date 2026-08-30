// Command cprest.cgi is the WHM plugin: a thin proxy between WHM and the
// cprest interface listening on a unix socket.
//
// It exists so that no TCP port on a shared cPanel server carries an
// interface that can read every destination credential. Everything it does
// is forward a request and copy back the response.
// See docs/adr/0007-standalone-mode.md.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

// socketPath is where the standalone node listens. It is a compile-time
// constant rather than a setting so that a writable config file cannot
// redirect the plugin at something else.
const socketPath = "/var/run/cprest/ui.sock"

func main() {
	// WHM authenticates the operator and tells us who they are. cprest can
	// read and delete every backup on this server, so it is root's tool
	// only — a reseller with a WHM login must never reach it.
	//
	// The AppConfig registration also restricts this, but that is a file
	// on disk that has not been verified against a live WHM. This check is
	// the one enforced here, in code, on every request.
	if user := whmUser(); user != "root" {
		deny(fmt.Sprintf("cprest is available to the root WHM account only (you are %q).", user))
		return
	}

	// The socket speaks plain HTTP; the host in the URL is a formality.
	target, _ := url.Parse("http://cprest")
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			request.Out.Host = target.Host
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				dialer := net.Dialer{Timeout: 5 * time.Second}
				return dialer.DialContext(ctx, "unix", socketPath)
			},
			ResponseHeaderTimeout: 5 * time.Minute,
		},
		// The interface emits query-only redirects so WHM's session token
		// survives. Anything that still comes back as an absolute path is
		// rewritten here, because such a path would drop the token.
		ModifyResponse: func(resp *http.Response) error {
			location := resp.Header.Get("Location")
			if location == "" || strings.HasPrefix(location, "?") {
				return nil
			}
			parsed, err := url.Parse(location)
			if err != nil || parsed.Scheme != "" || parsed.Host != "" {
				return nil
			}
			query := parsed.Query()
			if route := strings.TrimPrefix(parsed.Path, "/"); route != "" {
				query.Set("p", route)
			}
			resp.Header.Set("Location", "?"+query.Encode())
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, page, "cprest could not be reached: "+htmlEscape(err.Error()))
		},
	}

	if err := cgi.Serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// cpsrvd will not route anything after the script name: both
		// ".../cprest.cgi/" and ".../cprest.cgi/destinations" are 404s,
		// verified against cPanel 136. Every route therefore travels in
		// the "p" query parameter and is turned back into a path here.
		query := r.URL.Query()
		route := strings.TrimPrefix(query.Get("p"), "/")
		query.Del("p")

		if !safeRoute(route) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, page, "That address is not part of cprest.")
			return
		}
		r.URL.Path = "/" + route
		r.URL.RawQuery = query.Encode()

		// Checked here rather than at startup so the operator sees this at
		// the address their links actually use.
		if _, err := os.Stat(socketPath); err != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, page, "The cprest service is not running on this server. "+
				"Start it with: systemctl start cprest")
			return
		}
		proxy.ServeHTTP(w, r)
	})); err != nil {
		deny("cprest plugin failed: " + err.Error())
	}
}

// safeRoute accepts only the shapes the interface serves, so a crafted "p"
// cannot steer the proxy somewhere else.
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

// whmUser reports the WHM account that made the request. WHM sets
// REMOTE_USER; the others are fallbacks seen across versions.
func whmUser() string {
	for _, name := range []string{"REMOTE_USER", "WHM_USER", "USER"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func deny(message string) {
	_, _ = io.WriteString(os.Stdout,
		"Content-Type: text/html; charset=utf-8\r\nStatus: 403 Forbidden\r\n\r\n")
	fmt.Fprintf(os.Stdout, page, htmlEscape(message))
}

const page = `<!doctype html><html><head><meta charset="utf-8"><title>cprest</title></head>
<body style="font:14px system-ui,sans-serif;padding:2rem"><h1>cprest backups</h1><p>%s</p></body></html>`

func htmlEscape(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return replacer.Replace(s)
}
