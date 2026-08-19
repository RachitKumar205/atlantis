-- Dropping the overlap makes every renewal a hard cutover again: the moment a
-- new fingerprint is recorded, the certificate the machine is still holding
-- stops authenticating, and a lost response locks it out until an operator
-- issues a fresh enrolment token.
--
-- A caller mid-renewal when this runs — new certificate recorded, previous one
-- still inside its window — loses the previous one immediately. That is only a
-- problem for a machine that has not yet stored the replacement, which is the
-- exact case the column exists for.
ALTER TABLE atlantis.caller_identities
    DROP CONSTRAINT IF EXISTS caller_identities_prev_pair;
ALTER TABLE atlantis.caller_identities
    DROP COLUMN IF EXISTS prev_valid_until;
ALTER TABLE atlantis.caller_identities
    DROP COLUMN IF EXISTS prev_cert_fingerprint;
