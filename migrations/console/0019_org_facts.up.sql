-- What the fleet poller last observed about each organisation.
--
-- One row per organisation, overwritten each sweep. Operating N organisations
-- otherwise means opening N databases: each tenant's atlantis schema is the
-- only record of its schema version, its dead-letter queue and its parked
-- objects, and nothing central holds any of it.
--
-- A copy, not a move. ApplyMigration writes ir_checkpoint and schema_versions
-- in the same transaction as the DDL, so those tables stay where they are.
-- facts_at is what says how old this copy is.
--
-- Every fact column is nullable and NULL means the poll did not measure it.
-- That is a different fact from zero: an organisation with no dead jobs and an
-- organisation whose admin plane refused the call both read as 0 in a NOT NULL
-- integer column, and the second is the one worth paging on.
--
-- Nothing reads this table fleet-wide from Go. A bare SELECT on the pool binds
-- no organisation, console.current_org() is NULL, and the RESTRICTIVE policy
-- below admits no row — zero rows, no error. The gauges in
-- internal/console/metrics.go are the fleet view; this is what an operator
-- opens with psql when a gauge says an organisation is unreachable and the next
-- question is why.
CREATE TABLE IF NOT EXISTS console.org_facts (
    org                 TEXT        NOT NULL PRIMARY KEY
                                    REFERENCES console.orgs(org) ON DELETE CASCADE,

    -- When the sweep last tried, and when it last read every fact. Two
    -- columns because a failed or partial poll must not erase what was known:
    -- it writes collected_at and whichever facts it did read, and leaves
    -- facts_at, so a gap between the two says some numbers are stale.
    collected_at        TIMESTAMPTZ NOT NULL,
    facts_at            TIMESTAMPTZ,

    -- Whether the health listener answered. The poller always has an answer to
    -- that, so NOT NULL.
    reachable           BOOLEAN     NOT NULL,

    -- One of: unregistered, unprovisioned, dns, dial, tls, timeout, refused,
    -- rpc. NULL while reachable. The set is closed in
    -- internal/console/fleet.go; a CHECK here would make adding a kind a
    -- migration.
    unreachable_kind    TEXT,
    last_error          TEXT        NOT NULL DEFAULT '',

    -- From GET /status on the health listener.
    schema_version      BIGINT,
    server_version      TEXT        NOT NULL DEFAULT '',
    started_at          TIMESTAMPTZ,

    -- From ListDeadJobs. truncated is true when the response filled the
    -- poller's limit, which the response itself cannot say: it carries no total
    -- and no has_more, so a full page and an exactly-full queue are the same
    -- message.
    dead_jobs           INTEGER,
    dead_jobs_truncated BOOLEAN,

    -- From ListParkedObjects. overdue counts the rows whose reap_after has
    -- passed and which are still present, which is what the register is read
    -- for. truncated is the response's own has_more.
    parked_objects      INTEGER,
    parked_overdue      INTEGER,
    parked_truncated    BOOLEAN,

    -- Computed from ListFreezeWindows, which returns every row including
    -- expired ones and has no notion of now. ends_at is the latest end among
    -- the windows covering the poll, so a reader knows when it lifts.
    freeze_open         BOOLEAN,
    freeze_ends_at      TIMESTAMPTZ
);

-- Organisation isolation, the 0007 shape: a RESTRICTIVE boundary plus the
-- permissive grant. These rows carry a customer's schema version, their
-- dead-job backlog and their last admin-plane error — operational detail about
-- their system, which is why this table is policed where console.orgs is not.
ALTER TABLE console.org_facts ENABLE ROW LEVEL SECURITY;
ALTER TABLE console.org_facts FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS org_facts_org_isolation ON console.org_facts;
CREATE POLICY org_facts_org_isolation ON console.org_facts
    AS RESTRICTIVE
    USING (org = console.current_org())
    WITH CHECK (org = console.current_org());

DROP POLICY IF EXISTS org_facts_default_access ON console.org_facts;
CREATE POLICY org_facts_default_access ON console.org_facts
    AS PERMISSIVE USING (true) WITH CHECK (true);
