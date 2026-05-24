-- Generic key-value state table for lightweight sync watermarks and
-- flags that previously lived in Redis. Replaces the Redis dep for
-- the search syncer's incremental-sync cursor.
CREATE TABLE IF NOT EXISTS atlantis.sync_state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
