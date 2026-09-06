-- The program was renamed from cprest to Gniza, and its directories with
-- it. The staging root is stored per server and defaulted in the schema,
-- so both the default and the rows still following it have to move.
--
-- Only a row that is still on the old default is touched. A server whose
-- operator chose a staging root of their own keeps it: it is a path they
-- picked on a disk they picked, and the rename did not move it.

BEGIN;

ALTER TABLE servers ALTER COLUMN staging_root SET DEFAULT '/var/lib/gniza/staging';

UPDATE servers SET staging_root = '/var/lib/gniza/staging'
 WHERE staging_root = '/var/lib/cprest/staging';

COMMIT;
