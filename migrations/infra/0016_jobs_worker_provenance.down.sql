DROP INDEX IF EXISTS atlantis.jobs_worker_session_idx;

ALTER TABLE atlantis.jobs
    DROP COLUMN IF EXISTS worker_session_id,
    DROP COLUMN IF EXISTS worker_kind;
