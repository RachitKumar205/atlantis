-- schema_plans: schema changes waiting on a human.
--
-- A new table rather than a state column on schema_versions, and the reason is
-- what that table means. A schema_versions row says "this was applied":
-- entity_lineage carries a foreign key into version, parent_version
-- self-references it, and the numbers are read as the order things reached the
-- database. A pre-apply row would make version numbers non-monotonic with
-- respect to applied state — v8 pending while v9 is live — and every reader
-- that treats "highest version" as "current schema" would be wrong without
-- changing.
--
-- Rows here are created by ApplyMigration on its refusal path, not by
-- PlanSchema. PlanSchema is declared read-only in three places and is what the
-- console calls to render previews; making it enqueue would mean opening the
-- schema page files an approval request. Making the ATTEMPT TO APPLY enqueue
-- instead needs no new caller-facing RPC and leaves exactly one code path that
-- can create a pending plan.
CREATE TABLE IF NOT EXISTS atlantis.schema_plans (
    plan_id      TEXT        PRIMARY KEY,
    caller       TEXT        NOT NULL,
    change_class TEXT        NOT NULL,

    -- What the reviewer is deciding about. `files` is the proposed .atl source,
    -- not just a digest of it.
    --
    -- Storing the source is the difference between review and ceremony. A
    -- reviewer approves SCHEMA; up_sql is what the emitter makes of it, and a
    -- person asked to sign off on `ALTER TABLE ... DROP COLUMN` with no sight
    -- of the declaration that produced it is being asked to approve a diff they
    -- cannot check against intent.
    files      JSONB NOT NULL,
    files_hash TEXT  NOT NULL,
    diff       JSONB NOT NULL,
    up_sql     TEXT  NOT NULL DEFAULT '',
    down_sql   TEXT  NOT NULL DEFAULT '',

    -- The dependency hash the plan was computed against — callerDependencyHash's
    -- output, NOT atlantis.ir_checkpoint.content_hash.
    --
    -- That distinction is the whole reason an approval can survive the working
    -- day. The checkpoint hash covers every caller's schema merged together, so
    -- binding to it would expire an operator's decision the moment an unrelated
    -- team deployed — for a reason the approver could not see and nobody chose.
    -- The dependency hash moves only when something this caller reads moves.
    base_checkpoint_hash TEXT NOT NULL DEFAULT '',

    -- pending_approval | approved | rejected | applied | superseded
    --
    -- No CHECK, for the reason 0018 gives about capability names: constraining
    -- the set here means a migration every time the lifecycle grows a state,
    -- and the Go side already refuses to act on a state it does not recognise.
    state TEXT NOT NULL DEFAULT 'pending_approval',

    -- requested_by is the verified cert CN that tried to apply. decided_by is a
    -- console user, which this server cannot authenticate — it is recorded as
    -- given, so it is evidence rather than proof, and decided_by_role is what
    -- the console asserted it checked.
    requested_by    TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ,
    decided_by      TEXT        NOT NULL DEFAULT '',
    decided_by_role TEXT        NOT NULL DEFAULT '',
    decided_at      TIMESTAMPTZ,
    decision_reason TEXT        NOT NULL DEFAULT '',
    applied_version BIGINT      REFERENCES atlantis.schema_versions (version)
);

-- The console lists what is waiting, newest first.
CREATE INDEX IF NOT EXISTS schema_plans_state_idx
    ON atlantis.schema_plans (state, created_at DESC);

-- The apply path looks up this caller's outstanding plans.
CREATE INDEX IF NOT EXISTS schema_plans_caller_idx
    ON atlantis.schema_plans (caller, state);
