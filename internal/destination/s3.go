package destination

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// S3 covers Amazon S3 and every S3-compatible service: Wasabi, Cloudflare
// R2, MinIO, Ceph RGW, Backblaze B2 via its S3 API. They differ only in
// endpoint and credentials, so they share one implementation.
type S3 struct {
	// Endpoint is the service host, optionally with a scheme and port.
	// Empty means Amazon S3 proper, addressed by Region.
	Endpoint string
	// Region is sent as AWS_DEFAULT_REGION. Required by some providers.
	Region string
	Bucket string
	// AccessKeyID and SecretAccessKey come from the vault and are passed
	// to restic through the environment only.
	AccessKeyID     string
	SecretAccessKey string
	// SessionToken is optional, for temporary credentials.
	SessionToken string
}

var _ Destination = (*S3)(nil)

func (s *S3) Type() Type { return TypeS3 }

func (s *S3) URI(repoPath string) (string, error) {
	if s.Bucket == "" {
		return "", fmt.Errorf("s3: bucket is required")
	}
	cleaned, err := CleanRepoPath(repoPath)
	if err != nil {
		return "", err
	}
	if s.Endpoint == "" {
		if s.Region == "" {
			return "", fmt.Errorf("s3: region is required when no endpoint is set")
		}
		return fmt.Sprintf("s3:s3.amazonaws.com/%s/%s", s.Bucket, cleaned), nil
	}
	endpoint, err := s.normalisedEndpoint()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("s3:%s/%s/%s", endpoint, s.Bucket, cleaned), nil
}

func (s *S3) Env() (map[string]string, error) {
	if s.AccessKeyID == "" || s.SecretAccessKey == "" {
		return nil, fmt.Errorf("s3: access key and secret key are required")
	}
	env := map[string]string{
		"AWS_ACCESS_KEY_ID":     s.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY": s.SecretAccessKey,
	}
	if s.Region != "" {
		env["AWS_DEFAULT_REGION"] = s.Region
	}
	if s.SessionToken != "" {
		env["AWS_SESSION_TOKEN"] = s.SessionToken
	}
	return env, nil
}

// Options returns none. Provider-specific tuning (storage class, part
// size) belongs here when a destination needs it.
func (s *S3) Options() (map[string]string, error) { return map[string]string{}, nil }

func (s *S3) Preflight(ctx context.Context) error {
	if _, err := s.Env(); err != nil {
		return err
	}
	if s.Bucket == "" {
		return fmt.Errorf("s3: bucket is required")
	}
	endpoint := s.Endpoint
	if endpoint == "" {
		endpoint = "https://s3.amazonaws.com"
	}
	parsed, err := url.Parse(withScheme(endpoint))
	if err != nil {
		return fmt.Errorf("s3: parse endpoint: %w", err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("s3: endpoint must use https, got %q", parsed.Scheme)
	}
	port := parsed.Port()
	if port == "" {
		port = "443"
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(parsed.Hostname(), port))
	if err != nil {
		return fmt.Errorf("s3: dial endpoint: %w", err)
	}
	return conn.Close()
}

// normalisedEndpoint renders the endpoint the way restic's s3 backend
// expects it: bare host for the default scheme, or scheme://host[:port].
func (s *S3) normalisedEndpoint() (string, error) {
	parsed, err := url.Parse(withScheme(s.Endpoint))
	if err != nil {
		return "", fmt.Errorf("s3: parse endpoint: %w", err)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("s3: endpoint %q has no host", s.Endpoint)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("s3: endpoint %q must not contain a path", s.Endpoint)
	}
	if parsed.Scheme == "https" {
		return "https://" + parsed.Host, nil
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// withScheme defaults a bare host to https so url.Parse treats it as a host
// rather than a path.
func withScheme(endpoint string) string {
	if strings.Contains(endpoint, "://") {
		return endpoint
	}
	return "https://" + endpoint
}
