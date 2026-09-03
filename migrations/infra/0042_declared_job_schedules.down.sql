ALTER TABLE atlantis.job_schedules
  DROP CONSTRAINT IF EXISTS job_schedules_managed_by_check;

ALTER TABLE atlantis.job_schedules
  DROP COLUMN IF EXISTS managed_by,
  DROP COLUMN IF EXISTS owner_caller,
  DROP COLUMN IF EXISTS timeout_ms,
  DROP COLUMN IF EXISTS max_retries,
  DROP COLUMN IF EXISTS queue;
