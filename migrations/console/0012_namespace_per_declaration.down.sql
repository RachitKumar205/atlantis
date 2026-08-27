-- One namespace per import again. An import spanning several schemas keeps the
-- first namespace its declarations were read into, because the column holds
-- one.

ALTER TABLE console.schema_imports
    ADD COLUMN IF NOT EXISTS namespace TEXT NOT NULL DEFAULT '';

UPDATE console.schema_imports i
   SET namespace = coalesce((
        SELECT e.namespace FROM console.schema_import_entities e
         WHERE e.import_id = i.id
         ORDER BY e.table_name
         LIMIT 1), '');

ALTER TABLE console.schema_import_entities
    DROP COLUMN IF EXISTS namespace;
