-- Revocation stops destroying the caller it revokes.
--
-- ── What was wrong ─────────────────────────────────────────────────────────
--
-- RevokeCaller deleted the caller_identities row, and caller_capabilities
-- references that row ON DELETE CASCADE (0018). So revoking a caller destroyed
-- every grant it held — including the ones registration does not manage.
-- CAPABILITY_OPERATOR and CAPABILITY_LOGS_READ sit outside ManagedCapabilities
-- deliberately, so that re-registering a caller cannot strip authority an
-- operator granted by hand. Revoking stripped it anyway, and re-registering
-- afterwards restored the managed bundle and quietly left the rest behind.
--
-- The loss is invisible in the way that matters: the caller authenticates
-- again, and then one page returns PermissionDenied to whoever opens it.
--
-- ── Why it is being fixed now ──────────────────────────────────────────────
--
-- 'atlantis-console' is no longer exempt from the cert-binding check, which
-- gives the console real revocation for the first time — it holds an admin
-- credential for every organisation, and until now nothing could cut one off.
--
-- That turned the cascade from a wart into a trap. The console's grants are
-- spread across migrations (0019, and any later one adding a console feature —
-- see TestConsoleBootstrapGrantsEveryRPCTheConsoleCalls, which reads the union
-- rather than one file). Rebuilding them in application code would be a second
-- copy of a list that grows, drifting from this one silently. And RegisterCaller
-- refuses reserved CNs, so a deleted console identity had no supported way back
-- at all: revocation would have been a one-way door.
--
-- Nothing needs rebuilding if nothing is destroyed.
--
-- ── The shape ──────────────────────────────────────────────────────────────
--
-- revoked_at marks the row instead of removing it. Restoring is clearing it,
-- and every grant is still exactly where it was.
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

-- ── Why the filter is a view and not a WHERE clause in Go ──────────────────
--
-- Three separate readers make an authentication decision from these tables: the
-- auth allowlist, the cert-binding interceptor, and the signer's check before
-- it issues a certificate. A revoked caller has to be refused by all three, and
-- the signer matters as much as the other two — issuing a fresh certificate to a
-- caller somebody just revoked is the one outcome that makes revocation look
-- like it worked when it did not.
--
-- Three hand-written `revoked_at IS NULL` clauses is three chances to forget,
-- and a fourth reader added later inherits the omission by default. Naming the
-- filtered set once means a query that wants to see revoked callers has to say
-- so, which is the direction the mistake should fall in.

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
