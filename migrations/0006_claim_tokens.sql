-- Tell two attempts at the same job apart.
--
-- A lease that expires puts a job back on the queue, and the next claim
-- puts the same job id back into 'running'. Until now a report carried
-- only that id, so the late report of the abandoned attempt satisfied
-- every check the controller made and closed out the attempt that is
-- actually running -- with the wrong outcome, and, for a restore, while
-- the other attempt is still writing into a live account.
--
-- The claim token is what the two attempts do not share. It is generated
-- when the job is leased, travels in the assignment, comes back in the
-- report, and is cleared whenever the job returns to the queue.

BEGIN;

ALTER TABLE backup_jobs  ADD COLUMN claim_token uuid;
ALTER TABLE restore_jobs ADD COLUMN claim_token uuid;

-- Anything already running was claimed before there were tokens, so it
-- has no attempt to be told apart from. Returning it to the queue is the
-- honest state: whichever agent holds it will be refused, and the job
-- will be claimed again with a token.
UPDATE backup_jobs
   SET status = 'pending', lease_expires_at = NULL
 WHERE status = 'running';
UPDATE restore_jobs
   SET status = 'pending', lease_expires_at = NULL
 WHERE status = 'running';

COMMIT;
