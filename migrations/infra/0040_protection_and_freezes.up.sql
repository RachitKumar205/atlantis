-- Protected entities: per-entity approval floors the gate consults beside
-- the change policy and the caller's tier.
--
-- pattern is an exact `namespace.Entity` or a `namespace.*` prefix,
-- normalized at write time. floor is 'require_approval' (a matching diff
-- waits for a human whatever else the policies say) or 'admin_only' (the
-- human must be an admin). No CHECK, per 0018; the gate treats an unknown
-- floor as 'admin_only'.
CREATE TABLE IF NOT EXISTS atlantis.protected_entities (
    pattern    TEXT        PRIMARY KEY,
    floor      TEXT        NOT NULL DEFAULT 'require_approval',
    reason     TEXT        NOT NULL DEFAULT '',
    created_by TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Freeze windows: absolute intervals during which matching classes do not
-- apply. Overlapping windows union. classes uses codegen's class names
-- ('additive', 'backfill-required', 'cross-caller-breaking', 'destructive');
-- empty means every class. Approving stays possible during a freeze — the
-- decision is not the action — so an approved change queues behind the
-- window rather than being lost.
CREATE TABLE IF NOT EXISTS atlantis.freeze_windows (
    id         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    starts_at  TIMESTAMPTZ NOT NULL,
    ends_at    TIMESTAMPTZ NOT NULL,
    -- The timezone the window was written in, for display; the interval
    -- itself is absolute.
    display_tz TEXT        NOT NULL DEFAULT '',
    classes    TEXT[]      NOT NULL DEFAULT '{}',
    reason     TEXT        NOT NULL DEFAULT '',
    created_by TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at)
);

-- requested_by_actor is the human attributed to the apply that filed the
-- plan — the untrusted actor field from 0036, recorded so a decision can be
-- compared against it. decided_via is '' for an ordinary decision,
-- 'override' for an admin forcing a gated plan through, and
-- 'self_approval_override' when the override also waived the requester
-- matching the approver.
ALTER TABLE atlantis.schema_plans
    ADD COLUMN requested_by_actor TEXT NOT NULL DEFAULT '',
    ADD COLUMN decided_via        TEXT NOT NULL DEFAULT '';
