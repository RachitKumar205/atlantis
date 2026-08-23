-- Reverse of 0006.
--
-- Dropping created_by loses who made each organisation, and there is no way to
-- recover it: membership records who may act, never who created. Re-applying
-- 0006 leaves every existing organisation unowned, exactly as it found the two
-- that predated the column.
--
-- Dropping org_limit returns every account to unbounded creation. That is the
-- state this migration exists to end, so a deployment that runs this down
-- should not then serve POST /api/orgs.

ALTER TABLE cloud.users DROP COLUMN IF EXISTS org_limit;

DROP INDEX IF EXISTS cloud.orgs_created_by;

ALTER TABLE cloud.orgs DROP COLUMN IF EXISTS created_by;
