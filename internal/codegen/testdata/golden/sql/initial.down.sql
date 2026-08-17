-- atlantis initial migration (down)
DROP TABLE IF EXISTS "atlantis"."library_book";
DROP TABLE IF EXISTS "atlantis"."library_author";
DROP SCHEMA IF EXISTS atlantis CASCADE;
