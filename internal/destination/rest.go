package destination

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// REST is a restic rest-server endpoint. This is the recommended
// destination type for storage we control, because rest-server supports
// append-only mode: an attacker holding root on a source server can add
// snapshots but cannot delete or rewrite history. See docs/DESIGN.md §8.
type REST struct {
	// BaseURL is the server root, e.g. "https://backup.example.com".
	BaseURL string
	// Username and Password are HTTP basic-auth credentials from the
	// vault. With rest-server --private-repos, Username also determines
	// which subdirectory the credentials can reach.
	Username string
	Password string
	// AppendOnly records that the server enforces --append-only for these
	// credentials. Retention and pruning must then run from the
	// maintenance runner with a separate delete-capable credential.
	AppendOnly bool
	// CABundle is a PEM file trusted in addition to the system roots, for
	// a rest-server behind a private CA. restic is told about it
	// separately, through the runner's --cacert.
	CABundle string
	// HTTPClient is used by Preflight. Nil means a client with a short timeout.
	HTTPClient *http.Client
}

var _ Destination = (*REST)(nil)

func (r *REST) Type() Type { return TypeREST }

func (r *REST) URI(repoPath string) (string, error) {
	base, err := r.baseURL()
	if err != nil {
		return "", err
	}
	cleaned, err := CleanRepoPath(repoPath)
	if err != nil {
		return "", err
	}
	// Credentials belong in the environment, not in the URI, so that they
	// never reach a log line or an argument list.
	base.User = nil
	base.Path = strings.TrimSuffix(base.Path, "/") + "/" + cleaned + "/"
	return "rest:" + base.String(), nil
}

func (r *REST) Env() (map[string]string, error) {
	if r.Username == "" {
		return nil, fmt.Errorf("rest: username is required")
	}
	return map[string]string{
		"RESTIC_REST_USERNAME": r.Username,
		"RESTIC_REST_PASSWORD": r.Password,
	}, nil
}

// Options returns none: the REST backend is configured entirely by its URI
// and credentials.
func (r *REST) Options() (map[string]string, error) { return map[string]string{}, nil }

func (r *REST) Preflight(ctx context.Context) error {
	base, err := r.baseURL()
	if err != nil {
		return err
	}
	if r.Username == "" {
		return fmt.Errorf("rest: username is required")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return fmt.Errorf("rest: build request: %w", err)
	}
	req.SetBasicAuth(r.Username, r.Password)

	client := r.HTTPClient
	if client == nil {
		client, err = r.defaultClient()
		if err != nil {
			return err
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("rest: reach server: %w", err)
	}
	defer resp.Body.Close()

	// rest-server answers an unknown path with 404 once authenticated;
	// only an auth failure is a preflight failure here.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("rest: authentication rejected (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("rest: server error (HTTP %d)", resp.StatusCode)
	}

	if r.AppendOnly {
		return r.checkAppendOnly(ctx, base, client)
	}
	return nil
}

// checkAppendOnly finds out whether the server really refuses deletes.
//
// The setting was an operator's word for it, and the page said it meant
// ransomware here could not destroy the history. Nobody had asked the
// server. A rest-server started with --append-only refuses a delete
// whatever the object is; one without it answers 404 for an object that
// is not there. So a delete aimed at a name that cannot exist tells the
// two apart and removes nothing either way.
func (r *REST) checkAppendOnly(ctx context.Context, base *url.URL, client *http.Client) error {
	// A blob id is 64 hex characters. This one is all zeroes, which no
	// content hashes to, so the request cannot name a real object.
	probe := base.JoinPath("data", strings.Repeat("0", 64))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, probe.String(), nil)
	if err != nil {
		return fmt.Errorf("rest: build request: %w", err)
	}
	req.SetBasicAuth(r.Username, r.Password)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("rest: check append-only: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusMethodNotAllowed:
		// Refused, which is what append-only means.
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("rest: authentication rejected while checking append-only (HTTP %d)",
			resp.StatusCode)
	default:
		return fmt.Errorf(
			"rest: this destination is marked append-only, but the server accepted a delete "+
				"(HTTP %d). Start rest-server with --append-only, or clear the setting so the "+
				"page stops promising protection that is not there", resp.StatusCode)
	}
}

// defaultClient trusts the system roots plus any configured private CA.
func (r *REST) defaultClient() (*http.Client, error) {
	transport := &http.Transport{}
	if r.CABundle != "" {
		pem, err := os.ReadFile(r.CABundle)
		if err != nil {
			return nil, fmt.Errorf("rest: read CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("rest: CA bundle %s contains no certificates", r.CABundle)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: transport}, nil
}

func (r *REST) baseURL() (*url.URL, error) {
	if r.BaseURL == "" {
		return nil, fmt.Errorf("rest: base URL is required")
	}
	parsed, err := url.Parse(r.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("rest: parse base URL: %w", err)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("rest: base URL %q has no host", r.BaseURL)
	}
	// Checked here rather than only when the destination is tested. A
	// destination is saved before it is reachable and can be edited
	// afterwards, so a check that only ran on the test button left a way
	// to send this server's credentials, and every account on it, over
	// plain HTTP.
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("rest: base URL must use https, got %q", parsed.Scheme)
	}
	return parsed, nil
}
