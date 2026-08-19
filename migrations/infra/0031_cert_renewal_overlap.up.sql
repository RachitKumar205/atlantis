-- An overlap window, so a renewal whose response is lost does not lock a
-- machine out.
--
-- ── The failure this exists to remove ───────────────────────────────────────
--
-- caller_identities held ONE fingerprint and the cert-binding interceptor
-- accepted exactly that one. So renewal was: mint the new certificate, record
-- its fingerprint, hand it back. The moment the fingerprint was recorded, the
-- certificate the machine was still holding stopped authenticating.
--
-- Every step after the write is a step where the response can be lost — a
-- timeout, a proxy answering 502, the process dying before the file reaches
-- disk, a full filesystem. The machine then holds a superseded certificate and
-- needs a valid one to renew, which is a hard lockout with an administrator as
-- the only way out.
--
-- Rare per machine, and `tide login` renews unattended across a fleet, forever.
-- A one-in-ten-thousand delivery failure becomes a recurring incident with no
-- self-service recovery.
--
-- ── Bounded by time, not by use ─────────────────────────────────────────────
--
-- The tighter design retires the previous fingerprint the moment the new one is
-- first used: the old certificate dies as soon as the machine proves it has the
-- replacement. It was not chosen, and the reason is where the "first use" is
-- observed — inside the cert-binding interceptor, on every authenticated RPC.
-- Retiring there means a write on the hottest read path in the product, behind
-- a five-second cache that would let it fire repeatedly before the new state is
-- visible.
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
