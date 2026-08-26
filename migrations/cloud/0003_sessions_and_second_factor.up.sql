-- Signing in: a session, the half-finished login that precedes it, and the
-- second factor that turns one into the other.
--
-- 0001 said the boundary would arrive with the tables that can carry it. These
-- are those tables, so this migration also creates the discriminator 0001
-- deliberately left out.

-- The user discriminator.
--
-- Same mechanism as the console's organisation boundary (migrations/console/
-- 0004): a transaction-local GUC read through a STABLE function, with a
-- RESTRICTIVE policy comparing it. Read that migration's notes for why a GUC
-- rather than a table, why transaction-local, and why nullif() is load-bearing.
--
-- The reasoning transfers because the premise does: Cloud, like the console,
-- executes no caller-authored SQL. Every statement it issues is a literal in
-- internal/cloud with bound parameters, so the attack that made this shape
-- unsafe on the *server* — where a caller's own query body runs in the same
-- session as the reads a policy constrains — has no route here.
--
-- What differs is the discriminator. The console separates organisations; Cloud
-- separates users. One Cloud process authenticates everybody, so a query that
-- forgets `WHERE user_id = $1` hands one person another person's second factor.
CREATE OR REPLACE FUNCTION cloud.current_user_id()
RETURNS text LANGUAGE sql STABLE AS $$
    SELECT nullif(pg_catalog.current_setting('cloud.user', true), '')
$$;

CREATE OR REPLACE FUNCTION cloud.set_user(v text)
RETURNS void LANGUAGE sql AS $$
    SELECT pg_catalog.set_config('cloud.user', COALESCE(v, ''), true)
$$;

-- Sessions.
--
-- A signed-in browser. Reached by token, so like console.sessions it is the
-- table that *discovers* who a request is and cannot be filtered by who the
-- request is.
--
-- The token column holds the token itself rather than a hash, matching the
-- console. That is a deliberate difference from cloud.email_tokens, and the
-- distinction is where the value travels: a session token lives in a
-- Secure/HttpOnly cookie and is never in a URL, so it does not land in browser
-- history, referrers or access logs the way an emailed link does.
CREATE TABLE IF NOT EXISTS cloud.sessions (
    token      TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES cloud.users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS sessions_user ON cloud.sessions (user_id);
CREATE INDEX IF NOT EXISTS sessions_expiry ON cloud.sessions (expires_at);

-- Half-finished logins.
--
-- A password has been verified and a second factor has not. This is its own
-- table, and that is the single most important decision in this migration.
--
-- The obvious implementation is a `state` column on cloud.sessions. Under that
-- shape, every query that reads a session has to remember to check the column,
-- and the one that forgets authenticates somebody who has presented one factor.
-- That is not a hypothetical failure mode for this codebase; it is the shape of
-- most of the bugs in its CHANGELOG.
--
-- With two tables there is nothing to remember. A session lookup runs against
-- cloud.sessions, a pending login is not in cloud.sessions, so the lookup does
-- not find it. It fails closed by construction rather than by discipline.
--
-- `may_enrol` distinguishes an account that has a factor and must present it
-- from one that has none and must set one up. Both are half-finished logins;
-- they differ in which route the token opens, and neither opens a session.
CREATE TABLE IF NOT EXISTS cloud.pending_logins (
    token      TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES cloud.users(id) ON DELETE CASCADE,
    may_enrol  BOOLEAN NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS pending_logins_expiry ON cloud.pending_logins (expires_at);

-- The second factor.
--
-- The secret is ENCRYPTED, not hashed, and the difference is forced: verifying
-- a code means recomputing it, which needs the secret back. A password can be
-- hashed because it is only ever compared; this cannot.
--
-- So it is sealed with internal/secrets — the same keyring the console uses for
-- organisation private keys, for the same reason. Associated data is the user
-- id, which is what stops a ciphertext being lifted from one row onto another:
-- somebody who can UPDATE this table cannot move a known secret onto another
-- account, because it opens under a different user and the open fails.
--
-- Without that, a read of this table hands over every account's second factor
-- in the clear, at a point where the passwords beside it are argon2id and would
-- take real work.
--
-- last_used_step records the 30-second TOTP step a code was accepted in, so the
-- same code cannot be presented twice inside its own window. A TOTP code is
-- valid for tens of seconds, which is ample time to replay one observed over a
-- shoulder or in a phishing proxy.
CREATE TABLE IF NOT EXISTS cloud.totp_secrets (
    user_id        TEXT PRIMARY KEY REFERENCES cloud.users(id) ON DELETE CASCADE,
    secret_ct      BYTEA NOT NULL,
    confirmed_at   TIMESTAMPTZ,
    last_used_step BIGINT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Recovery, for a lost phone.
--
-- Hashed with argon2id rather than SHA-256, which is a departure from
-- cloud.email_tokens and deliberate. An email token is 256 bits from
-- crypto/rand and unguessable; a backup code has to be typed by a human, so it
-- is short — around 50 bits — which is inside reach of an offline attacker with
-- a fast hash and a GPU. The slow hash is what closes that, and it costs
-- nothing on an operation used approximately once.
CREATE TABLE IF NOT EXISTS cloud.backup_codes (
    id         BIGSERIAL PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES cloud.users(id) ON DELETE CASCADE,
    code_hash  TEXT NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS backup_codes_user ON cloud.backup_codes (user_id);

-- The boundary.
--
-- These two are the first Cloud tables that can carry it: both are read only
-- for a user who has already been identified, so there is always something to
-- bind. cloud.sessions and cloud.pending_logins are the queries that do the
-- identifying, so they stay exempt for the same structural reason as
-- cloud.users — see internal/cloud/store/policyguard.go, which refuses to start
-- if a table here is neither policed nor listed.
--
-- ENABLE plus FORCE. Without FORCE the owning role — the role Cloud connects
-- as — reads straight through, `\d` still lists the policy, and nothing
-- observable differs from a boundary that works. FORCE does not bind a
-- superuser either, which is why the guard also checks the role, and why this
-- migration is the point at which Cloud needs its own NOSUPERUSER role.
ALTER TABLE cloud.totp_secrets ENABLE ROW LEVEL SECURITY;
ALTER TABLE cloud.totp_secrets FORCE ROW LEVEL SECURITY;
ALTER TABLE cloud.backup_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE cloud.backup_codes FORCE ROW LEVEL SECURITY;

-- RESTRICTIVE for the boundary, permissive for the grant. Postgres admits a row
-- when ANY permissive policy allows AND EVERY restrictive one does, so a
-- boundary in the permissive slot is a grant that any later policy widens past.
-- The console learned this in its step 4; see migrations/console/0004.
CREATE POLICY totp_user_isolation ON cloud.totp_secrets
    AS RESTRICTIVE USING (user_id = cloud.current_user_id());
CREATE POLICY totp_default_access ON cloud.totp_secrets
    AS PERMISSIVE USING (true);

CREATE POLICY backup_codes_user_isolation ON cloud.backup_codes
    AS RESTRICTIVE USING (user_id = cloud.current_user_id());
CREATE POLICY backup_codes_default_access ON cloud.backup_codes
    AS PERMISSIVE USING (true);
