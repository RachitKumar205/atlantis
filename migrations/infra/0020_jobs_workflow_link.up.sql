-- Add the columns that link a job to the workflow step that enqueued it.
--
-- Migration 0010 created workflow_instances and workflow_step_history, and its
-- header describes the design accurately: "Each step enqueues a job into
-- atlantis.jobs with workflow_id + workflow_step set; when the worker marks
-- that job complete (or DLQ'd), the post-completion hook advances (or
-- compensates) the workflow."
--
-- It never added those two columns to atlantis.jobs.
--
-- So the workflow feature has never run. Every path that touches it fails on
-- the same missing columns:
--
--   * admin.StartWorkflow's job INSERT errors with
--     `column "workflow_id" of relation "jobs" does not exist`, so no workflow
--     can be started at all.
--   * jobs/workflow.go:69 selects COALESCE(workflow_id, 0) from atlantis.jobs
--     to decide whether a completed job belongs to a workflow, so the
--     post-completion hook could not fire even if a job existed.
--   * jobs/workflow.go:139 enqueues the next step the same way.
--
-- The columns are nullable because the overwhelming majority of jobs are not
-- workflow steps. NULL means "an ordinary job", which is exactly what the
-- COALESCE in workflow.go already assumes.
ALTER TABLE atlantis.jobs
    ADD COLUMN IF NOT EXISTS workflow_id   BIGINT
                             REFERENCES atlantis.workflow_instances(id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS workflow_step TEXT;

-- ON DELETE CASCADE matches workflow_step_history and is the safer direction
-- here: a job whose workflow row is gone is a step of an execution that no
-- longer exists, and leaving it queued would have a worker run a step that can
-- never be advanced or compensated. Job rows for ordinary work are unaffected —
-- their workflow_id is NULL and the constraint does not apply.

-- The engine looks up a workflow's jobs when advancing and when compensating.
-- Partial, because only workflow steps carry the column and indexing the NULLs
-- would be most of the table for none of the benefit.
CREATE INDEX IF NOT EXISTS jobs_workflow_idx
    ON atlantis.jobs (workflow_id, workflow_step)
    WHERE workflow_id IS NOT NULL;
