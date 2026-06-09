-- Provenance columns on atlantis.jobs. Records which kind of worker
-- claimed each row and (for dispatcher-mode workers) which streaming
-- session owns it. Forensic value during incident response: "which
-- pod ran job 4913 — direct-PG worker, or a dispatched worker, and
-- which session?" Pre-feature rows have NULL for both columns; this
-- is the operator's signal that those rows ran before provenance was
-- being recorded.

ALTER TABLE atlantis.jobs
    ADD COLUMN IF NOT EXISTS worker_kind        TEXT,  -- 'direct_pg' | 'dispatched' | NULL
    ADD COLUMN IF NOT EXISTS worker_session_id  TEXT;  -- dispatcher session id; NULL for direct_pg

-- Partial index so the dispatcher's "release every row owned by this
-- session" path on session-disconnect is O(rows-for-this-session)
-- instead of a seq scan over the whole queue. Only dispatched rows
-- have a non-NULL session id, so the partial predicate keeps the
-- index tiny.
CREATE INDEX IF NOT EXISTS jobs_worker_session_idx
    ON atlantis.jobs (worker_session_id)
    WHERE worker_session_id IS NOT NULL;
