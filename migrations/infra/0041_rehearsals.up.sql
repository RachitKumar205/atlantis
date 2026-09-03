-- Rehearsal: a migration executed against a disposable clone of the managed
-- database, so the verdict comes from what Postgres actually did.

-- Where clones are created. The 0035 shape: the DSN is sealed, source is the
-- redacted host:port for display, version drives pool reopening. Unset, the
-- server falls back to the managed database itself when its role can
-- CREATE DATABASE.
CREATE TABLE IF NOT EXISTS atlantis.rehearsal_database (
    id         INTEGER     PRIMARY KEY CHECK (id = 1),
    dsn_sealed BYTEA       NOT NULL,
    source     TEXT        NOT NULL DEFAULT '',
    version    BIGINT      NOT NULL DEFAULT 1,
    set_by     TEXT        NOT NULL DEFAULT '',
    set_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The clone registry, written BEFORE the database is created (the 0022
-- parked-objects lesson: a clone created before it is recorded is one the
-- reaper can never see). Correctness never reads a clone — verdicts live in
-- atlantis.rehearsals — so a leaked clone costs disk, not policy.
CREATE TABLE IF NOT EXISTS atlantis.rehearsal_clones (
    clone_id      TEXT        PRIMARY KEY,
    db_name       TEXT        NOT NULL,
    caller        TEXT        NOT NULL DEFAULT '',
    target_source TEXT        NOT NULL DEFAULT '',
    state         TEXT        NOT NULL DEFAULT 'creating',  -- creating | dropped
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL,
    rehearsal_id  TEXT        NOT NULL DEFAULT ''
);

-- One rehearsal: the verdict and what produced it. plan_id is the binding —
-- the deterministic digest of (caller, files hash, dependency hash), the
-- same identity approvals are keyed by — so a verdict simply does not exist
-- for content that has moved.
--
-- error_message carries SQLSTATE, message and the constraint/table/column
-- names. error_detail carries the DETAIL and HINT lines, which embed row
-- values (`Key (email)=(alice@…)`); list responses never return it.
CREATE TABLE IF NOT EXISTS atlantis.rehearsals (
    rehearsal_id         TEXT        PRIMARY KEY,
    plan_id              TEXT        NOT NULL,
    caller               TEXT        NOT NULL,
    files_hash           TEXT        NOT NULL,
    base_dependency_hash TEXT        NOT NULL DEFAULT '',
    verdict              TEXT        NOT NULL,
    reason               TEXT        NOT NULL DEFAULT '',
    sqlstate             TEXT        NOT NULL DEFAULT '',
    error_message        TEXT        NOT NULL DEFAULT '',
    error_detail         TEXT        NOT NULL DEFAULT '',
    diagnostics          JSONB       NOT NULL DEFAULT '{}',
    remediation          TEXT        NOT NULL DEFAULT '',
    outcome              TEXT        NOT NULL DEFAULT '',  -- '' | applied | apply_failed
    clone_ms             BIGINT      NOT NULL DEFAULT 0,
    execute_ms           BIGINT      NOT NULL DEFAULT 0,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at           TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS rehearsals_binding_idx
    ON atlantis.rehearsals (plan_id, created_at DESC);

-- The plan row carries its latest rehearsal, so the approval queue renders a
-- verdict without a join the console would otherwise make per row.
ALTER TABLE atlantis.schema_plans
    ADD COLUMN rehearsal_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN verdict      TEXT NOT NULL DEFAULT '';

-- The console triggers rehearsals from the approval queue. Per-caller
-- enablement is a deliberate grant an admin makes through SetApplyPolicy —
-- rehearsal output is a cross-caller data surface, so it is in no default
-- bundle and gets no backfill.
INSERT INTO atlantis.caller_capabilities (caller, capability, granted_by)
VALUES ('atlantis-console', 'CAPABILITY_SCHEMA_REHEARSE', '<migration:0041>')
ON CONFLICT DO NOTHING;
