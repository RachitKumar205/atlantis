-- Let an operator ask for the console's credentials to be replaced.
--
-- ── Why this is a request rather than a command ────────────────────────────
--
-- The credentials live in a Secret in the organisation's namespace, and Cloud
-- cannot reach it. Only the provisioner holds Kubernetes credentials — that is
-- the point of the scoped service account it runs under, and widening Cloud's
-- access so that one command could write a Secret would undo it.
--
-- So this follows purge_after exactly: `cloud org rotate-console` marks the row
-- and the provisioner acts on its next reconcile pass. The operator's command
-- returns immediately and the work happens where the credentials are.
--
-- ── Why rotation is not simply left to expiry ──────────────────────────────
--
-- Automatic rotation is driven by how much life a certificate has left, which
-- is the right trigger for the ordinary case and the wrong one for the case
-- that matters. An operator who believes a credential has leaked needs it
-- replaced now, and "wait for the renewal window" is not an answer — before
-- this column there was no way to ask at all.
ALTER TABLE cloud.org_provisioning
    ADD COLUMN IF NOT EXISTS console_rotate_requested_at TIMESTAMPTZ;

COMMENT ON COLUMN cloud.org_provisioning.console_rotate_requested_at IS
    'Set by `cloud org rotate-console`. The provisioner reissues the console''s '
    'certificates on its next reconcile pass and clears this. NULL means no '
    'request is outstanding; automatic renewal near expiry happens regardless.';

-- Partial index over outstanding requests only.
--
-- reconcile asks "which organisations have asked for a rotation" on every pass,
-- and a request is a rare, short-lived state — set by a human, cleared within
-- one interval. Indexing only the rows that have one keeps this small enough to
-- stay cached, and the alternative is a sequential scan over the whole fleet
-- every reconcile interval for a column that is almost always NULL.
CREATE INDEX IF NOT EXISTS org_provisioning_console_rotate_idx
    ON cloud.org_provisioning (org)
    WHERE console_rotate_requested_at IS NOT NULL;
