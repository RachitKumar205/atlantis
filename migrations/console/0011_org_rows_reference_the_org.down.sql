-- Rows may name an organisation the registry does not hold again, which makes
-- the retention sweeps incomplete rather than wrong: they still visit every
-- registered organisation.

ALTER TABLE console.schema_imports
    DROP CONSTRAINT IF EXISTS schema_imports_org_fkey;

ALTER TABLE console.enroll_tokens
    DROP CONSTRAINT IF EXISTS enroll_tokens_org_fkey;

DROP INDEX IF EXISTS console.console_enroll_tokens_org_idx;
