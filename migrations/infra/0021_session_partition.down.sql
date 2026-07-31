-- Dropping the setter before the table: policies referencing
-- current_partition() would error on any read once it is gone, so anything
-- that still has RLS enabled must be reverted before this runs.
DROP FUNCTION IF EXISTS atlantis.current_partition();
DROP FUNCTION IF EXISTS atlantis.set_partition(text);
DROP TABLE IF EXISTS atlantis.session_partition;
