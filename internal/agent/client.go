// Package agent runs on a cPanel server: it asks the controller for work,
// stages a payload and uploads it to every repository the job names.
package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/shuki/cprest/internal/protocol"
)

// ErrNoWork means the controller had nothing queued for this server.
var ErrNoWork = errors.New("agent: no work available")

// Client talks to the controller over mutual TLS. The agent always dials
// out; the controller never connects to a cPanel server.
type Client struct {
	baseURL string
	http    *http.Client
}

// ClientConfig is the agent's half of the mTLS handshake.
type ClientConfig struct {
	BaseURL        string
	ClientCertPath string
	ClientKeyPath  string
	CABundlePath   string
	// Timeout bounds enrolment and reporting. Job polling uses its own,
	// longer deadline because the controller holds the request open.
	Timeout time.Duration
}

// NewClient builds a Client.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("agent: controller URL is required")
	}
	cert, err := tls.LoadX509KeyPair(cfg.ClientCertPath, cfg.ClientKeyPath)
	if err != nil {
		return nil, fmt.Errorf("agent: load client certificate: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.CABundlePath)
	if err != nil {
		return nil, fmt.Errorf("agent: read CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("agent: CA bundle %s contains no certificates", cfg.CABundlePath)
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL: strings.TrimSuffix(cfg.BaseURL, "/"),
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					Certificates: []tls.Certificate{cert},
					RootCAs:      pool,
					MinVersion:   tls.VersionTLS13,
				},
			},
		},
	}, nil
}

// NewClientWithHTTP wraps an existing HTTP client, for tests that already
// hold a configured transport.
func NewClientWithHTTP(baseURL string, httpClient *http.Client) *Client {
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), http: httpClient}
}

// Enrol tells the controller what this host supports.
func (c *Client) Enrol(ctx context.Context, req protocol.EnrolRequest) (protocol.EnrolResponse, error) {
	var response protocol.EnrolResponse
	if err := c.do(ctx, http.MethodPost, protocol.PathEnrol, req, &response); err != nil {
		return protocol.EnrolResponse{}, err
	}
	return response, nil
}

// NextWork long-polls for work, which may be a backup or a restore. It
// returns ErrNoWork when the controller answers that nothing is queued.
func (c *Client) NextWork(ctx context.Context) (protocol.Assignment, error) {
	var assignment protocol.Assignment
	if err := c.do(ctx, http.MethodGet, protocol.PathNextJob, nil, &assignment); err != nil {
		return protocol.Assignment{}, err
	}
	switch assignment.Kind {
	case protocol.KindBackup:
		if assignment.Backup == nil || assignment.Backup.JobID == "" {
			return protocol.Assignment{}, errors.New("agent: backup assignment is empty")
		}
	case protocol.KindRestore:
		if assignment.Restore == nil || assignment.Restore.JobID == "" {
			return protocol.Assignment{}, errors.New("agent: restore assignment is empty")
		}
	case "":
		return protocol.Assignment{}, ErrNoWork
	default:
		return protocol.Assignment{}, fmt.Errorf("agent: unknown work kind %q", assignment.Kind)
	}
	return assignment, nil
}

// Report submits a backup job's outcome.
func (c *Client) Report(ctx context.Context, report protocol.JobReport) error {
	return c.do(ctx, http.MethodPost, protocol.PathReport, report, nil)
}

// ReportRestore submits a restore's outcome.
func (c *Client) ReportRestore(ctx context.Context, report protocol.RestoreReport) error {
	return c.do(ctx, http.MethodPost, protocol.PathRestoreReport, report, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("agent: encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("agent: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agent: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("agent: %s %s: %w", method, path, decodeError(resp))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, protocol.MaxBodyBytes)).Decode(out); err != nil {
		return fmt.Errorf("agent: decode response: %w", err)
	}
	return nil
}

func decodeError(resp *http.Response) error {
	var payload protocol.ErrorResponse
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if json.Unmarshal(body, &payload) == nil && payload.Error != "" {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, payload.Error)
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}
