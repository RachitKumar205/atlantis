-- Rolling back discards every attribution recorded since 0036. The caller on
-- each row survives; the person does not.
ALTER TABLE atlantis.schema_versions
    DROP CONSTRAINT IF EXISTS schema_versions_actor_scheme;

ALTER TABLE atlantis.schema_versions
    DROP COLUMN IF EXISTS actor_email,
    DROP COLUMN IF EXISTS actor;
