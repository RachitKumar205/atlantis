-- Per-caller apply policy: how much may happen unattended.
--
-- The change policy (0026) is the deployment-wide floor per plan class; this
-- is the per-caller ceiling. The gate takes the most restrictive answer of
-- the two, so neither can widen what the other closed.
--
-- Values: sandbox_only | always_ask | auto_safe | auto_verified | auto_all.
-- '' means the server default (auto_safe). No CHECK, for the reason 0018
-- gives about capability names; the gate treats an unknown non-empty value as
-- always_ask and logs it.
ALTER TABLE atlantis.caller_identities
    ADD COLUMN apply_policy TEXT NOT NULL DEFAULT '';

-- policy_events: append-only record of policy changes and overrides on this
-- server, beside the console's own audit log. Two ledgers, two trust domains:
-- the console cannot write here except through an RPC, and this survives a
-- console database that was lost or never deployed.
--
-- actor and actor_email are attribution the server cannot authenticate,
-- recorded as given (the schema_versions.actor reasoning in 0036).
CREATE TABLE IF NOT EXISTS atlantis.policy_events (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind        TEXT        NOT NULL,
    payload     JSONB       NOT NULL DEFAULT '{}',
    actor       TEXT        NOT NULL DEFAULT '',
    actor_email TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- What authorized an unattended apply, on the version row itself. Empty for
-- human-approved applies and for every row written before this existed; the
-- policy value is the tier that let it through, the verdict is the rehearsal
-- result it consumed.
ALTER TABLE atlantis.schema_versions
    ADD COLUMN applied_under_policy TEXT NOT NULL DEFAULT '',
    ADD COLUMN applied_verdict      TEXT NOT NULL DEFAULT '';
