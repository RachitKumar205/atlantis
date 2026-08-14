-- Dropping the table discards every pending and approved plan.
--
-- That is the honest outcome rather than a loss worth guarding against: a
-- deployment rolled back to before 0027 has no code that would read these rows,
-- and an approval recorded for a gate that no longer exists is not an approval
-- of anything. Whoever rolls forward again re-requests, and a reviewer decides
-- against the schema as it stands then.
DROP INDEX IF EXISTS atlantis.schema_plans_caller_idx;
DROP INDEX IF EXISTS atlantis.schema_plans_state_idx;
DROP TABLE IF EXISTS atlantis.schema_plans;
