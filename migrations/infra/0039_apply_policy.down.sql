ALTER TABLE atlantis.schema_versions
    DROP COLUMN IF EXISTS applied_under_policy,
    DROP COLUMN IF EXISTS applied_verdict;
DROP TABLE IF EXISTS atlantis.policy_events;
ALTER TABLE atlantis.caller_identities
    DROP COLUMN IF EXISTS apply_policy;
