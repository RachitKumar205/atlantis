-- Dropping the table discards every rule, including edits an operator made
-- after the migration ran. Nothing reads the table on the way down: the code
-- that consults it ships with the code that creates it, so a deployment rolled
-- back to before 0026 has no policy to be missing.
--
-- The console's grant goes with it. Leaving CAPABILITY_SCHEMA_APPROVE behind
-- would be a grant to call an RPC that no longer has a table to write to, and
-- a capability outliving the thing it authorises is how a grant survives into
-- a version where it means something else.
DELETE FROM atlantis.caller_capabilities
 WHERE caller = 'atlantis-console'
   AND capability = 'CAPABILITY_SCHEMA_APPROVE';

DROP TABLE IF EXISTS atlantis.change_policy;
