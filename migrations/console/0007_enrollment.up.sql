-- Enrolment: how a machine gets a client certificate without the console ever
-- holding its private key.
--
-- The console used to generate the keypair, build the CSR, and hand the private
-- key to a browser for download. The CSR half was always right; the key was
-- simply born in the wrong process. These two tables are what let it be born on
-- the machine that will use it.

-- console.enroll_tokens.
--
-- A single-use, short-lived credential an admin mints and hands to a machine.
-- The machine generates a key, builds a CSR, and presents both to the enrolment
-- endpoint, which is otherwise unauthenticated — this row is the whole of what
-- authorises it.
--
-- The primary key is the SHA-256 of the token, not the token. A row holding it
-- verbatim would be a bearer credential at rest, readable from a backup, a
-- logged query or a pg_dump. Unlike console.spent_assertions, which stores jti
-- in the clear, this value authorises something.
--
-- Both halves of "still redeemable", unused and unexpired, are predicates on
-- the UPDATE that spends the row, so the check and the spend cannot be
-- separated. Reaping expired rows runs on a timer and is not what makes an
-- expired token stop working: a 24-hour sweep would leave an hour-old
-- fifteen-minute token redeemable.
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
-- The enrolment endpoint has no session: the request names the organisation it
-- believes it is enrolling into, the handler binds that value, and this policy
-- compares it against the row. A token minted for acme and redeemed by a
-- request naming globex matches zero rows and is refused as unknown.
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

-- console.caller_certs.
--
-- Every certificate this console has issued, keyed by the SHA-256 of the leaf.
--
-- atlantis.caller_identities answers "is this the certificate I expect for this
-- caller", one row per caller inside one organisation's database. Renewal needs
-- the reverse lookup: given a presented certificate, which organisation and
-- caller is it? The leaf cannot say — signCSR copies the subject and adds no
-- extension, and caller names such as `backend` collide across organisations.
--
-- Without this table the renew route would take the organisation from the
-- request with nothing to check it against, so a caller in one organisation
-- could renew into another's atlantis.
--
-- No row-level security, the same exemption console.sessions has in 0004: a
-- table used to discover the organisation cannot be filtered by it. The key is
-- a SHA-256 over a certificate the requester already holds, so a lookup is not
-- a search, and every handler that lists these rows scopes them in Go by the
-- session's organisation.
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
