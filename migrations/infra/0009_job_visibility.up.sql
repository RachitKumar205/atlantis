-- Per-job RBAC: which callers are allowed to submit which jobs.
--
-- Seeded at `tide apply` time from the DSL `visible_to` modifier.
-- Operator can INSERT/DELETE directly for runtime overrides without
-- redeploying schema. A wildcard row (caller='*') grants access to
-- every caller.
CREATE TABLE IF NOT EXISTS atlantis.job_visibility (
    job_name    TEXT NOT NULL,
    caller      TEXT NOT NULL,
    PRIMARY KEY (job_name, caller)
);
