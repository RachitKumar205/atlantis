-- Reverse of 0010: puts 0009's tables back.
--
-- Shape only. The imports themselves are gone, and nothing here can recover
-- them — they described databases this deployment does not run, and are re-read
-- by pointing atlantis at the connection string again.
--
-- Kept identical to 0009 so a deployment that steps down to 9 finds the schema
-- that version describes, policies included. A down migration that restored the
-- tables without the boundary would leave two tables of per-account rows with
-- no policy, which policyguard refuses to start against.

CREATE TABLE IF NOT EXISTS cloud.schema_imports (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES cloud.users(id) ON DELETE CASCADE,
    source     TEXT NOT NULL CHECK (source !~ '@'),
    entities   INTEGER NOT NULL DEFAULT 0,
    namespace  TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS schema_imports_user
    ON cloud.schema_imports (user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS schema_imports_expires_at
    ON cloud.schema_imports (expires_at);

CREATE TABLE IF NOT EXISTS cloud.schema_import_entities (
    import_id   TEXT NOT NULL REFERENCES cloud.schema_imports(id) ON DELETE CASCADE,
    table_name  TEXT NOT NULL,
    entity_name TEXT NOT NULL,
    atl         TEXT NOT NULL,
    PRIMARY KEY (import_id, table_name)
);

ALTER TABLE cloud.schema_imports ENABLE ROW LEVEL SECURITY;
ALTER TABLE cloud.schema_imports FORCE ROW LEVEL SECURITY;
ALTER TABLE cloud.schema_import_entities ENABLE ROW LEVEL SECURITY;
ALTER TABLE cloud.schema_import_entities FORCE ROW LEVEL SECURITY;

CREATE POLICY schema_imports_user_isolation ON cloud.schema_imports
    AS RESTRICTIVE USING (user_id = cloud.current_user_id());
CREATE POLICY schema_imports_default_access ON cloud.schema_imports
    AS PERMISSIVE USING (true);

CREATE POLICY schema_import_entities_user_isolation ON cloud.schema_import_entities
    AS RESTRICTIVE USING (
        EXISTS (
            SELECT 1 FROM cloud.schema_imports i
            WHERE i.id = import_id AND i.user_id = cloud.current_user_id()
        )
    );
CREATE POLICY schema_import_entities_default_access ON cloud.schema_import_entities
    AS PERMISSIVE USING (true);
