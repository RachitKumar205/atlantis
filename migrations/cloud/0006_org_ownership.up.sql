-- Who created an organisation, and how many one account may create.
--
-- Both exist because organisations are about to become something a stranger
-- makes in a browser rather than something an operator types at a terminal.
-- Until now the only way to get one was `cloud org create`, run by somebody who
-- already had a shell on the machine, and neither question came up.

-- ── Who created it ──────────────────────────────────────────────────────────
--
-- cloud.memberships records who may act in an organisation. It has never
-- recorded who *made* one — `CreateOrgWithOwner`'s "owner" is nothing more than
-- the first admin grant, indistinguishable afterwards from an admin somebody
-- added later.
--
-- That gap has two consequences, and the second is the expensive one:
--
--   * "the owner may delete this organisation" has no subject. Any admin is
--     every other admin.
--   * a limit on organisations per account has nothing to count. Counting admin
--     memberships would mean somebody adding you to *their* organisation
--     consumes *your* quota — and it would do so through AddMember, which the
--     create path's lock cannot see.
--
-- Nullable, and it has to be: `acme` and `walkthru2` predate this column and
-- nothing knows who made them. Backfilling a guess would be worse than the
-- honest absence, because a wrong owner is a wrong answer to "may this person
-- delete it".
--
-- ON DELETE SET NULL rather than CASCADE. Deleting an account must not delete
-- the organisations it created — other people work in them, and their data is
-- not the departing account's to take. The organisation becomes unowned, which
-- is a state somebody has to resolve rather than one that resolves itself.
ALTER TABLE cloud.orgs
    ADD COLUMN IF NOT EXISTS created_by TEXT
        REFERENCES cloud.users(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS orgs_created_by ON cloud.orgs (created_by)
    WHERE created_by IS NOT NULL;

-- ── How many one account may create ─────────────────────────────────────────
--
-- A per-account column rather than one global setting, so a single account can
-- be raised without a migration or a deploy. There is no admin surface to
-- change it yet; it is an UPDATE, and saying so here is better than implying a
-- screen exists.
--
-- Three is a starting point, not a considered capacity. What makes a limit
-- necessary at all is that each organisation costs a namespace, a Postgres
-- cluster, roughly 400 MiB and three NodePorts — and the NodePort range caps
-- the whole cluster somewhere near 900 organisations regardless of who owns
-- them. An account with no limit is a way for one person to exhaust that.
--
-- This is not billing and should not be mistaken for it. When real quotas
-- arrive they replace this column; until then it is the difference between a
-- bounded cluster and an unbounded one.
ALTER TABLE cloud.users
    ADD COLUMN IF NOT EXISTS org_limit INT NOT NULL DEFAULT 3
        CHECK (org_limit >= 0);
