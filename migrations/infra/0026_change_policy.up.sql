-- change_policy: which classes of schema change may apply without a human.
--
-- The gate this replaces was GitHub. A cross-caller-breaking plan told the
-- operator to "open a PR in the atlantis repo to coordinate the change", and
-- the documented human-review mechanism was branch protection on a repository
-- atlantis does not own and, in a hosted deployment, cannot see. That is not a
-- gate the product can ship; this table is where the rule lives instead.
--
-- It belongs in atlantis.* rather than console.*, and that placement is
-- load-bearing rather than tidy. The enforcement point is ApplyMigration,
-- which reads the rule inside the advisory-locked apply transaction so a
-- concurrent policy edit serialises against the apply it would change. The
-- admin server has no connection to the console's schema and should not grow
-- one; a gate that has to reach across a service boundary to find out whether
-- it applies is a gate that fails open when that service is down.
--
-- change_class holds the PlanClass enum's NAME ('PLAN_CLASS_DESTRUCTIVE'), not
-- its number, for the reason 0018 gives about capabilities: numbers are a wire
-- detail a future proto edit could reorder, and the name is self-describing to
-- whoever reads this table during an incident.
--
-- Deliberately no CHECK on the class values. Coupling the table to the enum
-- means a migration every time a class is added, and that friction is how the
-- policy stops covering every class. Safety comes from the Go side instead,
-- where a class with no row and a class whose name nobody recognises are
-- treated identically: both require approval. A typo cannot widen anything —
-- it can only add a row nothing reads, and leave the class it was meant to
-- describe failing closed.
CREATE TABLE IF NOT EXISTS atlantis.change_policy (
    change_class     TEXT        PRIMARY KEY,
    require_approval BOOLEAN     NOT NULL,
    -- The console role that may approve this class. The admin server cannot
    -- verify console roles — that identity system is the console's — so this
    -- is what the console must present rather than something checked against
    -- a human here. Recorded here anyway, because the decision it qualifies is
    -- made here, and because a role that lived only in console storage could
    -- be edited by a path the apply transaction never sees.
    approver_role    TEXT        NOT NULL DEFAULT 'admin',
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by       TEXT        NOT NULL DEFAULT ''
);

-- Seed one row per class the differ can actually produce.
--
-- PLAN_CLASS_UNSPECIFIED and PLAN_CLASS_UNPARSEABLE are absent on purpose.
-- Neither names a change: unparseable means the DSL did not compile, so the
-- apply is refused before any policy could apply to it, and unspecified is the
-- zero value. A row for either would be a rule nothing can ever consult.
--
-- Two of the three "true" values are a RELAXATION, not a tightening, and it is
-- worth being exact about that because it inverts the usual caution. Today
-- ApplyMigration hard-refuses cross-caller-breaking and destructive plans
-- outright — there is no path that applies them at all. A rule saying "a human
-- may approve this" is strictly more permissive than "never". Seeding them
-- true takes nothing away from anyone.
--
-- PLAN_CLASS_BACKFILL_REQUIRED is the one genuine tightening: `tide apply
-- --backfill` runs one today with no human involved, and a deployment whose CI
-- does that on every merge would stop dead. So it is seeded by install age.
-- A fresh install has no history to break and gets the safe default. An
-- upgrade keeps working exactly as it did, and the operator turns it on when
-- they have somewhere for the approval to go.
--
-- This departs from 0018's argument that a migration should correct the bug
-- rather than carry it forward, and the difference is which direction the
-- change runs. 0018 revoked a privilege that had been granted by accident;
-- this introduces a gate over a workflow that was deliberately permitted.
-- Fixing an over-grant silently is right. Adding a stop sign silently is not.
--
-- The `event_type <> 'seed'` test is the pattern 0012 uses: 0012 writes a seed
-- row into schema_versions for a pre-existing checkpoint, so the presence of
-- rows is not by itself evidence anyone has ever applied anything.
INSERT INTO atlantis.change_policy (change_class, require_approval, approver_role, updated_by)
VALUES
    ('PLAN_CLASS_ADDITIVE', false, 'admin', '<migration:0026>'),
    ('PLAN_CLASS_BACKFILL_REQUIRED',
        NOT EXISTS (SELECT 1 FROM atlantis.schema_versions WHERE event_type <> 'seed'),
        'admin', '<migration:0026>'),
    ('PLAN_CLASS_CROSS_CALLER_BREAKING', true, 'admin', '<migration:0026>'),
    ('PLAN_CLASS_DESTRUCTIVE', true, 'admin', '<migration:0026>')
ON CONFLICT (change_class) DO NOTHING;

-- The console is the surface a human edits the policy through, so it gets the
-- capability that permits editing it.
--
-- This is the deliberate act 0019 describes for OPERATOR, and it does not
-- collapse the separation the capability exists for. CAPABILITY_SCHEMA_APPROVE
-- is absent from both bundles RegisterCaller hands out, so no caller acquires
-- it by being registered, and 'atlantis-console' holds no CAPABILITY_SCHEMA_
-- APPLY — 0019 withheld it precisely so that a human applies through their own
-- caller rather than through the console. The identity that wants a change and
-- the identity that permits it therefore cannot be the same one, and a test in
-- internal/server/authz asserts that rather than leaving it to be noticed.
--
-- An operator running the console under a different CN grants this by hand,
-- exactly as they must already do for OPERATOR.
INSERT INTO atlantis.caller_capabilities (caller, capability, granted_by)
VALUES ('atlantis-console', 'CAPABILITY_SCHEMA_APPROVE', '<migration:0026>')
ON CONFLICT DO NOTHING;
