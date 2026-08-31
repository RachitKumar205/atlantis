-- Where this server's managed schema lives, when it is not this database.
--
-- An organisation that adopted an existing database keeps atlantis's own
-- tables here and reads, plans and applies against that one. The DSN carries a
-- password, so it is stored sealed and never in a column anything else reads.
--
-- One row. A server manages one database, and a table that could hold two
-- would need a rule for which one wins.
CREATE TABLE IF NOT EXISTS atlantis.managed_database (
    id          SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),

    -- Sealed with the server's keyring, associated data 'managed_database'.
    -- Never logged, never returned by an RPC: what reads it opens a pool with
    -- it and nothing else.
    dsn_sealed  BYTEA NOT NULL,

    -- Host and port, for the console and the logs to name the database without
    -- unsealing it. The CHECK is the one 0010 puts on schema_imports.source:
    -- '@' appears in every URL-form DSN and in no host:port, so a connection
    -- string written here fails the insert.
    source      TEXT NOT NULL CHECK (source !~ '@'),

    -- Bumped on every write. A server caches the pool it opened and reopens
    -- when this moves, so setting a new DSN takes effect without a restart.
    version     BIGINT NOT NULL DEFAULT 1,

    set_by      TEXT NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
