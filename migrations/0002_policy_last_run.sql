-- Track when a policy last fired so the scheduler survives a restart
-- without either skipping a window or replaying every past one.

BEGIN;

ALTER TABLE policies ADD COLUMN last_run_at timestamptz;

CREATE INDEX backup_jobs_pending_idx ON backup_jobs (created_at)
    WHERE status = 'pending';

COMMIT;
