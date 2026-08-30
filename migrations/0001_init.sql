-- cprest initial schema. See docs/DESIGN.md §13.
--
-- Two details are deliberate and easy to get wrong:
--   * job targets reference a repository, not a destination, because many
--     repositories share one set of destination credentials (§5);
--   * bytes_added is recorded separately from bytes_processed, because only
--     the former is what a backup actually cost in storage.

BEGIN;

CREATE TYPE secret_kind AS ENUM ('backend_credentials', 'repository_password', 'ssh_key');

-- Envelope-encrypted secrets. Plaintext is never stored here: a master key
-- held outside the database wraps per-secret data keys.
CREATE TABLE secrets (
    id           uuid PRIMARY KEY,
    kind         secret_kind NOT NULL,
    ciphertext   bytea       NOT NULL,
    key_id       text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    rotated_at   timestamptz
);

CREATE TYPE server_status AS ENUM ('pending', 'active', 'suspended', 'retired');
CREATE TYPE payload_mode  AS ENUM ('split', 'monolithic');

CREATE TABLE servers (
    id                       uuid PRIMARY KEY,
    hostname                 text NOT NULL UNIQUE,
    agent_cert_fingerprint   text UNIQUE,
    -- Probed from "pkgacct --help" at enrolment: flag spellings differ
    -- between cPanel versions and must not be assumed (§4).
    pkgacct_flags            jsonb NOT NULL DEFAULT '{}'::jsonb,
    staging_root             text  NOT NULL DEFAULT '/var/lib/cprest/staging',
    max_concurrency          int   NOT NULL DEFAULT 1 CHECK (max_concurrency > 0),
    status                   server_status NOT NULL DEFAULT 'pending',
    created_at               timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE accounts (
    id             uuid PRIMARY KEY,
    server_id      uuid NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    cpanel_user    text NOT NULL,
    primary_domain text,
    size_estimate  bigint CHECK (size_estimate IS NULL OR size_estimate >= 0),
    active         boolean NOT NULL DEFAULT true,
    UNIQUE (server_id, cpanel_user)
);

CREATE TYPE destination_type AS ENUM ('local', 'sftp', 'rest', 's3');

CREATE TABLE destinations (
    id                    uuid PRIMARY KEY,
    name                  text NOT NULL UNIQUE,
    type                  destination_type NOT NULL,
    config                jsonb NOT NULL,
    credentials_secret_id uuid REFERENCES secrets(id),
    -- True when the endpoint enforces append-only for agent credentials.
    -- Retention then requires the maintenance runner (§8).
    append_only           boolean NOT NULL DEFAULT false,
    last_checked_at       timestamptz,
    last_check_error      text
);

CREATE TABLE repositories (
    id                     uuid PRIMARY KEY,
    destination_id         uuid NOT NULL REFERENCES destinations(id) ON DELETE RESTRICT,
    server_id              uuid NOT NULL REFERENCES servers(id) ON DELETE RESTRICT,
    path                   text NOT NULL,
    password_secret_id     uuid NOT NULL REFERENCES secrets(id),
    -- Chunker parameters are fixed at creation and can never change, so
    -- every repository after a server's first must be initialised with
    -- --copy-chunker-params from this source (§7).
    chunker_source_repo_id uuid REFERENCES repositories(id),
    initialised_at         timestamptz,
    UNIQUE (destination_id, path)
);

-- A server's first repository has no chunker source; later ones must have
-- one, and it must belong to the same server.
CREATE OR REPLACE FUNCTION repositories_check_chunker_source() RETURNS trigger AS $$
DECLARE
    existing int;
    source_server uuid;
BEGIN
    SELECT count(*) INTO existing
      FROM repositories
     WHERE server_id = NEW.server_id AND id <> NEW.id;

    IF existing = 0 THEN
        RETURN NEW;
    END IF;

    IF NEW.chunker_source_repo_id IS NULL THEN
        RAISE EXCEPTION
            'repository for server % needs chunker_source_repo_id: chunker parameters cannot be changed after init',
            NEW.server_id;
    END IF;

    SELECT server_id INTO source_server
      FROM repositories WHERE id = NEW.chunker_source_repo_id;

    IF source_server IS DISTINCT FROM NEW.server_id THEN
        RAISE EXCEPTION 'chunker source must belong to the same server';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER repositories_chunker_source
    BEFORE INSERT OR UPDATE ON repositories
    FOR EACH ROW EXECUTE FUNCTION repositories_check_chunker_source();

CREATE TABLE policies (
    id              uuid PRIMARY KEY,
    name            text NOT NULL UNIQUE,
    schedule_cron   text NOT NULL,
    payload_mode    payload_mode NOT NULL DEFAULT 'split',
    -- keep_last / keep_daily / keep_weekly / keep_monthly / keep_yearly.
    retention       jsonb NOT NULL,
    compression     text  NOT NULL DEFAULT 'auto'
                      CHECK (compression IN ('auto', 'max', 'off')),
    limit_upload_kib int  NOT NULL DEFAULT 0 CHECK (limit_upload_kib >= 0)
);

CREATE TABLE policy_repositories (
    policy_id     uuid NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE RESTRICT,
    PRIMARY KEY (policy_id, repository_id)
);

CREATE TABLE account_policies (
    account_id uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    policy_id  uuid NOT NULL REFERENCES policies(id) ON DELETE RESTRICT,
    PRIMARY KEY (account_id, policy_id)
);

CREATE TYPE job_status AS ENUM
    ('pending', 'running', 'success', 'partial_success', 'failed', 'cancelled');
CREATE TYPE job_target_status AS ENUM
    ('pending', 'running', 'success', 'failed', 'skipped');

CREATE TABLE backup_jobs (
    id           uuid PRIMARY KEY,
    account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    policy_id    uuid NOT NULL REFERENCES policies(id) ON DELETE RESTRICT,
    status       job_status NOT NULL DEFAULT 'pending',
    lease_expires_at timestamptz,
    started_at   timestamptz,
    finished_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX backup_jobs_account_created_idx ON backup_jobs (account_id, created_at DESC);
CREATE INDEX backup_jobs_open_idx ON backup_jobs (status)
    WHERE status IN ('pending', 'running');

CREATE TABLE backup_job_targets (
    id               uuid PRIMARY KEY,
    job_id           uuid NOT NULL REFERENCES backup_jobs(id) ON DELETE CASCADE,
    repository_id    uuid NOT NULL REFERENCES repositories(id) ON DELETE RESTRICT,
    status           job_target_status NOT NULL DEFAULT 'pending',
    snapshot_id      text,
    bytes_added      bigint CHECK (bytes_added IS NULL OR bytes_added >= 0),
    bytes_processed  bigint CHECK (bytes_processed IS NULL OR bytes_processed >= 0),
    duration_seconds numeric,
    attempt          int NOT NULL DEFAULT 0,
    -- A snapshot exists but some source files could not be read
    -- (restic exit code 3).
    incomplete       boolean NOT NULL DEFAULT false,
    error            text,
    UNIQUE (job_id, repository_id)
);

CREATE INDEX backup_job_targets_repo_idx ON backup_job_targets (repository_id);

CREATE TYPE maintenance_kind AS ENUM ('forget', 'check', 'provision', 'drill');

CREATE TABLE maintenance_runs (
    id            uuid PRIMARY KEY,
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    kind          maintenance_kind NOT NULL,
    status        job_status NOT NULL DEFAULT 'pending',
    started_at    timestamptz,
    finished_at   timestamptz,
    output        text
);

CREATE INDEX maintenance_runs_repo_idx ON maintenance_runs (repository_id, started_at DESC);

COMMIT;
