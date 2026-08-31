-- Drop the file header from stored declarations.
--
-- The header used to open every entity. A namespace is written as one file, so
-- a database of 84 tables produced 84 copies of an eight-line comment. It is
-- emitted once per file at read time now, and a stored declaration starts at
-- its `entity` line.
--
-- The header holds no `entity ` substring, so the first occurrence is the start
-- of the declaration.
--
-- The table carries FORCE ROW LEVEL SECURITY and a RESTRICTIVE policy on
-- console.current_org(). A migration binds no organisation, so the policy
-- admits no rows and the UPDATE reports success having rewritten none. FORCE
-- is lifted for the statement and restored after it; the migration runs in one
-- transaction, so no session sees the table without it.

ALTER TABLE console.schema_import_entities NO FORCE ROW LEVEL SECURITY;

UPDATE console.schema_import_entities
   SET atl = substring(atl FROM position('entity ' IN atl))
 WHERE atl LIKE '// Generated from the live database.%'
   AND position('entity ' IN atl) > 0;

ALTER TABLE console.schema_import_entities FORCE ROW LEVEL SECURITY;
