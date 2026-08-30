-- Restore, dispatched through the same agent lease mechanism as backup.
--
-- A restore has one source repository rather than N targets, so it gets its
-- own table instead of being forced into backup_job_targets.

BEGIN;

-- 'account' rebuilds the whole cpmove archive; 'files' pulls named paths
-- out of a snapshot and leaves them where the operator asked.
CREATE TYPE restore_kind AS ENUM ('account', 'files');

CREATE TABLE restore_jobs (
    id             uuid PRIMARY KEY,
    account_id     uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    -- The controller chooses the source repository; the agent is told
    -- which one to read and never picks for itself.
    repository_id  uuid NOT NULL REFERENCES repositories(id) ON DELETE RESTRICT,
    snapshot_id    text NOT NULL,
    kind           restore_kind NOT NULL DEFAULT 'account',
    include_paths  text[] NOT NULL DEFAULT '{}',
    -- Where a 'files' restore should leave what it recovered.
    target_dir     text,
    -- Whether to hand the rebuilt archive to cPanel's restorepkg. Off by
    -- default: materialising the files is safe, overwriting a live account
    -- is not.
    apply          boolean NOT NULL DEFAULT false,

    status           job_status NOT NULL DEFAULT 'pending',
    lease_expires_at timestamptz,
    attempt          int NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL DEFAULT now(),
    started_at       timestamptz,
    finished_at      timestamptz,

    bytes_restored bigint CHECK (bytes_restored IS NULL OR bytes_restored >= 0),
    archive_path   text,
    error          text,

    CONSTRAINT restore_files_needs_paths
        CHECK (kind <> 'files' OR cardinality(include_paths) > 0),
    -- Applying a partial file restore through restorepkg makes no sense:
    -- restorepkg takes a whole account archive.
    CONSTRAINT restore_apply_is_account_only
        CHECK (NOT apply OR kind = 'account')
);

CREATE INDEX restore_jobs_pending_idx ON restore_jobs (created_at)
    WHERE status = 'pending';
CREATE INDEX restore_jobs_account_idx ON restore_jobs (account_id, created_at DESC);

COMMIT;
