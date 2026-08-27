-- What a database looked like when this organisation pointed atlantis at it.
--
-- Onboarding reads a customer's existing Postgres over a connection string they
-- supply and hands back .atl describing it. Keeping the result is what lets them
-- close the tab and come back, and what lets the declarations become a commit.
--
-- Scoped to the organisation, not to a person. The .atl targets this
-- organisation's atlantis, so an import belongs to the organisation the way a
-- schema version does — and anybody who can reach this console for that
-- organisation is somebody who may read it.
--
-- Three rules, each enforced here rather than left to a handler:
--
--   * The connection string is never stored. `source` holds host and port
--     alone, because that is what tells two imports apart, and a column that
--     could hold a DSN is a column that eventually does.
--   * Row-level security by organisation, like every other console table
--     holding per-organisation rows. See 0004.
--   * Rows expire. An import is a snapshot of a database this console does not
--     run, and keeping it after it stops being useful is holding somebody
--     else's schema for no reason.

CREATE TABLE IF NOT EXISTS console.schema_imports (
    id         TEXT PRIMARY KEY,
    org        TEXT NOT NULL,

    -- Host and port, never credentials. CHECK rather than convention: '@'
    -- appears in every URL-form DSN and in no host:port, so a connection
    -- string written here fails the insert instead of being stored.
    source     TEXT NOT NULL CHECK (source !~ '@'),

    -- The namespace the entities were generated into, and how many there were.
    namespace  TEXT NOT NULL,
    entities   INTEGER NOT NULL DEFAULT 0,

    -- Who ran it, as Cloud's subject. The audit actor, matching how every other
    -- console row records one.
    actor      TEXT NOT NULL DEFAULT '',

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Stored rather than computed from created_at and the running binary's
    -- window, so shortening the default later cannot move the expiry of a row
    -- already written.
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS schema_imports_org
    ON console.schema_imports (org, created_at DESC);

-- Drives whatever reaps expired rows.
CREATE INDEX IF NOT EXISTS schema_imports_expires_at
    ON console.schema_imports (expires_at);

-- One generated declaration per table.
--
-- A table of its own rather than a JSON column on the import: these are listed
-- one at a time, and a document that has to be parsed to count its members is
-- one nothing can index.
CREATE TABLE IF NOT EXISTS console.schema_import_entities (
    import_id   TEXT NOT NULL REFERENCES console.schema_imports(id) ON DELETE CASCADE,

    -- Carried so the boundary can be enforced on this table directly. Reaching
    -- through the import would make every read a join, and a policy that has to
    -- join is one that gets dropped for performance.
    org         TEXT NOT NULL,

    table_name  TEXT NOT NULL,
    entity_name TEXT NOT NULL,

    -- The declaration, as generated. Reviewed before it is committed anywhere,
    -- so it is stored exactly as produced.
    atl         TEXT NOT NULL,

    PRIMARY KEY (import_id, table_name)
);

CREATE INDEX IF NOT EXISTS schema_import_entities_org
    ON console.schema_import_entities (org);

-- The organisation boundary, as 0004 established it for console.audit_log.
--
-- ENABLE plus FORCE: without FORCE the owning role, which is the role this
-- console connects as, reads straight through while `\d` still lists the
-- policy. RESTRICTIVE for the boundary and PERMISSIVE for the grant, because
-- Postgres admits a row when ANY permissive policy allows AND EVERY restrictive
-- one does — a boundary written permissive is a grant the next policy widens
-- past.
ALTER TABLE console.schema_imports ENABLE ROW LEVEL SECURITY;
ALTER TABLE console.schema_imports FORCE ROW LEVEL SECURITY;
ALTER TABLE console.schema_import_entities ENABLE ROW LEVEL SECURITY;
ALTER TABLE console.schema_import_entities FORCE ROW LEVEL SECURITY;

CREATE POLICY schema_imports_org_isolation ON console.schema_imports
    AS RESTRICTIVE
    USING (org = console.current_org())
    WITH CHECK (org = console.current_org());
CREATE POLICY schema_imports_default_access ON console.schema_imports
    AS PERMISSIVE USING (true) WITH CHECK (true);

CREATE POLICY schema_import_entities_org_isolation ON console.schema_import_entities
    AS RESTRICTIVE
    USING (org = console.current_org())
    WITH CHECK (org = console.current_org());
CREATE POLICY schema_import_entities_default_access ON console.schema_import_entities
    AS PERMISSIVE USING (true) WITH CHECK (true);
