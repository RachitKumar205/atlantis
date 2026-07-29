-- Dropping the table discards every grant, including any an operator made by
-- hand after the migration ran. can_mutate is untouched by the up migration
-- and still carries its original values, so rolling back returns the server
-- to the boolean gate rather than to no authorization at all.
DROP INDEX IF EXISTS atlantis.caller_capabilities_caller_idx;
DROP TABLE IF EXISTS atlantis.caller_capabilities;
