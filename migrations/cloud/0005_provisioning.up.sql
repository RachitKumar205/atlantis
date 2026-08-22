-- What decides an organisation should have an atlantis, and what remembers it.
--
-- Until now nothing did. `cloud org create` wrote a row saying an organisation
-- exists; `cloud org register` — seven flags and three file paths later — said
-- which atlantis serves it. Between those two commands sat a state the product
-- has no way to represent and every user meets: an organisation that exists and
-- cannot be entered, which /authorize answers with a 503 naming a CLI command.
--
-- The only provisioning state expressible today is `console_url = ''`. That was
-- the right shape when a human was about to type the next command anyway, and
-- 0004 chose it deliberately so "not configured yet" was one value rather than
-- two. It cannot distinguish queued from in-flight from failed-after-four-tries,
-- which is exactly what a machine doing the work needs to know.

-- ── The queue ───────────────────────────────────────────────────────────────
--
-- Modelled on atlantis.jobs, which is the lease queue this codebase already
-- runs, and deliberately not on a simpler design. Two properties are worth the
-- copy:
--
--   * The claim is one statement — a CTE with FOR UPDATE SKIP LOCKED feeding an
--     UPDATE — so two provisioners cannot take the same row. A select followed
--     by an update has a window between them; this has none.
--
--   * A lease rather than a flag. claimed_until expiring is what makes a row
--     claimable again, so a provisioner that dies mid-work frees it by doing
--     nothing, and there is no sweeper to write or to forget to run.
CREATE TABLE IF NOT EXISTS cloud.org_provisioning (
    org   TEXT PRIMARY KEY REFERENCES cloud.orgs(name) ON DELETE CASCADE,

    state TEXT NOT NULL DEFAULT 'pending'
          CHECK (state IN ('pending', 'provisioning', 'ready', 'failed')),

    -- Incremented when the row is claimed, not when work fails, so a process
    -- that dies mid-provision still burns an attempt. Otherwise a crash loop
    -- retries for ever at full speed and every row looks untouched.
    attempts   INT  NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',

    -- Who holds the lease and until when. claimed_by is for a human reading the
    -- table during an incident; only claimed_until is load-bearing.
    claimed_by    TEXT,
    claimed_until TIMESTAMPTZ,

    -- Backoff, as an explicit column rather than arithmetic in the claim.
    --
    -- atlantis.jobs is the model for everything else here and is the wrong
    -- model for this one thing: it has no backoff at all. Its last_error_at is
    -- written and never read, and the gating was left to "a later iteration"
    -- that never happened, so a failed job is eligible again on the next tick.
    --
    -- atlantis.parked_objects got there first and its migration says why:
    -- without this, failing rows at the head of the queue stop all work for ever
    -- while every run reports success.
    next_attempt_after TIMESTAMPTZ,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Written on every state change, not merely defaulted. console.orgs uses a
    -- column of this name as its freshness signal and the per-org client cache
    -- keys off it, so one that only ever recorded row creation would be a trap
    -- for whoever reads it next.
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Shaped to match the claim predicate, as jobs_pending_idx is.
--
-- 'provisioning' belongs in here. The claim has to consider claimed rows in
-- order to reclaim an expired lease, and a partial index that omits that state
-- drops exactly the reclaim branch out of the index.
CREATE INDEX IF NOT EXISTS org_provisioning_claimable_idx
    ON cloud.org_provisioning (created_at)
    WHERE state IN ('pending', 'failed', 'provisioning');

-- ── The record ──────────────────────────────────────────────────────────────
--
-- Cloud has never had an audit log. The console has one and Cloud's user id was
-- designed to be its actor — cloud.users' comment says the id becomes the `sub`
-- claim "and therefore the audit actor" — but nothing on this side ever wrote a
-- row of its own. `cloud org register`, `cloud user create` and `cloud member
-- add` leave no trace beyond the row they wrote.
--
-- Provisioning is where that stops being tolerable: it creates a namespace,
-- mints a certificate authority and writes an organisation's credentials.
-- "When was acme's CA minted, and by which provisioner" should be answerable
-- from the database rather than from whatever log retention happens to exist.
--
-- Deliberately NOT partitioned by month, unlike console.audit_log.
--
-- That table is partitioned because it records every operator action in the
-- product. This one records a handful of rows per organisation for its
-- lifetime, and partitioning is not free: it needs partitions created at boot
-- because a static migration cannot create next March's, plus a worker to drop
-- old ones — a worker which, in the console, had never successfully dropped a
-- partition until somebody looked. A plain table with a retention delete is the
-- right size for this volume.
CREATE TABLE IF NOT EXISTS cloud.audit_log (
    id BIGSERIAL PRIMARY KEY,

    -- The organisation the action concerns. Not a foreign key: an audit row
    -- outliving the thing it describes is the normal case, and a cascade here
    -- would delete the record of a deletion.
    org TEXT NOT NULL,

    -- Who acted, and their email as it was at the time.
    --
    -- Written onto the row rather than resolved when the log is read, for the
    -- reason the console states: an entry should say who acted when it
    -- happened, and a lookup reports whoever holds that identity now.
    --
    -- A machine action names itself — the provisioner writes a constant, not an
    -- empty string, because a blank actor reads as a bug in the logging rather
    -- than as a machine acting on its own behalf.
    actor       TEXT NOT NULL,
    actor_email TEXT NOT NULL DEFAULT '',

    action TEXT NOT NULL,
    detail JSONB,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS audit_log_org_created_idx
    ON cloud.audit_log (org, created_at DESC);
