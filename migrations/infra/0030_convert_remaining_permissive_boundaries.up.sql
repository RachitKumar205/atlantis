-- Convert the tenant boundaries migration 0025 could not see.
--
-- 0025 selected the policies to invert by name: `polname LIKE '%\_tenant\_isolation'`.
-- codegen does not always emit that name. partitionPolicyName is
-- truncateIdent(tableName || '_tenant_isolation'), and truncateIdent replaces
-- everything past 54 bytes with '_' plus eight hex digits of the name's sha256.
-- `_tenant_isolation` is 17 bytes, so any table name of 47 bytes or more — a
-- namespace and an entity name, nothing exotic — produces a policy called
-- something like `analytics_customer_engagement_daily_rollup_tenant_is_9f3ac21b`.
-- The LIKE does not match it. 0025 walked past the table and reported success.
--
-- What that leaves behind is not a cosmetic gap. The boundary stays in the
-- PERMISSIVE slot, where policies OR, and 0025 is the change that made it safe
-- to delete emitForeignPolicyGuard — the check that used to REFUSE any
-- migration against a table carrying a second permissive policy. So on exactly
-- those tables the guard is gone and the condition it guarded against is
-- reachable: one access-control grant, and every tenant's rows are readable
-- through it. Nothing reports this. `tide inspect` compares declarations, and
-- the declaration is correct.
--
-- Selected by DEPENDENCY, not by name
-- -----------------------------------
-- A name is a description of a policy; what MAKES a policy the tenant boundary
-- is that its predicate calls atlantis.current_partition(). Postgres records
-- that in pg_depend — it is why DROP FUNCTION on a function a policy uses is
-- refused — so the catalogue can be asked the real question directly. That
-- answer is immune to truncation, to hand-edited names, and to whatever
-- truncateIdent does next.
--
-- The consequence worth stating: this also catches a permissive policy an
-- operator wrote themselves that happens to call current_partition(). Moving it
-- is still correct. The predicate is carried verbatim, and restrictive policies
-- AND where permissive ones OR, so the policy ends up exactly as strict as it
-- was or stricter. A boundary that fails closed is the failure mode to prefer.
--
-- Idempotent against a database 0025 handled correctly: those boundaries are
-- already restrictive, so the loop does not select them and this migration does
-- nothing.
--
-- The grant is CONDITIONAL, unlike 0025's
-- ---------------------------------------
-- 0025 created `<table>_default_access` unconditionally. On a table where the
-- operator had already written their own permissive grants, that adds
-- `USING (true)` beside them — and because permissive policies OR, that does
-- not add a policy, it stops every one of theirs from constraining anything.
-- The same defect lived in emitPartitionPolicy and is fixed there in the same
-- change as this file. Here the grant is created only when the table would
-- otherwise be left admitting nothing, which is the only reason it exists.
--
-- Why the loop terminates
-- -----------------------
-- It mutates pg_policy while iterating over pg_policy, and the rename it does
-- first produces a row that still satisfies its own WHERE: `<name>_legacy` is
-- permissive and still calls current_partition(). A loop that re-read the
-- catalogue would find that row, and the iteration for it would reach an
-- ALTER POLICY on something the previous iteration had already dropped.
--
-- It does not, because FOR ... IN <query> takes its snapshot when the query
-- opens and rows created inside the loop are not visible to it. That is the
-- load-bearing property of this whole migration and it is not visible in the
-- text, so it is stated here and driven by
-- TestMigration0030HandlesATableWithTwoTenantScopedPermissivePolicies — a
-- fixture with two matching policies, which is the smallest case where a
-- re-scan and a snapshot behave differently. A single-policy fixture cannot
-- tell them apart.
--
-- Isolation holds after EVERY statement
-- -------------------------------------
--   after RENAME   permissive {legacy, ...}                    -> unchanged
--   after CREATE   permissive {legacy, ...}, restrictive {t}   -> narrower
--   after GRANT    permissive {legacy, true}, restrictive {t}  -> tenant-scoped
--   after DROP     permissive {true|operator's}, restrictive {t} -> tenant-scoped
--
-- One transaction anyway, but a migration whose correctness depends on the
-- transaction cannot be reasoned about by an operator running it by hand.

-- Replicates internal/codegen.truncateIdent, byte for byte.
--
-- Postgres length() counts characters and Go's len() counts bytes, so 0025's
-- inline version of this is exact only for ASCII names. The DSL admits any
-- unicode letter. Walking characters and stopping before the prefix would
-- exceed 54 BYTES lands on the same offset Go's `for i := range name` picks,
-- because every rune start is a character boundary.
--
-- pg_temp, so it disappears with the session and leaves nothing to maintain.
-- OR REPLACE because this is a repair migration and an operator re-running the
-- file in the same psql session is an expected thing to do; a plain CREATE
-- fails the second time with "function already exists", which reads as the
-- migration itself being broken.
CREATE OR REPLACE FUNCTION pg_temp.atl_ident(name text) RETURNS text AS $fn$
DECLARE
    cut int := 0;
    i   int;
BEGIN
    IF octet_length(name) <= 63 THEN
        RETURN name;
    END IF;
    FOR i IN 1..length(name) LOOP
        EXIT WHEN octet_length(left(name, i)) > 54;
        cut := i;
    END LOOP;
    RETURN left(name, cut) || '_' || substr(encode(sha256(name::bytea), 'hex'), 1, 8);
END
$fn$ LANGUAGE plpgsql IMMUTABLE;

DO $atl0030$
DECLARE
    fn         oid;
    r          record;
    legacy     text;
    grant_name text;
    check_expr text;
    others     int;
BEGIN
    fn := to_regprocedure('atlantis.current_partition()');
    IF fn IS NULL THEN
        -- Nothing has ever been isolated in this database. to_regprocedure
        -- rather than a ::regprocedure cast so that is a no-op rather than an
        -- error naming a function the operator has never heard of.
        RAISE NOTICE 'atlantis: atlantis.current_partition() is absent, so there are no tenant boundaries to convert';
        RETURN;
    END IF;

    FOR r IN
        SELECT ns.nspname                              AS schema_name,
               c.relname                               AS table_name,
               p.polname                               AS pol_name,
               pg_get_expr(p.polqual, p.polrelid)      AS qual,
               pg_get_expr(p.polwithcheck, p.polrelid) AS with_check
          FROM pg_policy p
          JOIN pg_class c      ON c.oid = p.polrelid
          JOIN pg_namespace ns ON ns.oid = c.relnamespace
         WHERE p.polpermissive
           AND EXISTS (SELECT 1
                         FROM pg_depend d
                        WHERE d.classid    = 'pg_policy'::regclass
                          AND d.objid      = p.oid
                          AND d.refclassid = 'pg_proc'::regclass
                          AND d.refobjid   = fn)
         ORDER BY ns.nspname, c.relname, p.polname
    LOOP
        -- A policy with no USING clause is write-only and is not a boundary
        -- anything reads through. Leave what is not recognised alone.
        CONTINUE WHEN r.qual IS NULL;

        legacy     := pg_temp.atl_ident(r.pol_name || '_legacy');
        grant_name := pg_temp.atl_ident(r.table_name || '_default_access');

        -- For ALL and UPDATE a null WITH CHECK means Postgres reuses USING as
        -- the check. Making it explicit keeps the restrictive policy's write
        -- half exactly as strict as the permissive one's was.
        check_expr := coalesce(r.with_check, r.qual);

        EXECUTE format('ALTER POLICY %I ON %I.%I RENAME TO %I',
                       r.pol_name, r.schema_name, r.table_name, legacy);

        EXECUTE format('CREATE POLICY %I ON %I.%I AS RESTRICTIVE USING (%s) WITH CHECK (%s)',
                       r.pol_name, r.schema_name, r.table_name, r.qual, check_expr);

        -- Everything permissive EXCEPT the boundary about to be dropped. If
        -- that count is zero the table would admit nothing once the legacy
        -- policy goes, because restrictive policies only ever subtract.
        SELECT count(*) INTO others
          FROM pg_policy p2
          JOIN pg_class c2      ON c2.oid = p2.polrelid
          JOIN pg_namespace ns2 ON ns2.oid = c2.relnamespace
         WHERE ns2.nspname = r.schema_name
           AND c2.relname  = r.table_name
           AND p2.polpermissive
           AND p2.polname <> legacy;

        IF others = 0 THEN
            EXECUTE format('CREATE POLICY %I ON %I.%I AS PERMISSIVE USING (true) WITH CHECK (true)',
                           grant_name, r.schema_name, r.table_name);
        END IF;

        EXECUTE format('DROP POLICY %I ON %I.%I',
                       legacy, r.schema_name, r.table_name);

        RAISE NOTICE 'atlantis: tenant isolation on %.% (policy %) is now RESTRICTIVE',
                     r.schema_name, r.table_name, r.pol_name;
    END LOOP;
END
$atl0030$;
