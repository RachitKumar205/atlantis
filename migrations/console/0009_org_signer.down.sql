-- Reverse of 0009. Every organisation falls back to the process-wide signer.
--
-- That is the behaviour before this migration, so nothing breaks in a
-- deployment that still has ATL_SIGNER_* set. What it does destroy is each
-- organisation's own signer credentials, including a sealed private key nothing
-- else holds — so an organisation that had one must be provisioned again to get
-- it back, and until then its callers are issued from whichever authority the
-- shared signer holds.

ALTER TABLE console.orgs DROP COLUMN IF EXISTS signer_client_key_ct;
ALTER TABLE console.orgs DROP COLUMN IF EXISTS signer_client_cert_pem;
ALTER TABLE console.orgs DROP COLUMN IF EXISTS signer_ca_pem;
ALTER TABLE console.orgs DROP COLUMN IF EXISTS signer_addr;
