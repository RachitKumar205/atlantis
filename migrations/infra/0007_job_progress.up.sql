-- Long-running job support: per-attempt progress reporting.
--
-- atlantis.jobs gains three columns the handler can write through a
-- Checkpoint helper while it works. The columns are NULLable so
-- existing rows from before this migration stay valid; in-flight jobs
-- that don't call Checkpoint just leave the fields NULL.
--
-- progress_pct is bounded 0..100; progress_msg is a free-form short
-- string the operator sees in `tide job status`. progress_at is the
-- write timestamp so a stale-progress alert can fire when a handler
-- claims a long timeout but stops reporting.
ALTER TABLE atlantis.jobs
    ADD COLUMN IF NOT EXISTS progress_pct  SMALLINT
        CHECK (progress_pct IS NULL OR (progress_pct >= 0 AND progress_pct <= 100)),
    ADD COLUMN IF NOT EXISTS progress_msg  TEXT,
    ADD COLUMN IF NOT EXISTS progress_at   TIMESTAMPTZ;
