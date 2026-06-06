-- caller_identities.cert_expires_at: NotAfter of the most recently
-- console-issued cert for this caller.
--
-- This is a UI hint only. Trust still flows through (a) the actual
-- mTLS handshake's leaf NotAfter, (b) the caller_identities allowlist,
-- and (c) revocation by removing the identity row. Persisting it here
-- lets the console render a cert-validity meter without re-asking the
-- signer on every page load.
--
-- Out-of-band issuances (e.g. `make self-host-caller-cert` or anything
-- that bypasses the console BFF) won't update this column — the meter
-- will then reflect "last issued from here," which is the honest
-- semantic. A future signer.ListLeaves RPC would replace this with
-- ground truth.
ALTER TABLE atlantis.caller_identities
    ADD COLUMN IF NOT EXISTS cert_expires_at TIMESTAMPTZ;
