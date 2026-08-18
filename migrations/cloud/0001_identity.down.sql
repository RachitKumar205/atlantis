-- Reverses 0001. Drops the schema and everything Cloud knows about who anyone
-- is: every account, every organisation, every membership.
--
-- CASCADE because the tables reference each other, and dropping the schema is
-- the honest expression of what this does — a partial teardown that left users
-- without memberships would be a worse state than either end.
DROP SCHEMA IF EXISTS cloud CASCADE;
