ALTER TABLE console.schema_imports
    DROP CONSTRAINT IF EXISTS schema_imports_findings_are_arrays;

ALTER TABLE console.schema_imports
    DROP COLUMN IF EXISTS suggestions,
    DROP COLUMN IF EXISTS skipped,
    DROP COLUMN IF EXISTS warnings;
