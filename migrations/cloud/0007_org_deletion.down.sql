-- Reverse of 0007.
--
-- Dropping these columns does not delete anything and does not restore
-- anything. What it does is strand whatever was mid-window: a row still reading
-- `deleted` loses the date that would have destroyed it, so it sits in that
-- state for ever — not serving, not destroyed, and invisible to a reaper that
-- no longer has a column to select on.
--
-- A deployment running this down should first decide what to do with those
-- rows. Setting them back to 'ready' returns them to service, since their
-- namespaces were never touched; that is almost certainly what is wanted, and
-- it is deliberately not done here because it is a data decision rather than a
-- schema one.
--
--     UPDATE cloud.org_provisioning SET state = 'ready' WHERE state = 'deleted';
--
-- Run that before this, or accept the strand.

-- The state constraint goes back to the four 0005 knew about, and that is why
-- the UPDATE above is not optional: narrowing the CHECK while a row still reads
-- 'deleted' or 'purging' fails outright, and this migration will not apply.
--
-- That is the better failure. The alternative — leaving the constraint wide —
-- would let a state exist that no code recognises.

DROP INDEX IF EXISTS cloud.org_provisioning_purge_after;

ALTER TABLE cloud.org_provisioning
    DROP COLUMN IF EXISTS purge_after,
    DROP COLUMN IF EXISTS deleted_at;

ALTER TABLE cloud.org_provisioning
    DROP CONSTRAINT IF EXISTS org_provisioning_state_check;

ALTER TABLE cloud.org_provisioning
    ADD CONSTRAINT org_provisioning_state_check
    CHECK (state IN ('pending', 'provisioning', 'ready', 'failed'));
