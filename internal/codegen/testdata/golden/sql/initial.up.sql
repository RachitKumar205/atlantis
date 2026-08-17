-- atlantis initial migration
CREATE SCHEMA IF NOT EXISTS atlantis;

CREATE TABLE IF NOT EXISTS "atlantis"."library_author" (
  "id" BIGINT,
  "name" TEXT NOT NULL,
  "bio" TEXT,
  "rating" DOUBLE PRECISION,
  "tenure" INTERVAL,
  "created_at" TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT "library_author_pkey" PRIMARY KEY ("id")
);

CREATE TABLE IF NOT EXISTS "atlantis"."library_book" (
  "id" BIGINT,
  "tenant" VARCHAR(32) NOT NULL,
  "title" TEXT NOT NULL,
  "author_id" BIGINT NOT NULL,
  "score" REAL,
  "page_count" INTEGER,
  "summary" TEXT,
  "expires_at" TIMESTAMPTZ,
  "created_at" TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT "library_book_pkey" PRIMARY KEY ("id"),
  CONSTRAINT "library_book_author_id_fkey" FOREIGN KEY ("author_id") REFERENCES "atlantis"."library_author" ("id")
);
ALTER TABLE "atlantis"."library_book" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "atlantis"."library_book" FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS "library_book_tenant_isolation" ON "atlantis"."library_book";
CREATE POLICY "library_book_tenant_isolation" ON "atlantis"."library_book" AS RESTRICTIVE USING ("tenant" = atlantis.current_partition()) WITH CHECK ("tenant" = atlantis.current_partition());
DO $atlantis_default_access$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polrelid = '"atlantis"."library_book"'::regclass AND polpermissive) THEN
        CREATE POLICY "library_book_default_access" ON "atlantis"."library_book" AS PERMISSIVE USING (true) WITH CHECK (true);
    END IF;
END
$atlantis_default_access$;
CREATE INDEX IF NOT EXISTS "library_book_partition_idx" ON "atlantis"."library_book" ("tenant");

