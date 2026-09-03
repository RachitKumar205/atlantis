ALTER TABLE atlantis.schema_plans
    DROP COLUMN IF EXISTS requested_by_actor,
    DROP COLUMN IF EXISTS decided_via;
DROP TABLE IF EXISTS atlantis.freeze_windows;
DROP TABLE IF EXISTS atlantis.protected_entities;
