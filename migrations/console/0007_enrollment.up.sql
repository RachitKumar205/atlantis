-- Enrolment: how a machine gets a client certificate without the console ever
-- holding its private key.
--
-- The console used to generate the keypair, build the CSR, and hand the private
-- key to a browser for download. The CSR half was always right; the key was
-- simply born in the wrong process. These two tables are what let it be born on
-- the machine that will use it.

-- ── console.enroll_tokens ───────────────────────────────────────────────────
--
-- A single-use, short-lived credential an admin mints and hands to a machine.
-- The machine generates a key, builds a CSR, and presents both to the enrolment
-- endpoint, which is otherwise unauthenticated — this row is the whole of what
-- authorises it.
--
-- ── The primary key is a hash, not the token ────────────────────────────────
--
-- console.spent_assertions stores its `jti` in the clear and that is fine: a
-- jti identifies an assertion, it does not authorise anything. This does. A row
-- holding the token verbatim is a bearer credential at rest, so a backup, a
-- logged query or a pg_dump would hand over every unredeemed enrolment. The
-- endpoint hashes what it is given and looks that up; nothing anywhere can read
-- a token back out.
--
-- ── used_at, and why expiry is not enforced here ────────────────────────────
--
-- Both halves of "still redeemable" — unused, and unexpired — are predicates on
-- the UPDATE that spends the row, so the check and the spend cannot be
-- separated by a race or by a later refactor. Reaping expired rows is
-- housekeeping that runs on a timer; it is deliberately NOT what makes an
-- expired token stop working. A sweeper on a 24-hour tick would leave an
-- hour-old fifteen-minute token perfectly redeemable, and nothing about that
-- failure is visible.
CREATE TABLE IF NOT EXISTS console.enroll_tokens (
    token_sha256 BYTEA       PRIMARY KEY,
    org          TEXT        NOT NULL,
    caller       TEXT        NOT NULL,
    -- The Cloud subject that minted it. An enrolment is a credential grant, so
    -- it needs an actor for the same reason registering a caller does.
    created_by   TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ NOT NULL,
    used_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS console_enroll_tokens_expires_idx
    ON console.enroll_tokens (expires_at);

-- Organisation isolation, the same shape as console.audit_log in 0004: a
-- RESTRICTIVE boundary that only subtracts, plus the permissive grant that
-- admits anything at all.
--
-- This is load-bearing rather than decorative, and it is worth being exact
-- about why, because the enrolment endpoint has no session. The request names
-- the organisation it believes it is enrolling into, the handler binds THAT
-- value, and this policy is what compares it against the row. A token minted
-- for acme, redeemed by a request naming globex, matches zero rows and is
-- refused as an unknown token — which is also the right thing to tell whoever
-- sent it.
--
-- The failure mode to know about: a handler that runs this on the bare pool
-- instead of a bound transaction leaves console.current_org() NULL, the
-- restrictive policy admits nothing, and EVERY valid token is refused. That
-- reads as "enrolment is broken" rather than as a scoping mistake.
ALTER TABLE console.enroll_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE console.enroll_tokens FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS enroll_tokens_org_isolation ON console.enroll_tokens;
CREATE POLICY enroll_tokens_org_isolation ON console.enroll_tokens
    AS RESTRICTIVE
    USING (org = console.current_org())
    WITH CHECK (org = console.current_org());

DROP POLICY IF EXISTS enroll_tokens_default_access ON console.enroll_tokens;
CREATE POLICY enroll_tokens_default_access ON console.enroll_tokens
    AS PERMISSIVE USING (true) WITH CHECK (true);

-- ── console.caller_certs ────────────────────────────────────────────────────
--
-- Every certificate this console has issued, keyed by the SHA-256 of the leaf.
--
-- ── Why the console needs its own record at all ─────────────────────────────
--
-- atlantis already stores a fingerprint per caller (atlantis.caller_identities),
-- but that is one row per caller inside one organisation's database, and it
-- answers "is this the certificate I expect for this caller". Renewal needs the
-- opposite lookup: given a certificate somebody just presented, which
-- organisation and which caller is it? Nothing in the certificate answers that.
-- signCSR copies the CSR's subject and adds no extension, and there is one CA,
-- so a leaf carries a CN and nothing else — and caller names are `backend`,
-- `api`, `worker`, which collide across organisations as a matter of course.
--
-- Without this table the renew route would have to take the organisation from
-- the request and would have nothing to check it against, so a caller in one
-- organisation could renew into another's atlantis and supersede the identity
-- that was working there. With it, the organisation and the caller both come
-- off a row this console wrote, and the request supplies neither.
--
-- ── No row-level security, deliberately ─────────────────────────────────────
--
-- Same exemption as console.sessions, and for the same reason 0004 gives: a
-- table that a request uses to DISCOVER its organisation cannot be filtered by
-- that organisation. The renewal lookup happens before anything knows which
-- organisation is involved — that is the entire point of it.
--
-- What keeps it safe is that the key is unguessable and globally unique (a
-- SHA-256 over a certificate nobody else holds), and that every handler which
-- LISTS these rows scopes them in Go by the session's organisation. A lookup by
-- fingerprint is not a search; you can only find the row for a certificate you
-- already have.
CREATE TABLE IF NOT EXISTS console.caller_certs (
    fingerprint   BYTEA       PRIMARY KEY,
    org           TEXT        NOT NULL,
    caller        TEXT        NOT NULL,
    issued_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at    TIMESTAMPTZ NOT NULL,
    -- Set when a later certificate for the same caller takes over. Kept rather
    -- than deleted so "this certificate used to be valid and was replaced" is
    -- distinguishable from "this certificate was never issued here", which are
    -- very different things to tell somebody whose deploy just started failing.
    superseded_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS console_caller_certs_org_caller_idx
    ON console.caller_certs (org, caller);
