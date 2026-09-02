-- One `tide login` in flight: a device code the CLI polls with, and a short
-- user code the person types into the approval page.
--
-- The device code is stored as its SHA-256, as enroll tokens are in the
-- console's 0007: a row holding it verbatim would be a bearer credential at
-- rest. The user code is stored plainly — it is display material with a
-- five-attempt budget, not a credential.
--
-- No assertion is ever stored here. Approval records a decision; the next
-- poll spends it (approved -> consumed, one UPDATE) and mints a fresh
-- two-minute assertion in the response. Minting at approval would both race
-- that TTL and park a live credential in this table.
--
-- caller and client_meta come from the unauthenticated start request. Cloud
-- cannot verify either; they exist so the approval page can show the person
-- exactly what is asking — a caller name, a hostname, an address — and the
-- console re-checks the caller against its own registry at enrolment.
CREATE TABLE IF NOT EXISTS cloud.cli_grants (
    code_sha256 BYTEA       PRIMARY KEY,
    user_code   TEXT        NOT NULL,
    caller      TEXT        NOT NULL,
    client_meta JSONB       NOT NULL DEFAULT '{}'::jsonb,
    user_id     TEXT,
    org         TEXT,
    status      TEXT        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'denied', 'expired', 'consumed')),
    attempts    INTEGER     NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at  TIMESTAMPTZ NOT NULL,
    approved_at TIMESTAMPTZ,
    consumed_at TIMESTAMPTZ
);

-- The approval page looks a grant up by what the person typed. Partial, so a
-- code can be reissued once its grant leaves pending and the alphabet stays
-- small without collisions accumulating.
CREATE UNIQUE INDEX IF NOT EXISTS cloud_cli_grants_user_code_idx
    ON cloud.cli_grants (user_code) WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS cloud_cli_grants_expires_idx
    ON cloud.cli_grants (expires_at);
