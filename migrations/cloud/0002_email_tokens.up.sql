-- Proof that somebody controls an email address.
--
-- Two flows need it and they are the same mechanism pointed at different ends:
-- verifying an address at sign-up, and resetting the password on an address
-- already verified. One table with a `purpose` column rather than two tables,
-- because the spend logic — single use, short TTL, constant-time lookup — is
-- the part that has to be right and duplicating it is how one copy drifts.

-- ── The token ───────────────────────────────────────────────────────────────
--
-- What is stored is a HASH of the token, not the token.
--
-- The token travels in a URL, which means it lands in browser history, in the
-- referrer of whatever the landing page loads, and in any log along the way.
-- Storing it verbatim means a read of this table is a working password reset
-- for every account with a pending one — the same reasoning that puts a hash in
-- cloud.users rather than a password.
--
-- SHA-256 rather than argon2id, and the difference from the password case is
-- worth stating: this value is 256 bits from crypto/rand, so it is not
-- guessable and there is no dictionary to run against it. The slow hash next
-- door exists because human-chosen passwords are guessable; that reason does
-- not apply here, and a slow hash on a link click would be a denial-of-service
-- surface rather than a defence.
CREATE TABLE IF NOT EXISTS cloud.email_tokens (
    token_hash BYTEA PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES cloud.users(id) ON DELETE CASCADE,

    -- What presenting this token is allowed to do. A verification token must
    -- not be usable to set a password: they are issued under different
    -- conditions — one at sign-up to an unproven address, the other on request
    -- to a proven one — and a token that works for either is only as strong as
    -- the weaker path that issued it.
    purpose    TEXT NOT NULL CHECK (purpose IN ('verify_email', 'reset_password')),

    -- The address the token was sent to, recorded at issue time.
    --
    -- Not read back to route anything; it exists so that a reset issued to an
    -- old address cannot be spent after the account's address changes. The
    -- alternative is a token that outlives the mailbox that proved it.
    email      TEXT NOT NULL,

    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Spending is recorded rather than deleting the row, so that presenting a
    -- token twice can be told apart from presenting one that never existed.
    -- The sweeper removes them once expired; until then a replay is visible.
    used_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS email_tokens_user ON cloud.email_tokens (user_id, purpose);

-- For the expiry sweep, which is the only query that scans by time.
CREATE INDEX IF NOT EXISTS email_tokens_expiry ON cloud.email_tokens (expires_at);

-- Not policed, and this one is a genuine judgement rather than a structural
-- necessity like cloud.users.
--
-- The spend is a lookup BY TOKEN HASH with nobody signed in — that is the whole
-- point of a reset link — so a policy keyed to the current user would match
-- nothing and every reset would report an invalid token. Same shape as the
-- OAuth callback in 0001.
--
-- Recorded in internal/cloud/store/policyguard.go, which refuses to start if a
-- table here is neither policed nor listed.
