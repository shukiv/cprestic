package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound means no row matched.
var ErrNotFound = errors.New("store: not found")

// CreateServer registers a cPanel server and pins the certificate its agent
// will present. Enrolment is deliberately operator-driven: an agent cannot
// register itself.
func (s *Store) CreateServer(ctx context.Context, hostname, certFingerprint string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO servers (id, hostname, agent_cert_fingerprint, status)
		VALUES (gen_random_uuid(), $1, $2, 'pending')
		RETURNING id::text`, hostname, certFingerprint).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: create server: %w", err)
	}
	return id, nil
}

// ServerByFingerprint looks up the server a client certificate belongs to.
// This is the controller's authentication step: an unknown fingerprint is
// an unknown agent.
func (s *Store) ServerByFingerprint(ctx context.Context, fingerprint string) (Server, error) {
	return s.scanServer(s.pool.QueryRow(ctx, `
		SELECT id::text, hostname, coalesce(agent_cert_fingerprint, ''),
		       pkgacct_flags, staging_root, max_concurrency, status::text
		  FROM servers
		 WHERE agent_cert_fingerprint = $1`, fingerprint))
}

// ServerByID looks up a server by its identifier.
func (s *Store) ServerByID(ctx context.Context, id string) (Server, error) {
	return s.scanServer(s.pool.QueryRow(ctx, `
		SELECT id::text, hostname, coalesce(agent_cert_fingerprint, ''),
		       pkgacct_flags, staging_root, max_concurrency, status::text
		  FROM servers
		 WHERE id = $1`, id))
}

func (s *Store) scanServer(row pgx.Row) (Server, error) {
	var (
		server Server
		flags  []byte
	)
	err := row.Scan(&server.ID, &server.Hostname, &server.CertFingerprint,
		&flags, &server.StagingRoot, &server.MaxConcurrency, &server.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Server{}, ErrNotFound
	}
	if err != nil {
		return Server{}, fmt.Errorf("store: read server: %w", err)
	}
	if len(flags) > 0 {
		if err := json.Unmarshal(flags, &server.PkgacctFlags); err != nil {
			return Server{}, fmt.Errorf("store: decode pkgacct_flags: %w", err)
		}
	}
	return server, nil
}

// RecordEnrolment stores what the agent reported about its host and marks
// the server active.
func (s *Store) RecordEnrolment(ctx context.Context, serverID string,
	pkgacctFlags map[string]string, stagingRoot string) error {
	flags, err := json.Marshal(pkgacctFlags)
	if err != nil {
		return fmt.Errorf("store: encode pkgacct_flags: %w", err)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE servers
		   SET pkgacct_flags = $2,
		       staging_root  = coalesce(nullif($3, ''), staging_root),
		       status        = 'active'
		 WHERE id = $1`, serverID, flags, stagingRoot)
	if err != nil {
		return fmt.Errorf("store: record enrolment: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateAccount registers a cPanel account for backup.
func (s *Store) CreateAccount(ctx context.Context, account Account) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO accounts (id, server_id, cpanel_user, primary_domain, size_estimate)
		VALUES (gen_random_uuid(), $1, $2, nullif($3, ''), nullif($4, 0)::bigint)
		RETURNING id::text`,
		account.ServerID, account.CPanelUser, account.PrimaryDomain, account.SizeEstimate).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: create account: %w", err)
	}
	return id, nil
}

// SetAccountSizeEstimate updates the figure the staging preflight uses.
func (s *Store) SetAccountSizeEstimate(ctx context.Context, accountID string, bytes int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE accounts SET size_estimate = $2 WHERE id = $1`, accountID, bytes)
	if err != nil {
		return fmt.Errorf("store: set size estimate: %w", err)
	}
	return nil
}
