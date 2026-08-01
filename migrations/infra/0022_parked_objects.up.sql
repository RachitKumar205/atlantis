-- The register of objects parked by a destructive migration.
--
-- Destructive changes no longer drop; they rename a column out of the way or
-- move a table into atlantis_tombstone, leaving the rows intact. That makes the
-- change reversible, and it also means the objects accumulate forever unless
-- something eventually removes them. This table is what makes that possible: it
-- records what was parked, when, and when it may be reaped.
--
-- Why a table rather than inferring it from a naming convention. The reaper has
-- to know WHEN each object was parked, and Postgres does not record when a
-- column was renamed or a table moved. Without a recorded timestamp the only
-- options are dropping everything that matches the convention — which reaps a
-- parked table the instant it is created, defeating the point — or never
-- dropping anything.
--
-- The rows are written by the generated migration itself, in the same
-- transaction as the rename. A park recorded separately, after the fact, is a
-- park that can be applied and never recorded: the object then survives every
-- retention window, invisible to the reaper and to anyone looking for it.

CREATE TABLE IF NOT EXISTS atlantis.parked_objects (
    id            bigserial PRIMARY KEY,

    -- 'table' or 'column'. They are reaped differently — DROP TABLE against the
    -- tombstone schema, ALTER TABLE ... DROP COLUMN against the live one — and
    -- conflating them would have the reaper issue the wrong statement.
    kind          text NOT NULL CHECK (kind IN ('table', 'column')),

    -- Where the parked object now lives. For a table this is the tombstone
    -- schema; for a column it is the live schema, because a column cannot be
    -- moved out of its table.
    schema_name   text NOT NULL,
    object_name   text NOT NULL,

    -- For a column, the table holding it. NULL for a parked table.
    parent_table  text,

    -- The schema the object came from, which for a table is NOT schema_name:
    -- schema_name is the tombstone it now sits in, original_schema is where a
    -- restore has to put it back. They differ for every entity that overrides
    -- its table with `table "consumer.accounts"`. Without this column the
    -- register cannot describe a restore, which is most of what it is for.
    original_schema text NOT NULL,

    -- What it was called before. This is what a restore renames back to, and
    -- what makes the row legible to somebody deciding whether to keep it.
    original_name text NOT NULL,

    parked_at     timestamptz NOT NULL DEFAULT now(),

    -- When the reaper may drop it. Stored as an absolute instant rather than
    -- computed from parked_at plus a retention setting, so that changing the
    -- retention configuration cannot retroactively shorten the window an
    -- already-parked object was promised.
    reap_after    timestamptz NOT NULL,

    -- Set when the reaper drops it. The row is kept as an audit record: "this
    -- table existed and was removed on this date" is the question asked after
    -- the fact, and a deleted row cannot answer it.
    reaped_at     timestamptz,

    -- Reap attempts that failed, why, and when to try again.
    --
    -- Without these the reaper starves: it selects the oldest due rows, and a
    -- drop that cannot succeed — the usual cause is a dependency created after
    -- the park — leaves reaped_at NULL, so the same rows are selected again
    -- every run. A batch of stuck registrations at the head of the queue stops
    -- ALL reaping, indefinitely, while every run reports success.
    --
    -- Backing off moves them out of the way. last_error is kept because the
    -- alternative record of a failed reap is one WARN line an hour in a log
    -- nobody is reading, and an operator asking "why is this still here" has
    -- nowhere else to look.
    attempts           integer NOT NULL DEFAULT 0,
    last_error         text,
    next_attempt_after timestamptz
);

-- The reaper's query: unreaped rows whose window has passed. Partial, because
-- reaped rows are retained indefinitely as audit and would otherwise dominate
-- the index.
CREATE INDEX IF NOT EXISTS parked_objects_due_idx
    ON atlantis.parked_objects (reap_after)
    WHERE reaped_at IS NULL;

-- One live registration per object. A second park of the same name while the
-- first is still pending would otherwise leave two rows, and reaping either
-- would drop an object the other still claims to be protecting.
CREATE UNIQUE INDEX IF NOT EXISTS parked_objects_unique_live_idx
    ON atlantis.parked_objects (schema_name, coalesce(parent_table, ''), object_name)
    WHERE reaped_at IS NULL;

-- The reaper's whole safety argument is "it only ever touches rows in this
-- table" (jobs/reaper.go). That makes write access to it load-bearing: a row
-- naming a LIVE table with reap_after in the past is a DROP TABLE, executed by
-- a background job on a schedule. atlantis executes caller-authored SQL in
-- session, so the default grants are not something to rely on here — the
-- neighbouring 0021 established that and this follows it.
REVOKE ALL ON atlantis.parked_objects FROM PUBLIC;
