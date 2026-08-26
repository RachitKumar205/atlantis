-- Stop pinning a caller to one certificate.
--
-- cert_fingerprint bound a caller to exactly one leaf and the cert-binding
-- interceptor refused anything else; migration 0031 added
-- prev_cert_fingerprint and prev_valid_until so a renewal whose response was
-- lost did not lock the machine out. All three are dropped.
--
-- Leaf lifetime drops from 90 days to 7 (cmd/signer), and the check becomes
-- chain plus common name. Pinning bought revocation without a CRL, which is
-- worth much more at 90 days than at 7: smallstep's step-ca guidance puts
-- service certificates at "one month or less" and defaults to passive
-- revocation, and SPIRE issues SVIDs with a one-hour default and pins nothing.
--
-- Explicit revocation is unaffected. The cert-binding interceptor refuses any
-- caller with no caller_identities row, on a five-second cache, and
-- RevokeCaller deletes that row. Pinning added only "this particular
-- certificate is superseded", which a seven-day life now covers.
--
-- Removing it also removes: lockout by a renewal that was never received, the
-- overlap window that made that survivable, two machines sharing one caller
-- name superseding each other into an unattended lockout about a day later,
-- and enrolment being a one-way door that needed a warning in the console.
ALTER TABLE atlantis.caller_identities
    DROP CONSTRAINT IF EXISTS caller_identities_prev_pair;

ALTER TABLE atlantis.caller_identities
    DROP COLUMN IF EXISTS prev_valid_until;

ALTER TABLE atlantis.caller_identities
    DROP COLUMN IF EXISTS prev_cert_fingerprint;

-- cert_expires_at stays. It is read by GetCallers and shown in the console, and
-- "when does this caller's certificate run out" is a question worth answering
-- whether or not anything enforces a particular certificate.
ALTER TABLE atlantis.caller_identities
    DROP COLUMN IF EXISTS cert_fingerprint;
