-- Deleting an organisation, with a window in which it can be undone.
--
-- `Kube.Destroy` has existed since the Kubernetes backend was written and
-- nothing outside a test has ever called it. So an organisation could be
-- created and never removed: its namespace, its Postgres cluster, its volume
-- and its certificate authority persisted for ever. These two columns are what
-- lets that be closed without making the close irreversible.

-- purge_after is stored rather than computed as deleted_at plus the running
-- binary's window, so that shortening the default later cannot move the
-- destruction date of an organisation already inside its window.
--
-- deleted_at is kept even though purge_after alone drives the reaper: it is
-- what an audit reads.
--
-- Both nullable, being meaningless outside state 'deleted'. No CHECK ties them
-- to the state, because a row legitimately carries purge_after in 'purging' as
-- well, and a constraint spanning three columns would need relaxing the first
-- time a state is added.
--
-- A deleted organisation keeps its row, with two consequences:
--
--   * its name stays taken until the row goes, which is correct while the
--     namespace exists, since two organisations resolving to org-acme would
--     collide in the cluster.
--   * it still counts against cloud.users.org_limit. The namespace, database
--     and volume remain reserved, so ignoring deleted organisations would let
--     one account hold unbounded capacity by deleting and recreating.
--
-- `cloud org purge` releases both.
ALTER TABLE cloud.org_provisioning
    ADD COLUMN IF NOT EXISTS deleted_at  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS purge_after TIMESTAMPTZ;

-- The two new states.
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
