package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CreateSecret stores a sealed credential and returns its identifier.
// The caller seals the value; the store never sees plaintext.
func (s *Store) CreateSecret(ctx context.Context, kind SecretKind, sealed []byte, keyID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO secrets (id, kind, ciphertext, key_id)
		VALUES (gen_random_uuid(), $1, $2, $3)
		RETURNING id::text`, string(kind), sealed, keyID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: create secret: %w", err)
	}
	return id, nil
}

// CreateDestination registers a storage endpoint.
func (s *Store) CreateDestination(ctx context.Context, dest Destination) (string, error) {
	if len(dest.Config) == 0 {
		dest.Config = []byte(`{}`)
	}
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO destinations (id, name, type, config, credentials_secret_id, append_only)
		VALUES (gen_random_uuid(), $1, $2, $3, nullif($4, '')::uuid, $5)
		RETURNING id::text`,
		dest.Name, dest.Type, dest.Config, dest.CredentialsSecretID, dest.AppendOnly).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: create destination: %w", err)
	}
	return id, nil
}

// CreateRepository records a repository, filling in the chunker source
// automatically.
//
// Chunker parameters are fixed when a repository is created and can never
// change, so every repository after a server's first must copy them from
// that first one. Doing it here means an operator cannot forget; a database
// trigger enforces the same rule as a backstop. See docs/DESIGN.md §7.
func (s *Store) CreateRepository(ctx context.Context, repo Repository) (Repository, error) {
	var created Repository
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if repo.ChunkerSourceRepoID == "" {
			var existing string
			err := tx.QueryRow(ctx, `
				SELECT id::text FROM repositories
				 WHERE server_id = $1
				 ORDER BY initialised_at NULLS LAST, id
				 LIMIT 1`, repo.ServerID).Scan(&existing)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				// First repository for this server: it becomes the source.
			case err != nil:
				return fmt.Errorf("store: find chunker source: %w", err)
			default:
				repo.ChunkerSourceRepoID = existing
			}
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO repositories
			       (id, destination_id, server_id, path, password_secret_id, chunker_source_repo_id)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, nullif($5, '')::uuid)
			RETURNING id::text, destination_id::text, server_id::text, path,
			          password_secret_id::text, coalesce(chunker_source_repo_id::text, ''),
			          initialised_at`,
			repo.DestinationID, repo.ServerID, repo.Path,
			repo.PasswordSecretID, repo.ChunkerSourceRepoID)
		if err := row.Scan(&created.ID, &created.DestinationID, &created.ServerID,
			&created.Path, &created.PasswordSecretID, &created.ChunkerSourceRepoID,
			&created.InitialisedAt); err != nil {
			return fmt.Errorf("store: create repository: %w", err)
		}
		return nil
	})
	return created, err
}

// MarkRepositoryInitialised records that "restic init" has succeeded.
func (s *Store) MarkRepositoryInitialised(ctx context.Context, repositoryID string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE repositories SET initialised_at = now() WHERE id = $1`, repositoryID)
	if err != nil {
		return fmt.Errorf("store: mark repository initialised: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RepositoryWithDestination returns a repository joined to its destination,
// for the maintenance runner.
func (s *Store) RepositoryWithDestination(ctx context.Context, repositoryID string) (Repository, Destination, error) {
	var (
		repo Repository
		dest Destination
	)
	err := s.pool.QueryRow(ctx, `
		SELECT r.id::text, r.destination_id::text, r.server_id::text, r.path,
		       r.password_secret_id::text, coalesce(r.chunker_source_repo_id::text, ''),
		       r.initialised_at,
		       d.id::text, d.name, d.type::text, d.config,
		       coalesce(d.credentials_secret_id::text, ''), d.append_only
		  FROM repositories r
		  JOIN destinations d ON d.id = r.destination_id
		 WHERE r.id = $1`, repositoryID).Scan(
		&repo.ID, &repo.DestinationID, &repo.ServerID, &repo.Path,
		&repo.PasswordSecretID, &repo.ChunkerSourceRepoID, &repo.InitialisedAt,
		&dest.ID, &dest.Name, &dest.Type, &dest.Config,
		&dest.CredentialsSecretID, &dest.AppendOnly)
	if errors.Is(err, pgx.ErrNoRows) {
		return Repository{}, Destination{}, ErrNotFound
	}
	if err != nil {
		return Repository{}, Destination{}, fmt.Errorf("store: read repository: %w", err)
	}
	return repo, dest, nil
}

// ListRepositories returns every repository, newest last.
func (s *Store) ListRepositories(ctx context.Context) ([]Repository, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, destination_id::text, server_id::text, path,
		       password_secret_id::text, coalesce(chunker_source_repo_id::text, ''), initialised_at
		  FROM repositories ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list repositories: %w", err)
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

// Secret returns a sealed credential.
func (s *Store) Secret(ctx context.Context, secretID string) ([]byte, string, error) {
	var (
		sealed []byte
		keyID  string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT ciphertext, key_id FROM secrets WHERE id = $1`, secretID).Scan(&sealed, &keyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("store: read secret: %w", err)
	}
	return sealed, keyID, nil
}

// CreatePolicy registers a schedule with its retention and payload settings.
func (s *Store) CreatePolicy(ctx context.Context, policy Policy) (string, error) {
	retention, err := json.Marshal(policy.Retention)
	if err != nil {
		return "", fmt.Errorf("store: encode retention: %w", err)
	}
	if policy.Compression == "" {
		policy.Compression = "auto"
	}
	if policy.PayloadMode == "" {
		policy.PayloadMode = "split"
	}

	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO policies (id, name, schedule_cron, payload_mode, retention, compression, limit_upload_kib)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6)
		RETURNING id::text`,
		policy.Name, policy.ScheduleCron, policy.PayloadMode,
		retention, policy.Compression, policy.LimitUploadKiB).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: create policy: %w", err)
	}
	return id, nil
}

// AttachRepositoryToPolicy adds a backup target to a policy.
func (s *Store) AttachRepositoryToPolicy(ctx context.Context, policyID, repositoryID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO policy_repositories (policy_id, repository_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, policyID, repositoryID)
	if err != nil {
		return fmt.Errorf("store: attach repository to policy: %w", err)
	}
	return nil
}

// AttachPolicyToAccount puts an account on a schedule.
func (s *Store) AttachPolicyToAccount(ctx context.Context, accountID, policyID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO account_policies (account_id, policy_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, accountID, policyID)
	if err != nil {
		return fmt.Errorf("store: attach policy to account: %w", err)
	}
	return nil
}

// ListPolicies returns every policy.
func (s *Store) ListPolicies(ctx context.Context) ([]Policy, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, name, schedule_cron, payload_mode::text, retention,
		       compression, limit_upload_kib
		  FROM policies ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list policies: %w", err)
	}
	defer rows.Close()

	var policies []Policy
	for rows.Next() {
		var (
			policy    Policy
			retention []byte
		)
		if err := rows.Scan(&policy.ID, &policy.Name, &policy.ScheduleCron,
			&policy.PayloadMode, &retention, &policy.Compression,
			&policy.LimitUploadKiB); err != nil {
			return nil, fmt.Errorf("store: scan policy: %w", err)
		}
		if len(retention) > 0 {
			if err := json.Unmarshal(retention, &policy.Retention); err != nil {
				return nil, fmt.Errorf("store: decode retention: %w", err)
			}
		}
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}

// AccountsForPolicy lists the accounts a policy covers.
func (s *Store) AccountsForPolicy(ctx context.Context, policyID string) ([]Account, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id::text, a.server_id::text, a.cpanel_user,
		       coalesce(a.primary_domain, ''), coalesce(a.size_estimate, 0), a.active
		  FROM accounts a
		  JOIN account_policies ap ON ap.account_id = a.id
		 WHERE ap.policy_id = $1 AND a.active
		 ORDER BY a.cpanel_user`, policyID)
	if err != nil {
		return nil, fmt.Errorf("store: accounts for policy: %w", err)
	}
	defer rows.Close()

	var accounts []Account
	for rows.Next() {
		var account Account
		if err := rows.Scan(&account.ID, &account.ServerID, &account.CPanelUser,
			&account.PrimaryDomain, &account.SizeEstimate, &account.Active); err != nil {
			return nil, fmt.Errorf("store: scan account: %w", err)
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

// PolicyLastRun reports when the scheduler last fired a policy. A zero time
// means it has never fired.
func (s *Store) PolicyLastRun(ctx context.Context, policyID string) (time.Time, error) {
	var lastRun *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT last_run_at FROM policies WHERE id = $1`, policyID).Scan(&lastRun)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("store: read policy last run: %w", err)
	}
	if lastRun == nil {
		return time.Time{}, nil
	}
	return *lastRun, nil
}

// SetPolicyLastRun records a firing, so a controller restart neither skips
// a window nor replays past ones.
func (s *Store) SetPolicyLastRun(ctx context.Context, policyID string, at time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE policies SET last_run_at = $2 WHERE id = $1`, policyID, at)
	if err != nil {
		return fmt.Errorf("store: set policy last run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RepositoriesNeedingInit lists repositories that have never been created
// on their destination.
func (s *Store) RepositoriesNeedingInit(ctx context.Context) ([]Repository, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, destination_id::text, server_id::text, path,
		       password_secret_id::text, coalesce(chunker_source_repo_id::text, ''), initialised_at
		  FROM repositories
		 WHERE initialised_at IS NULL
		 -- A repository that seeds another's chunker parameters must exist
		 -- first, so order sources before dependants.
		 ORDER BY chunker_source_repo_id NULLS FIRST, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list uninitialised repositories: %w", err)
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

// StartMaintenanceRun records that upkeep has begun on a repository.
func (s *Store) StartMaintenanceRun(ctx context.Context, repositoryID, kind string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO maintenance_runs (id, repository_id, kind, status, started_at)
		VALUES (gen_random_uuid(), $1, $2, 'running', now())
		RETURNING id::text`, repositoryID, kind).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: start maintenance run: %w", err)
	}
	return id, nil
}

// FinishMaintenanceRun closes out an upkeep run.
func (s *Store) FinishMaintenanceRun(ctx context.Context, runID, status, output string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE maintenance_runs
		   SET status = $2::job_status, finished_at = now(), output = nullif($3, '')
		 WHERE id = $1`, runID, status, output)
	if err != nil {
		return fmt.Errorf("store: finish maintenance run: %w", err)
	}
	return nil
}

// SealedRepository gathers everything needed to open a repository's
// credentials, for the maintenance runner.
func (s *Store) SealedRepository(ctx context.Context, repositoryID string) (ClaimedTarget, error) {
	var target ClaimedTarget
	err := s.pool.QueryRow(ctx, `
		SELECT r.id::text, r.path, d.type::text, d.config,
		       coalesce(cs.ciphertext, ''::bytea), ps.ciphertext
		  FROM repositories r
		  JOIN destinations d  ON d.id = r.destination_id
		  JOIN secrets ps      ON ps.id = r.password_secret_id
		  LEFT JOIN secrets cs ON cs.id = d.credentials_secret_id
		 WHERE r.id = $1`, repositoryID).Scan(
		&target.RepositoryID, &target.RepositoryPath, &target.DestinationType,
		&target.DestinationConfig, &target.CredentialsSealed, &target.RepoPasswordSealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ClaimedTarget{}, ErrNotFound
	}
	if err != nil {
		return ClaimedTarget{}, fmt.Errorf("store: read sealed repository: %w", err)
	}
	return target, nil
}
