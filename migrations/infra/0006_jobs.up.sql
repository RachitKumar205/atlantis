-- Job runtime: in-Postgres queue for caller-declared `job` blocks.
--
-- atlantis.jobs is the canonical queue. Workers claim rows with
-- FOR UPDATE SKIP LOCKED + a heartbeat lease (claimed_until) so a
-- crashed pod's in-flight work becomes claimable by a peer after
-- the lease expires — same fault-tolerance pattern used by the
-- backfill worker.
--
-- atlantis.jobs_dead is the dead-letter table. Rows whose attempts
-- exceed max_retries are moved here so the live table stays small
-- and operators can inspect/retry poison messages via
-- `tide job dead` / `tide job retry`.
--
-- atlantis.job_schedules holds cron-driven jobs. The scheduler
-- component (singleton-elected via pg_try_advisory_lock) evaluates
-- specs and INSERTs into atlantis.jobs when a fire is due.
--
-- LISTEN/NOTIFY: every INSERT on atlantis.jobs broadcasts the row's
-- queue name on 'atl_jobs' so worker pools wake immediately. The
-- ticker fallback (1s) handles missed notifications across pod
-- restarts.

CREATE TABLE IF NOT EXISTS atlantis.jobs (
    id              BIGSERIAL PRIMARY KEY,
    -- Canonical "namespace.JobName" id. The worker registry uses
    -- this string to route to the handler the caller registered at
    -- server startup.
    job_name        TEXT NOT NULL,
    -- Named queue for partitioning worker pools (e.g. "shopify"
    -- vs "default"). Defaults to "default" when the caller's
    -- declaration omits it.
    queue           TEXT NOT NULL DEFAULT 'default',
    -- Typed args from the DSL `args { ... }` block, serialized as
    -- JSON. The worker deserializes back into the generated
    -- <Job>Args Go struct.
    args            JSONB NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','running','complete','failed')),
    attempts        INT NOT NULL DEFAULT 0,
    max_retries     INT NOT NULL,
    -- Per-attempt deadline in milliseconds. NULL means no timeout —
    -- the worker won't cancel the handler regardless of how long it
    -- runs. The DSL defaults to 30m when the declaration omits a
    -- timeout; emitting an unbounded job is opt-in via the `timeout
    -- none` form.
    timeout_ms      INT,
    last_error      TEXT,
    last_error_at   TIMESTAMPTZ,
    -- claimed_by is the pod id ("hostname-pid" or similar) that
    -- currently owns the row; claimed_until is the lease expiry.
    -- Lease semantics: a pod must heartbeat (extend claimed_until)
    -- to keep ownership; if it crashes, the row becomes claimable
    -- by another pod once now() > claimed_until.
    claimed_by      TEXT,
    claimed_until   TIMESTAMPTZ,
    -- scheduled_for lets the caller defer execution (delayed jobs).
    -- now() means "drain ASAP." The claim predicate filters on
    -- scheduled_for <= now() so future rows wait politely.
    scheduled_for   TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    enqueued_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Audit. Either a caller name ("vendor"), a cron submission tag
    -- ("schedule:0 */15 * * *"), or a procedure-driven enqueue tag
    -- ("procedure:vendor.SetDefaultMembership").
    submitted_by    TEXT
);

-- Drain workers query for pending rows whose scheduled_for has passed
-- and whose lease (if any) has expired. The partial-index condition
-- matches the claim WHERE clause exactly so the scan stays small even
-- when atlantis.jobs accumulates terminal rows the operator hasn't
-- archived yet.
CREATE INDEX IF NOT EXISTS jobs_pending_idx
    ON atlantis.jobs (queue, scheduled_for)
    WHERE status IN ('pending','running');

-- General-purpose lookup for status RPCs + ad-hoc queries.
CREATE INDEX IF NOT EXISTS jobs_status_idx
    ON atlantis.jobs (status, enqueued_at);

-- Per-job-name lookup so the status RPCs can paginate by job kind.
CREATE INDEX IF NOT EXISTS jobs_job_name_idx
    ON atlantis.jobs (job_name, enqueued_at DESC);


-- Dead-letter table. Schema mirrors atlantis.jobs (sans claim fields
-- which are irrelevant for a row that's no longer being worked) plus
-- moved_at for triage. Operators retry via UPDATE-back-to-jobs in
-- RetryDeadJob.
CREATE TABLE IF NOT EXISTS atlantis.jobs_dead (
    id              BIGINT PRIMARY KEY,
    job_name        TEXT NOT NULL,
    queue           TEXT NOT NULL,
    args            JSONB NOT NULL,
    attempts        INT NOT NULL,
    max_retries     INT NOT NULL,
    last_error      TEXT,
    last_error_at   TIMESTAMPTZ,
    moved_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    enqueued_at     TIMESTAMPTZ NOT NULL,
    submitted_by    TEXT
);

CREATE INDEX IF NOT EXISTS jobs_dead_moved_at_idx
    ON atlantis.jobs_dead (moved_at DESC);


-- Cron-driven jobs. One row per (job_name) — a job can have at most
-- one schedule; the DSL `schedule "..."` modifier emits this row at
-- apply time. The scheduler component evaluates cron_spec on a
-- ticker and INSERTs into atlantis.jobs when last_fired_at +
-- (next-fire-from-spec) <= now().
CREATE TABLE IF NOT EXISTS atlantis.job_schedules (
    id              BIGSERIAL PRIMARY KEY,
    job_name        TEXT NOT NULL UNIQUE,
    cron_spec       TEXT NOT NULL,
    -- Operator-flippable kill switch. Lets ops disable a misbehaving
    -- scheduled job without redeploying the schema.
    enabled         BOOLEAN NOT NULL DEFAULT true,
    last_fired_at   TIMESTAMPTZ,
    -- Default args for cron-fired invocations. The scheduler
    -- substitutes per-fire values (current timestamp, run id) into
    -- this template at INSERT time. NULL or '{}' means "fire with
    -- empty args" — most scheduled jobs take no parameters.
    default_args    JSONB NOT NULL DEFAULT '{}'::jsonb
);


-- LISTEN/NOTIFY broadcaster. Notifies on the queue name so worker
-- pools per queue can subscribe to their own channel suffix (e.g.
-- LISTEN atl_jobs; consume payloads matching this pool's queue).
-- The payload is the queue name; the worker re-queries the table
-- after waking because the row id is irrelevant to claim order.
CREATE OR REPLACE FUNCTION atlantis.notify_job_enqueue()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('atl_jobs', NEW.queue);
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS jobs_notify ON atlantis.jobs;
CREATE TRIGGER jobs_notify
    AFTER INSERT ON atlantis.jobs
    FOR EACH ROW EXECUTE FUNCTION atlantis.notify_job_enqueue();
