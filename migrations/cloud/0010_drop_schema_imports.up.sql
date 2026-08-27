-- Schema import moves to the console.
--
-- 0009 put these tables here, keyed to the account. That was the wrong owner: a
-- generated declaration targets one organisation's atlantis, so an import
-- belongs beside that organisation's schema rather than beside the identity
-- that happened to run it. Cloud holds identity and the organisation lifecycle;
-- schema is the console's.
--
-- console.schema_imports (migrations/console/0010) replaces them, scoped by
-- organisation and policed by console.current_org().
--
-- Dropped by a new migration rather than by deleting 0009. A deployment that
-- applied 0009 records version 9, and removing the file would leave the binary
-- carrying migrations up to 8 while the database says 9 — which golang-migrate
-- refuses to reconcile, and the process does not start.

DROP TABLE IF EXISTS cloud.schema_import_entities;
DROP TABLE IF EXISTS cloud.schema_imports;
