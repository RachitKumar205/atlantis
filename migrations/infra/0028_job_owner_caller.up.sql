-- owner_caller: which caller a job belongs to, recorded by the server.
--
-- atlantis.jobs already had submitted_by, and it looks like this column but is
-- not. submitted_by is written from the request body; cmd/tide fills it from
-- $USER. It is provenance for a human reading the queue, and it was also the
-- input to SubmitJob's `visible_to` check — which made that check advisory:
-- any authenticated caller could name whatever identity the job declared and
-- submit it. owner_caller is written from the mTLS identity the request
-- arrived under, which the caller cannot choose, so the two columns are kept
-- side by side rather than one replacing the other. They answer different
-- questions and only one of them is trustworthy.
--
-- The read side is the larger part. CAPABILITY_JOBS_READ is in the base
-- bundle every registered caller receives, and GetJobStatus and ListDeadJobs
-- selected by id with no owner predicate — so every caller could read every
-- other caller's job args and error text. In a product whose premise is that
-- callers do not see each other's data, that is the premise failing in the
-- one table it was never applied to.
--
-- DEFAULT '' rather than NULL, and the empty string means "submitted before
-- this migration, owner not recorded". Reads scope on equality, so '' matches
-- no caller and legacy rows become visible only to operators, who are not
-- scoped. That is the fail-closed direction: an operator keeps the whole
-- queue for triage, and a caller does not inherit rows this deployment cannot
-- attribute. Backfilling from submitted_by was rejected — it would promote an
-- unverified string into the column whose entire purpose is being verified.
ALTER TABLE atlantis.jobs
    ADD COLUMN IF NOT EXISTS owner_caller TEXT NOT NULL DEFAULT '';

ALTER TABLE atlantis.jobs_dead
    ADD COLUMN IF NOT EXISTS owner_caller TEXT NOT NULL DEFAULT '';

-- Both read paths filter on owner_caller for every non-operator request, so
-- it is on the hot path for any deployment with more than one caller.
CREATE INDEX IF NOT EXISTS jobs_owner_caller_idx
    ON atlantis.jobs (owner_caller);

CREATE INDEX IF NOT EXISTS jobs_dead_owner_caller_idx
    ON atlantis.jobs_dead (owner_caller, moved_at DESC);
