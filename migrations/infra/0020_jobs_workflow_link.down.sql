-- Rolling back returns the workflow feature to non-functional, which is the
-- state it was in before 0020. Any in-flight workflow becomes unadvanceable:
-- its instance row survives but the jobs lose their step linkage, so the
-- post-completion hook stops firing. That is inherent to reverting the column,
-- not something the down migration can soften.
DROP INDEX IF EXISTS atlantis.jobs_workflow_idx;

ALTER TABLE atlantis.jobs
    DROP COLUMN IF EXISTS workflow_step,
    DROP COLUMN IF EXISTS workflow_id;
