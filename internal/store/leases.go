package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RenewBackupLease gives a running backup more time.
//
// A lease is a fixed span and the work is not. A backup of a large account
// over a slow link can outlast it, and when it does the job is handed to
// somebody else while the first agent is still uploading. Saying "still
// working" is cheaper than sizing the lease for the worst account on the
// slowest night.
//
// Only the attempt that holds the job may extend it, which is what the
// claim token is for: an agent whose lease was taken back must not be able
// to take it from whoever has it now. Nothing renewed means exactly that,
// and the caller has to stop rather than keep working on a job it no
// longer holds.
func (s *Store) RenewBackupLease(ctx context.Context, serverID, jobID, claimToken string,
	lease time.Duration) (time.Time, error) {

	return s.renewLease(ctx, `
		UPDATE backup_jobs bj SET lease_expires_at = now() + $4::interval
		 WHERE bj.id = $1 AND bj.status = 'running'
		   AND bj.claim_token = $3::uuid
		   AND bj.account_id IN (SELECT id FROM accounts WHERE server_id = $2)
		RETURNING bj.lease_expires_at`, serverID, jobID, claimToken, lease)
}

// RenewRestoreLease gives a running restore more time. The same reasoning
// as RenewBackupLease, and more urgently: a restore that loses its lease
// mid-write is one writing into a live account with no claim on it.
func (s *Store) RenewRestoreLease(ctx context.Context, serverID, jobID, claimToken string,
	lease time.Duration) (time.Time, error) {

	return s.renewLease(ctx, `
		UPDATE restore_jobs rj SET lease_expires_at = now() + $4::interval
		 WHERE rj.id = $1 AND rj.status = 'running'
		   AND rj.claim_token = $3::uuid
		   AND rj.account_id IN (SELECT id FROM accounts WHERE server_id = $2)
		RETURNING rj.lease_expires_at`, serverID, jobID, claimToken, lease)
}

func (s *Store) renewLease(ctx context.Context, query, serverID, jobID, claimToken string,
	lease time.Duration) (time.Time, error) {

	var expires time.Time
	err := s.pool.QueryRow(ctx, query, jobID, serverID, claimToken, lease.String()).Scan(&expires)
	if err != nil {
		// No row means the job is not running, is not this server's, or
		// is running under a different attempt. All three mean the same
		// thing to the agent asking: it does not hold this job.
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, ErrNotFound
		}
		return time.Time{}, fmt.Errorf("store: renew lease: %w", err)
	}
	return expires, nil
}
