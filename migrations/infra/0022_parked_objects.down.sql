-- Dropping the register does not un-park anything: the parked tables and
-- columns stay exactly where they are. It only removes the record of them, so
-- nothing will ever reap them and no list says they exist.
--
-- So this refuses while any live registration remains, rather than warning
-- about it in a comment. CI exercises up -> down -> up, and a comment does not
-- survive that: the cycle would silently produce an empty register alongside a
-- tombstone schema full of tables that are now permanently invisible.
--
-- To proceed deliberately: reverse the migrations that parked the objects
-- (which restores them and clears their rows), or reap them early with
--   UPDATE atlantis.parked_objects SET reap_after = now() WHERE ...
-- and let the reaper run.
DO $$
DECLARE
    live integer;
BEGIN
    IF to_regclass('atlantis.parked_objects') IS NULL THEN
        RETURN;
    END IF;
    SELECT count(*) INTO live FROM atlantis.parked_objects WHERE reaped_at IS NULL;
    IF live > 0 THEN
        RAISE EXCEPTION 'atlantis: % object(s) are still parked. Dropping the '
            'register would strand them in the tombstone schema with nothing to '
            'reap them and no record that they exist. Restore or reap them first.',
            live;
    END IF;
END $$;

DROP TABLE IF EXISTS atlantis.parked_objects;
