-- What a database looked like when somebody pointed atlantis at it.
--
-- Onboarding reads a customer's Postgres over a connection string they supply
-- and hands back .atl describing it. Keeping the result is what lets them close
-- the tab, come back, and still have it.
--
-- This is a class of data Cloud has not held before: the table and column names
-- of a database it does not run. Three rules follow from that, and each is
-- enforced here rather than left to the handler.
--
--   * The connection string is never stored. `source` holds the host and port
--     alone, because that is what a person needs to tell two imports apart, and
--     a column that could hold a DSN is a column that eventually does.
--   * Everything cascades from cloud.users, so deleting an account takes the
--     schema with it. Same rule as sessions, identities and email tokens.
--   * Rows expire. An import is a snapshot of somebody else's database, and
--     keeping it after it stops being useful is holding their schema for no
--     reason.

CREATE TABLE IF NOT EXISTS cloud.schema_imports (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES cloud.users(id) ON DELETE CASCADE,

    -- Host and port, never credentials. CHECK rather than convention: '@'
    -- appears in every URL-form DSN and in no host:port, so a connection
    -- string written here fails the insert instead of being stored.
    source     TEXT NOT NULL CHECK (source !~ '@'),

    -- What the run produced, for a list that does not need to read every
    -- declaration to say how big an import was.
    entities   INTEGER NOT NULL DEFAULT 0,
    namespace  TEXT NOT NULL,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- When this stops being kept. Stored rather than computed from created_at
    -- and the running binary's window, so shortening the default later cannot
    -- move the expiry of a row already written — the same reasoning as
    -- org_provisioning.purge_after in 0007.
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS schema_imports_user
    ON cloud.schema_imports (user_id, created_at DESC);

-- Drives whatever reaps expired rows.
CREATE INDEX IF NOT EXISTS schema_imports_expires_at
    ON cloud.schema_imports (expires_at);

-- One generated declaration per table.
--
-- A table of its own rather than a JSON column on the import: the console lists
-- these one at a time, and a document that has to be parsed to count its
-- members is one nothing can index.
CREATE TABLE IF NOT EXISTS cloud.schema_import_entities (
    import_id   TEXT NOT NULL REFERENCES cloud.schema_imports(id) ON DELETE CASCADE,

    -- The physical table, schema-qualified, and the entity name proposed for
    -- it. Both come from the customer's catalogue.
    table_name  TEXT NOT NULL,
    entity_name TEXT NOT NULL,

    -- The declaration, as generated. Reviewed before it is committed anywhere,
    -- so it is stored exactly as produced.
    atl         TEXT NOT NULL,

    PRIMARY KEY (import_id, table_name)
);

-- Both tables hold rows belonging to one account, so both carry the boundary.
--
-- The store scopes every read by user_id already. That is the WHERE clause this
-- exists to survive: one Cloud process authenticates everybody, and a query
-- that forgets is not observable — the page renders either way. policyguard
-- refuses to start if a table here is neither policed nor exempt, which is what
-- caught these being added without it.
--
-- ENABLE plus FORCE. Without FORCE the owning role, which is the role Cloud
-- connects as, reads straight through while `\d` still lists the policy.
ALTER TABLE cloud.schema_imports ENABLE ROW LEVEL SECURITY;
ALTER TABLE cloud.schema_imports FORCE ROW LEVEL SECURITY;
ALTER TABLE cloud.schema_import_entities ENABLE ROW LEVEL SECURITY;
ALTER TABLE cloud.schema_import_entities FORCE ROW LEVEL SECURITY;

-- RESTRICTIVE for the boundary, permissive for the grant. Postgres admits a row
-- when ANY permissive policy allows AND EVERY restrictive one does, so a
-- boundary written permissive is a grant that the next policy widens past.
CREATE POLICY schema_imports_user_isolation ON cloud.schema_imports
    AS RESTRICTIVE USING (user_id = cloud.current_user_id());
CREATE POLICY schema_imports_default_access ON cloud.schema_imports
    AS PERMISSIVE USING (true);

-- The entities carry no user_id of their own, so the boundary reaches through
-- the import they belong to. Duplicating the column would be a second copy to
-- keep true, and a row whose two answers disagree is one nothing can resolve.
CREATE POLICY schema_import_entities_user_isolation ON cloud.schema_import_entities
    AS RESTRICTIVE USING (
        EXISTS (
            SELECT 1 FROM cloud.schema_imports i
            WHERE i.id = import_id AND i.user_id = cloud.current_user_id()
        )
    );
CREATE POLICY schema_import_entities_default_access ON cloud.schema_import_entities
    AS PERMISSIVE USING (true);
