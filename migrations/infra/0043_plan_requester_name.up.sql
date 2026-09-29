-- The email and name of the person in requested_by_actor, as the apply that
-- filed the plan gave them, denormalised as schema_versions does in 0036 and
-- 0037. This server keeps no user records, so the plan is the only record of
-- the name.
ALTER TABLE atlantis.schema_plans
    ADD COLUMN IF NOT EXISTS requested_by_actor_email TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS requested_by_actor_name  TEXT NOT NULL DEFAULT '';
