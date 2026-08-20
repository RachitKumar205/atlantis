-- Stop pinning a caller to one certificate.
--
-- ── What this removes and what replaces it ──────────────────────────────────
--
-- cert_fingerprint bound a caller to exactly one leaf, and the cert-binding
-- interceptor refused anything else. Migration 0031 then added
-- prev_cert_fingerprint and prev_valid_until so a renewal whose response was
-- lost did not lock the machine out. All three go.
--
-- What replaces them is a short certificate. Leaf lifetime drops from 90 days
-- to 7 (cmd/signer), and the trust decision becomes the one every comparable
-- system makes: verify the chain and the common name, and let a certificate
-- that should not exist stop working when it expires.
--
-- ── Why pinning existed, and why that reason is gone ────────────────────────
--
-- It bought revocation without a CRL. At 90 days that was worth a great deal:
-- "this leaked certificate stops working now" versus "in up to three months" is
-- not a real choice.
--
-- At 7 days it is. smallstep's own guidance for step-ca puts service
-- certificates at "one month or less" and defaults to passive revocation for
-- exactly this reason; SPIRE issues SVIDs with a one-hour default and pins
-- nothing. The industry position is that short lifetimes and active revocation
-- are alternatives, and the short-lifetime one is easier to operate correctly.
--
-- ── Deliberate revocation is unaffected, which is the part that matters ─────
--
-- The cert-binding interceptor still refuses any caller with no
-- caller_identities row, on a five-second cache, and RevokeCaller deletes that
-- row. So "cut this caller off now" remains immediate and is not what pinning
-- was providing. What pinning added on top was "this PARTICULAR certificate is
-- superseded", and that is what a seven-day life now covers.
--
-- ── The complexity it was generating ───────────────────────────────────────
--
-- Pinning is what produced every hard edge around enrolment: a caller could be
-- locked out by a renewal it never received; the overlap window existed to make
-- that survivable; two machines sharing one caller name could supersede each
-- other into a permanent, unattended lockout roughly a day later; and enrolling
-- a caller was a one-way door that needed a warning in the console. None of
-- those survive this migration.
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
