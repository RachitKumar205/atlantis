-- Let an operator ask for the console's credentials to be replaced.
--
-- A request, not a command. The credentials live in a Secret in the
-- organisation's namespace and only the provisioner holds Kubernetes
-- credentials, so `cloud org rotate-console` marks the row and the provisioner
-- acts on its next reconcile pass. Same shape as purge_after.
--
-- Automatic rotation triggers on remaining certificate life, which does not
-- cover a credential believed to have leaked.
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
