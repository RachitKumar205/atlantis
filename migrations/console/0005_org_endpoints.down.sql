-- Reverse of 0005. Removes each organisation's address and credentials.
--
-- This destroys the encrypted private keys along with the columns, and they are
-- not recoverable from anywhere else — the console was the only holder. After
-- running this, every organisation has to be registered again with freshly
-- issued certificates.
--
-- It exists so a failed migration can be unwound to a known shape, which is
-- what golang-migrate needs. It is not a rollback plan for a console that has
-- organisations on it.

ALTER TABLE console.orgs DROP COLUMN IF EXISTS updated_at;
ALTER TABLE console.orgs DROP COLUMN IF EXISTS client_key_ct;
ALTER TABLE console.orgs DROP COLUMN IF EXISTS client_cert_pem;
ALTER TABLE console.orgs DROP COLUMN IF EXISTS ca_pem;
ALTER TABLE console.orgs DROP COLUMN IF EXISTS atl_health_addr;
ALTER TABLE console.orgs DROP COLUMN IF EXISTS atl_endpoint;
