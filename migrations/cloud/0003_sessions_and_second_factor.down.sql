-- Reverses 0003. Everyone is signed out, and every enrolled second factor is
-- destroyed — which means every account goes back to needing enrolment before
-- it can sign in again.
--
-- The discriminator functions go too. They exist only for the policies on the
-- tables above; leaving them behind would leave the policy guard looking for
-- something no table references.
DROP TABLE IF EXISTS cloud.backup_codes;
DROP TABLE IF EXISTS cloud.totp_secrets;
DROP TABLE IF EXISTS cloud.pending_logins;
DROP TABLE IF EXISTS cloud.sessions;

DROP FUNCTION IF EXISTS cloud.set_user(text);
DROP FUNCTION IF EXISTS cloud.current_user_id();
