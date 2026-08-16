-- Same caveat 0028's down carries, for the same reason: dropping the column
-- discards the ownership record and returns GetWorkflowStatus to reading
-- across every caller, because the code that scopes it ships with the code
-- that creates this column. Correct as a rollback — a server running at that
-- point does not consult the column — but not neutral, and an operator
-- reaching for it during an incident should know that.
DROP INDEX IF EXISTS atlantis.workflow_instances_owner_caller_idx;

ALTER TABLE atlantis.workflow_instances DROP COLUMN IF EXISTS owner_caller;
