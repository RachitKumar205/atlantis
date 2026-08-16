-- owner_caller on workflow_instances: the half of 0028 that was missed.
--
-- 0028 closed the cross-caller read on atlantis.jobs and jobs_dead, and its
-- header states the reasoning at length: CAPABILITY_JOBS_READ is in the base
-- bundle every registered caller receives, so a read path that selects by id
-- with no owner predicate lets any caller read any other caller's rows.
--
-- atlantis.workflow_instances is the sibling table in that same premise and it
-- was left alone. GetWorkflowStatus selected `WHERE id = $1` and nothing else,
-- and the ids are a bigint sequence — so iterating 1, 2, 3 returned another
-- caller's workflow_name, status, current_step, submitted_by and error_msg.
-- error_msg is handler failure text, which is exactly the content 0028's
-- header calls out as the reason the jobs fix mattered.
--
-- DEFAULT '' carries the same meaning it does in 0028: "started before this
-- migration, owner not recorded". Reads scope on equality, so '' matches no
-- caller and legacy rows stay visible only to operators, who are not scoped.
-- Backfilling from submitted_by is rejected for the same reason as there — it
-- would promote a request-body string into the column whose whole purpose is
-- that the caller cannot choose it.
ALTER TABLE atlantis.workflow_instances
    ADD COLUMN IF NOT EXISTS owner_caller TEXT NOT NULL DEFAULT '';

-- The read path filters on this for every non-operator request.
CREATE INDEX IF NOT EXISTS workflow_instances_owner_caller_idx
    ON atlantis.workflow_instances (owner_caller, started_at DESC);
