-- Say what a failed restore already changed.
--
-- A granular restore is not transactional and cannot be: cPanel has no
-- way to undo a loaded database, and a home directory written over cannot
-- be put back. So a restore can overwrite one database, fail on the next,
-- and until now report nothing but the failure -- leaving the account
-- changed, the operator unaware, and the customer to find out.
--
-- The agent already reports what it wrote and whether the live account was
-- touched. These are where that lands in fleet mode; standalone has held
-- both since it was written.

BEGIN;

ALTER TABLE restore_jobs ADD COLUMN detail  text;
ALTER TABLE restore_jobs ADD COLUMN applied boolean NOT NULL DEFAULT false;

COMMIT;
