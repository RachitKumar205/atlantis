-- Declared-schedule columns on atlantis.job_schedules.
--
-- A row now carries everything a fire needs, so the scheduler enqueues
-- from the row alone: the queue the job's workers drain, the retry
-- budget, the per-attempt timeout, and the caller whose read scope the
-- fired row lands in.
--
-- managed_by says who owns the row: 'builtin' rows are seeded once at
-- boot and then hand-edited; 'dsl' rows mirror a declared `schedule`
-- clause and are rewritten from the checkpoint, except `enabled`, which
-- stays an operator decision on both.

ALTER TABLE atlantis.job_schedules
  ADD COLUMN IF NOT EXISTS queue        TEXT    NOT NULL DEFAULT 'atlantis',
  ADD COLUMN IF NOT EXISTS max_retries  INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS timeout_ms   INTEGER,
  ADD COLUMN IF NOT EXISTS owner_caller TEXT    NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS managed_by   TEXT    NOT NULL DEFAULT 'builtin';

-- timeout_ms NULL means no per-attempt deadline, matching
-- atlantis.jobs.timeout_ms. Existing rows are built-ins, which ran with
-- a 10-minute deadline before the column existed.
UPDATE atlantis.job_schedules SET timeout_ms = 600000 WHERE timeout_ms IS NULL;

-- Guarded: ADD CONSTRAINT has no IF NOT EXISTS, and the column adds
-- above survive a re-run after a partial failure, so this must too.
DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
     WHERE conname = 'job_schedules_managed_by_check'
       AND conrelid = 'atlantis.job_schedules'::regclass
  ) THEN
    ALTER TABLE atlantis.job_schedules
      ADD CONSTRAINT job_schedules_managed_by_check
      CHECK (managed_by IN ('builtin', 'dsl'));
  END IF;
END $$;
