-- An overlap window, so a renewal whose response is lost does not lock a
-- machine out.
--
-- With one fingerprint per caller, recording the new one stops the certificate
-- the machine still holds from authenticating. Any step after that write can
-- lose the response — a timeout, a proxy 502, the process dying before the file
-- reaches disk — leaving the machine holding a superseded certificate and
-- needing a valid one to renew. `tide login` renews unattended across a fleet,
-- so a rare delivery failure becomes a recurring lockout with no self-service
-- recovery.
--
-- The window is bounded by time, not by first use of the new certificate.
-- First use is observable only in the cert-binding interceptor, on every
-- authenticated RPC, so retiring there is a write on the hottest read path,
-- behind a five-second cache that would let it fire repeatedly.
--
-- prev_valid_until costs one comparison and no write. The exposure it buys is
-- explicit and short: a superseded certificate keeps working until this
-- timestamp and not one second longer, whether or not anything replaced it.
--
-- If use-based retirement is ever wanted, it belongs where the console can see
-- it rather than where atlantis does.
ALTER TABLE atlantis.caller_identities
    ADD COLUMN IF NOT EXISTS prev_cert_fingerprint BYTEA;

ALTER TABLE atlantis.caller_identities
    ADD COLUMN IF NOT EXISTS prev_valid_until TIMESTAMPTZ;

-- Both or neither. A previous fingerprint with no deadline would be a second
-- certificate that authenticates forever, which is the opposite of what this
-- migration is for; a deadline with no fingerprint is a value nothing reads.
--
-- Written as a CHECK rather than left to the code, because the code that
-- maintains these is one UPDATE in one function today and will not always be.
ALTER TABLE atlantis.caller_identities
    DROP CONSTRAINT IF EXISTS caller_identities_prev_pair;
ALTER TABLE atlantis.caller_identities
    ADD CONSTRAINT caller_identities_prev_pair CHECK (
        (prev_cert_fingerprint IS NULL AND prev_valid_until IS NULL) OR
        (prev_cert_fingerprint IS NOT NULL AND prev_valid_until IS NOT NULL)
    );

-- Existing rows get NULL for both, which means "no overlap in progress" — the
-- correct state for every caller that has not renewed since this landed.
