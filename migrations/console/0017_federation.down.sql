ALTER TABLE console.caller_certs
    DROP COLUMN IF EXISTS renewals_remaining;

DROP TABLE IF EXISTS console.federation_rules;
