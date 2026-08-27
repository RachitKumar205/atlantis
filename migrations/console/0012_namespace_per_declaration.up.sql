-- Move the namespace from the import to the declaration.
--
-- 0010 recorded one namespace per import, because a pass named the schemas to
-- read and the namespace to put them in. A pass now reads every schema the
-- database has and gives each one a namespace of its own, so an import spans as
-- many namespaces as the database has schemas and the column no longer
-- describes it.
--
-- The pair (schema, namespace) is not stored: table_name is schema-qualified,
-- so the schema is already on the row.

ALTER TABLE console.schema_import_entities
    ADD COLUMN IF NOT EXISTS namespace TEXT NOT NULL DEFAULT '';

-- Rows written by 0010 carry the import's namespace. Backfilled before the
-- column goes, so an import stored yesterday still says what it was read into.
UPDATE console.schema_import_entities e
   SET namespace = i.namespace
  FROM console.schema_imports i
 WHERE i.id = e.import_id AND e.namespace = '';

ALTER TABLE console.schema_imports
    DROP COLUMN IF EXISTS namespace;
