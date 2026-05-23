-- Workflow orchestration tables.
--
-- A workflow instance is a running execution of a declared workflow.
-- Each step enqueues a job into atlantis.jobs with workflow_id +
-- workflow_step set; when the worker marks that job complete (or
-- DLQ'd), the post-completion hook advances (or compensates) the
-- workflow.

CREATE TABLE IF NOT EXISTS atlantis.workflow_instances (
    id              BIGSERIAL PRIMARY KEY,
    workflow_name   TEXT NOT NULL,
    state           JSONB NOT NULL DEFAULT '{}'::jsonb,
    status          TEXT NOT NULL DEFAULT 'running'
                    CHECK (status IN ('running','completing','complete','compensating','failed','cancelled')),
    current_step    TEXT,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,
    error_msg       TEXT,
    submitted_by    TEXT
);

CREATE INDEX IF NOT EXISTS workflow_instances_status_idx
    ON atlantis.workflow_instances (status, started_at DESC);
CREATE INDEX IF NOT EXISTS workflow_instances_name_idx
    ON atlantis.workflow_instances (workflow_name, started_at DESC);

-- Track which steps have completed so compensation knows which to
-- undo. One row per step that ran successfully; the engine inserts
-- on step completion and reads in reverse order during compensation.
CREATE TABLE IF NOT EXISTS atlantis.workflow_step_history (
    id              BIGSERIAL PRIMARY KEY,
    workflow_id     BIGINT NOT NULL REFERENCES atlantis.workflow_instances(id) ON DELETE CASCADE,
    step_name       TEXT NOT NULL,
    job_id          BIGINT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'complete'
                    CHECK (status IN ('complete','compensated','failed')),
    completed_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS workflow_step_history_wf_idx
    ON atlantis.workflow_step_history (workflow_id, completed_at);
