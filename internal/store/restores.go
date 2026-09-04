package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shuki/cprest/internal/job"
)

// RestoreRequest asks for an account, or part of one, to be brought back.
type RestoreRequest struct {
	AccountID string
	// RepositoryID is the source. Leave it empty to let the store pick a
	// provisioned repository for the account's server.
	RepositoryID string
	SnapshotID   string
	Kind         string
	IncludePaths []string
	TargetDir    string
	// Apply writes the restore into the live account rather than leaving
	// a copy to collect. A whole account goes to restorepkg; a part of one
	// is written back where it belongs.
	Apply bool
}

// ClaimedRestore is a restore leased to an agent.
type ClaimedRestore struct {
	JobID          string
	LeaseExpiresAt time.Time
	Account        Account
	SnapshotID     string
	Kind           string
	IncludePaths   []string
	TargetDir      string
	Apply          bool
	Source         ClaimedTarget
}

// CreateRestore queues a restore.
func (s *Store) CreateRestore(ctx context.Context, req RestoreRequest) (string, error) {
	if req.Kind == "" {
		req.Kind = "account"
	}
	if req.SnapshotID == "" {
		return "", errors.New("store: restore needs a snapshot id")
	}
	if req.IncludePaths == nil {
		// A nil slice becomes SQL NULL, and the column is an empty array
		// by default rather than nullable.
		req.IncludePaths = []string{}
	}

	var jobID string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		repositoryID := req.RepositoryID
		if repositoryID == "" {
			// Any provisioned repository for the account's server holds
			// the same history, so the first one is as good as another.
			// An operator who cares picks explicitly.
			err := tx.QueryRow(ctx, `
				SELECT r.id::text
				  FROM repositories r
				  JOIN accounts a ON a.server_id = r.server_id
				 WHERE a.id = $1 AND r.initialised_at IS NOT NULL
				 ORDER BY r.initialised_at
				 LIMIT 1`, req.AccountID).Scan(&repositoryID)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("store: no provisioned repository holds account %s", req.AccountID)
			}
			if err != nil {
				return fmt.Errorf("store: choose restore source: %w", err)
			}
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO restore_jobs
			       (id, account_id, repository_id, snapshot_id, kind,
			        include_paths, target_dir, apply)
			VALUES (gen_random_uuid(), $1, $2, $3, $4::restore_kind, $5, nullif($6, ''), $7)
			RETURNING id::text`,
			req.AccountID, repositoryID, req.SnapshotID, req.Kind,
			req.IncludePaths, req.TargetDir, req.Apply).Scan(&jobID); err != nil {
			return fmt.Errorf("store: create restore: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return jobID, nil
}

// ClaimNextRestore leases one pending restore for a server.
//
// Restores are claimed ahead of backups by the caller, because someone is
// usually waiting for one. An account with a backup already running is
// skipped: both stage under the account's name, and the two would collide.
func (s *Store) ClaimNextRestore(ctx context.Context, serverID string, lease time.Duration) (ClaimedRestore, error) {
	var claimed ClaimedRestore
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE restore_jobs SET
			    status = 'running',
			    started_at = coalesce(started_at, now()),
			    lease_expires_at = now() + $2::interval,
			    attempt = attempt + 1
			 WHERE id = (
			     SELECT rj.id
			       FROM restore_jobs rj
			       JOIN accounts a ON a.id = rj.account_id
			      WHERE a.server_id = $1 AND rj.status = 'pending'
			        AND NOT EXISTS (
			            SELECT 1 FROM backup_jobs bj
			             WHERE bj.account_id = rj.account_id AND bj.status = 'running')
			        AND NOT EXISTS (
			            SELECT 1 FROM restore_jobs other
			             WHERE other.account_id = rj.account_id AND other.status = 'running')
			      ORDER BY rj.created_at
			      FOR UPDATE OF rj SKIP LOCKED
			      LIMIT 1)
			 RETURNING id::text, lease_expires_at, account_id::text, repository_id::text,
			           snapshot_id, kind::text, include_paths, coalesce(target_dir, ''), apply`,
			serverID, lease.String())

		var repositoryID string
		err := row.Scan(&claimed.JobID, &claimed.LeaseExpiresAt, &claimed.Account.ID,
			&repositoryID, &claimed.SnapshotID, &claimed.Kind,
			&claimed.IncludePaths, &claimed.TargetDir, &claimed.Apply)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoWork
		}
		if err != nil {
			return fmt.Errorf("store: claim restore: %w", err)
		}

		if err := tx.QueryRow(ctx, `
			SELECT id::text, server_id::text, cpanel_user, coalesce(primary_domain, ''),
			       coalesce(size_estimate, 0), active
			  FROM accounts WHERE id = $1`, claimed.Account.ID).Scan(
			&claimed.Account.ID, &claimed.Account.ServerID, &claimed.Account.CPanelUser,
			&claimed.Account.PrimaryDomain, &claimed.Account.SizeEstimate,
			&claimed.Account.Active); err != nil {
			return fmt.Errorf("store: read restore account: %w", err)
		}

		if err := tx.QueryRow(ctx, `
			SELECT r.id::text, r.path, d.type::text, d.config,
			       coalesce(cs.ciphertext, ''::bytea), ps.ciphertext
			  FROM repositories r
			  JOIN destinations d  ON d.id = r.destination_id
			  JOIN secrets ps      ON ps.id = r.password_secret_id
			  LEFT JOIN secrets cs ON cs.id = d.credentials_secret_id
			 WHERE r.id = $1`, repositoryID).Scan(
			&claimed.Source.RepositoryID, &claimed.Source.RepositoryPath,
			&claimed.Source.DestinationType, &claimed.Source.DestinationConfig,
			&claimed.Source.CredentialsSealed, &claimed.Source.RepoPasswordSealed); err != nil {
			return fmt.Errorf("store: read restore source: %w", err)
		}
		return nil
	})
	if err != nil {
		return ClaimedRestore{}, err
	}
	return claimed, nil
}

// RestoreOutcome is what an agent reports back.
type RestoreOutcome struct {
	Status        job.Status
	BytesRestored uint64
	ArchivePath   string
	Error         string
}

// ApplyRestoreReport records a restore's result.
func (s *Store) ApplyRestoreReport(ctx context.Context, jobID string, outcome RestoreOutcome) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE restore_jobs SET
		    status = $2::job_status,
		    bytes_restored = $3,
		    archive_path = nullif($4, ''),
		    error = nullif($5, ''),
		    lease_expires_at = NULL,
		    finished_at = now()
		 WHERE id = $1`,
		jobID, string(outcome.Status), int64(outcome.BytesRestored),
		outcome.ArchivePath, outcome.Error)
	if err != nil {
		return fmt.Errorf("store: record restore result: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ReclaimExpiredRestoreLeases returns abandoned restores to the queue.
func (s *Store) ReclaimExpiredRestoreLeases(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE restore_jobs
		   SET status = 'pending', lease_expires_at = NULL
		 WHERE status = 'running'
		   AND lease_expires_at IS NOT NULL
		   AND lease_expires_at < now()`)
	if err != nil {
		return 0, fmt.Errorf("store: reclaim restore leases: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Restore reads one restore job.
type Restore struct {
	ID            string
	AccountID     string
	RepositoryID  string
	SnapshotID    string
	Kind          string
	Status        job.Status
	BytesRestored uint64
	ArchivePath   string
	Error         string
	Attempt       int
	CreatedAt     time.Time
	FinishedAt    *time.Time
}

// RestoreByID reads a restore job.
func (s *Store) RestoreByID(ctx context.Context, jobID string) (Restore, error) {
	var (
		restore Restore
		status  string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, account_id::text, repository_id::text, snapshot_id,
		       kind::text, status::text, coalesce(bytes_restored, 0),
		       coalesce(archive_path, ''), coalesce(error, ''), attempt,
		       created_at, finished_at
		  FROM restore_jobs WHERE id = $1`, jobID).Scan(
		&restore.ID, &restore.AccountID, &restore.RepositoryID, &restore.SnapshotID,
		&restore.Kind, &status, &restore.BytesRestored, &restore.ArchivePath,
		&restore.Error, &restore.Attempt, &restore.CreatedAt, &restore.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Restore{}, ErrNotFound
	}
	if err != nil {
		return Restore{}, fmt.Errorf("store: read restore: %w", err)
	}
	restore.Status = job.Status(status)
	return restore, nil
}

// AccountByUser finds an account by server and cPanel user, for the CLI.
func (s *Store) AccountByUser(ctx context.Context, hostname, user string) (Account, error) {
	var account Account
	err := s.pool.QueryRow(ctx, `
		SELECT a.id::text, a.server_id::text, a.cpanel_user,
		       coalesce(a.primary_domain, ''), coalesce(a.size_estimate, 0), a.active
		  FROM accounts a
		  JOIN servers s ON s.id = a.server_id
		 WHERE s.hostname = $1 AND a.cpanel_user = $2`, hostname, user).Scan(
		&account.ID, &account.ServerID, &account.CPanelUser,
		&account.PrimaryDomain, &account.SizeEstimate, &account.Active)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("store: read account: %w", err)
	}
	return account, nil
}

// ProvisionedRepositoriesForAccount lists repositories holding an account's
// server's backups, so an operator can choose where to restore from.
func (s *Store) ProvisionedRepositoriesForAccount(ctx context.Context, accountID string) ([]Repository, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id::text, r.destination_id::text, r.server_id::text, r.path,
		       r.password_secret_id::text, coalesce(r.chunker_source_repo_id::text, ''),
		       r.initialised_at
		  FROM repositories r
		  JOIN accounts a ON a.server_id = r.server_id
		 WHERE a.id = $1 AND r.initialised_at IS NOT NULL
		 ORDER BY r.initialised_at`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list repositories for account: %w", err)
	}
	defer rows.Close()

	var repos []Repository
	for rows.Next() {
		var repo Repository
		if err := rows.Scan(&repo.ID, &repo.DestinationID, &repo.ServerID, &repo.Path,
			&repo.PasswordSecretID, &repo.ChunkerSourceRepoID, &repo.InitialisedAt); err != nil {
			return nil, fmt.Errorf("store: scan repository: %w", err)
		}
		repos = append(repos, repo)
	}
	return repos, rows.Err()
}
