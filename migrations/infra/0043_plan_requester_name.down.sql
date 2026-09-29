ALTER TABLE atlantis.schema_plans
  DROP COLUMN IF EXISTS requested_by_actor_name,
  DROP COLUMN IF EXISTS requested_by_actor_email;
