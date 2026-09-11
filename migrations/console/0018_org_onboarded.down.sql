-- Dropping the column returns every organisation to being asked again on the
-- next sign-in, which is the state before 0018.
ALTER TABLE console.orgs DROP COLUMN IF EXISTS onboarded_at;
