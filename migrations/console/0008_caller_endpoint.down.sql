-- Callers fall back to atl_endpoint, which is what they used before this
-- column existed. An organisation that had set a distinct public address loses
-- it, and machines enrolling afterwards are handed the console's own route —
-- which may not resolve for them.
ALTER TABLE console.orgs DROP COLUMN IF EXISTS atl_public_endpoint;
