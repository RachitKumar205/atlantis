-- Restores the columns, empty.
--
-- Every caller comes back NULL, which the pre-0032 interceptor read as its
-- bootstrap window: accept any CA-signed certificate until the first issuance
-- records a fingerprint. So rolling back does not restore pinning's protection,
-- it restores the machinery — and the protection returns caller by caller as
-- each one next enrols.
--
-- Certificates issued while 0032 was applied carry a seven-day life. If the
-- signer's certTTL is not put back at the same time, they will be replaced by
-- 90-day ones at the next renewal without anything saying so.
ALTER TABLE atlantis.caller_identities
    ADD COLUMN IF NOT EXISTS cert_fingerprint BYTEA;

ALTER TABLE atlantis.caller_identities
    ADD COLUMN IF NOT EXISTS prev_cert_fingerprint BYTEA;

ALTER TABLE atlantis.caller_identities
    ADD COLUMN IF NOT EXISTS prev_valid_until TIMESTAMPTZ;

ALTER TABLE atlantis.caller_identities
    DROP CONSTRAINT IF EXISTS caller_identities_prev_pair;
ALTER TABLE atlantis.caller_identities
    ADD CONSTRAINT caller_identities_prev_pair CHECK (
        (prev_cert_fingerprint IS NULL AND prev_valid_until IS NULL) OR
        (prev_cert_fingerprint IS NOT NULL AND prev_valid_until IS NOT NULL)
    );
