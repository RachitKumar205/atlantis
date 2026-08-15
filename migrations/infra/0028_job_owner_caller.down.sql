-- Dropping the column discards the ownership record and returns the job RPCs
-- to reading across every caller, because the code that scopes them ships with
-- the code that creates this column: a deployment rolled back past 0028 has no
-- scoping to enforce.
--
-- That is worth saying plainly rather than leaving implicit. This down
-- migration widens who can read what. It is correct as a rollback — the
-- server running at that point does not consult the column — but an operator
-- reaching for it during an incident should know it is not neutral.
DROP INDEX IF EXISTS atlantis.jobs_dead_owner_caller_idx;
DROP INDEX IF EXISTS atlantis.jobs_owner_caller_idx;

ALTER TABLE atlantis.jobs_dead DROP COLUMN IF EXISTS owner_caller;
ALTER TABLE atlantis.jobs      DROP COLUMN IF EXISTS owner_caller;
