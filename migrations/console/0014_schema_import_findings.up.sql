-- Keep what an import found, beside the declarations it generated.
--
-- 0010 stored the entities alone. The suggestions, the tables that were found
-- and not declared, and the facts introspection could not verify were computed
-- and returned once, so a review reached by URL showed a schema with nothing
-- said about it.
--
-- Documents rather than rows: each is read whole with the import it belongs
-- to, none is addressed on its own, and no count of them is taken in SQL.
-- console.schema_import_entities is a table because a declaration is keyed by
-- (import_id, table_name) and the import carries a count of them.

ALTER TABLE console.schema_imports
    ADD COLUMN IF NOT EXISTS suggestions JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS skipped     JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS warnings    JSONB NOT NULL DEFAULT '[]'::jsonb;

-- An array, so a read can index it without checking the shape first. A row
-- written before this migration keeps the default and reads as "nothing found",
-- which is what is known about it.
ALTER TABLE console.schema_imports
    DROP CONSTRAINT IF EXISTS schema_imports_findings_are_arrays;
ALTER TABLE console.schema_imports
    ADD CONSTRAINT schema_imports_findings_are_arrays CHECK (
        jsonb_typeof(suggestions) = 'array'
        AND jsonb_typeof(skipped) = 'array'
        AND jsonb_typeof(warnings) = 'array'
    );
