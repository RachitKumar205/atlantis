-- Cache invalidation outbox. A background worker drains rows via
-- LISTEN/NOTIFY, bumps the memcached version pointer, then deletes the row.

CREATE SCHEMA IF NOT EXISTS atlantis;

CREATE TABLE IF NOT EXISTS atlantis.cache_invalidations (
    id           BIGSERIAL PRIMARY KEY,
    entity       TEXT      NOT NULL,
    row_id       TEXT      NOT NULL,
    new_version  BIGINT    NOT NULL,
    enqueued_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts     INT       NOT NULL DEFAULT 0,
    last_error   TEXT
);

-- Drain workers select the oldest row first.
CREATE INDEX IF NOT EXISTS cache_invalidations_enqueued_idx
    ON atlantis.cache_invalidations (enqueued_at);

-- Partial index for the failure-sweep query.
CREATE INDEX IF NOT EXISTS cache_invalidations_attempts_idx
    ON atlantis.cache_invalidations (attempts) WHERE attempts > 0;

CREATE OR REPLACE FUNCTION atlantis.notify_cache_invalidation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('atl_cache_invalidations', NEW.id::text);
    RETURN NEW;
END;
$$;

-- PG has no CREATE TRIGGER IF NOT EXISTS; drop-then-create instead.
DROP TRIGGER IF EXISTS cache_invalidations_notify ON atlantis.cache_invalidations;
CREATE TRIGGER cache_invalidations_notify
    AFTER INSERT ON atlantis.cache_invalidations
    FOR EACH ROW EXECUTE FUNCTION atlantis.notify_cache_invalidation();
