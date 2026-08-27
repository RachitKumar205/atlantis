-- Reverse of 0009.
--
-- Drops every stored import. Nothing else reads these tables and no other row
-- references them, so this strands nothing — the schemas they describe live in
-- the customers' own databases, and an import is re-read by pointing atlantis
-- at the connection string again.
--
-- The entities table goes first. Its foreign key would otherwise refuse the
-- drop, and naming the order here is cheaper than relying on CASCADE.

DROP TABLE IF EXISTS cloud.schema_import_entities;
DROP TABLE IF EXISTS cloud.schema_imports;
