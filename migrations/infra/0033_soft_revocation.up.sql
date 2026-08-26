-- Revocation stops destroying the caller it revokes.
--
-- Deleting the caller_identities row destroys every grant the caller holds,
-- because caller_capabilities references it ON DELETE CASCADE (0018). That
-- includes CAPABILITY_OPERATOR and CAPABILITY_LOGS_READ, which sit outside
-- ManagedCapabilities so re-registering cannot strip a hand-granted capability.
-- The loss surfaces later as PermissionDenied on one page.
--
-- The console's grants are spread across migrations (0019 and any later one
-- adding a console feature; TestConsoleBootstrapGrantsEveryRPCTheConsoleCalls
-- reads the union rather than one file), so rebuilding them in application code
-- would be a second copy of a growing list. RegisterCaller refuses reserved
-- CNs, so a deleted console identity has no supported way back.
--
-- revoked_at marks the row instead of removing it. Restoring is clearing it,
-- and every grant stays where it was.
ALTER TABLE atlantis.caller_identities
    ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ;

COMMENT ON COLUMN atlantis.caller_identities.revoked_at IS
    'When set, this caller cannot authenticate. Its capabilities are retained so '
    'that restoring it is clearing this column. See the active_callers views.';

-- Partial index over the revoked rows only.
--
-- It is the EXCEPT in active_callers below that this serves — "list every
-- revoked caller" — which the auth allowlist evaluates in full on every reload,
-- thirty seconds apart, for the life of the process. Revocations are rare, so
-- indexing only them is a small index answering the whole of that subquery
-- instead of a sequential scan over every caller.
--
-- It does NOT serve the per-caller lookups. Those arrive as `caller = $1` and
-- are answered by the primary key; the revoked_at test is then one row's worth
-- of filtering and wants no index at all.
CREATE INDEX IF NOT EXISTS caller_identities_revoked_idx
    ON atlantis.caller_identities (caller)
    WHERE revoked_at IS NOT NULL;

-- The filter is a view rather than a WHERE clause in Go. Three readers make an
-- authentication decision from these tables: the auth allowlist, the
-- cert-binding interceptor, and the signer's check before issuing a
-- certificate. Three hand-written `revoked_at IS NULL` clauses is three chances
-- to omit one, and a fourth reader added later inherits the omission by
-- default. With a view, a query that wants revoked callers has to say so.

-- Callers permitted to authenticate at all: anyone who has registered or been
-- registered, minus anyone revoked.
--
-- The EXCEPT is the reason a revoked caller cannot slip back in through
-- caller_registrations. That table is written by `tide apply` rather than by an
-- operator, so without this a revoked caller would be readmitted by its own
-- next deploy — revocation undone by the thing being revoked.
CREATE OR REPLACE VIEW atlantis.active_callers AS
    (
        SELECT caller FROM atlantis.caller_registrations
        UNION
        SELECT caller FROM atlantis.caller_identities
    )
    EXCEPT
    SELECT caller FROM atlantis.caller_identities WHERE revoked_at IS NOT NULL;

-- Operator-registered identities that are still live.
--
-- Deliberately narrower than active_callers: the cert-binding interceptor and
-- the signer both require an *identity* row, not merely a registration, and
-- pointing either at active_callers would loosen a check rather than filter it.
CREATE OR REPLACE VIEW atlantis.active_caller_identities AS
    SELECT caller FROM atlantis.caller_identities WHERE revoked_at IS NULL;
