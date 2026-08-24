-- Deleting an organisation, with a window in which it can be undone.
--
-- `Kube.Destroy` has existed since the Kubernetes backend was written and
-- nothing outside a test has ever called it. So an organisation could be
-- created and never removed: its namespace, its Postgres cluster, its volume
-- and its certificate authority persisted for ever. These two columns are what
-- lets that be closed without making the close irreversible.

-- ── When it was deleted, and when it may be destroyed ───────────────────────
--
-- Two columns rather than one, and the second is not derivable from the first.
--
-- purge_after could be computed as deleted_at + whatever window the running
-- binary is configured with. It is stored instead, because a window is a
-- promise made to somebody at the moment they pressed delete. Computing it
-- would mean shortening the default later silently moved the destruction date
-- of every organisation already in the window — including ones deleted under
-- the old promise, whose owners were told they had thirty days.
--
-- deleted_at is kept even though purge_after alone would drive the reaper. It
-- is what answers "when did this happen" in an audit, and it is the column a
-- support conversation starts from.
--
-- Both nullable, because they are meaningless in every state but `deleted`.
-- There is deliberately no CHECK tying them to the state: a row can legitimately
-- carry a purge_after in `purging` as well as in `deleted`, and a constraint
-- spanning three columns would have to be relaxed the first time a state is
-- added — which is how a constraint stops being trusted.
--
-- # Why the row survives deletion
--
-- A deleted organisation keeps its row, and that has two consequences worth
-- naming rather than discovering:
--
--   * its name stays taken, so the same name cannot be reused until the row
--     goes. That is correct while the namespace still exists — two
--     organisations resolving to org-acme would collide in the cluster.
--   * it still counts against cloud.users.org_limit. The namespace, database
--     and volume are still reserved, so a limit that ignored deleted
--     organisations would let one account hold unbounded capacity by deleting
--     and recreating.
--
-- `cloud org purge` is the escape hatch for both.
ALTER TABLE cloud.org_provisioning
    ADD COLUMN IF NOT EXISTS deleted_at  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS purge_after TIMESTAMPTZ;

-- ── The two new states ──────────────────────────────────────────────────────
--
-- 0005 wrote CHECK (state IN ('pending','provisioning','ready','failed')), so
-- the states are enumerated in the schema and not only in Go. That is the right
-- design — a typo in a state name fails at the database rather than becoming a
-- row nothing will ever claim — and it means this migration has to widen it.
--
-- Found by the tests rather than by reading: every deletion test failed with
-- `violates check constraint "org_provisioning_state_check"`. A comment in an
-- earlier draft of this file asserted the column had no enumeration behind it.
-- It did.
--
-- Dropped and recreated rather than altered, because Postgres has no
-- ALTER CONSTRAINT for a CHECK. The name is the one 0005 got by default, and it
-- is stated explicitly here so this does not silently do nothing on a database
-- where the constraint was created under another name.
ALTER TABLE cloud.org_provisioning
    DROP CONSTRAINT IF EXISTS org_provisioning_state_check;

ALTER TABLE cloud.org_provisioning
    ADD CONSTRAINT org_provisioning_state_check
    CHECK (state IN ('pending', 'provisioning', 'ready', 'failed',
                     'deleted', 'purging'));

-- The reaper's predicate, indexed.
--
-- Partial, because the reaper only ever asks about rows that have a
-- purge_after — which is a handful among every organisation that ever existed.
-- A full index here would be mostly nulls and would grow with the fleet rather
-- than with the backlog.
CREATE INDEX IF NOT EXISTS org_provisioning_purge_after
    ON cloud.org_provisioning (purge_after)
    WHERE purge_after IS NOT NULL;
