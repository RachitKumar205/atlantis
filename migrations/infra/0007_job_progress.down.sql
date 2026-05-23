ALTER TABLE atlantis.jobs
    DROP COLUMN IF EXISTS progress_pct,
    DROP COLUMN IF EXISTS progress_msg,
    DROP COLUMN IF EXISTS progress_at;
