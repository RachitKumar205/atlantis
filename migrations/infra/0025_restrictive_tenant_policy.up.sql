-- Move tenant isolation from the PERMISSIVE slot to the RESTRICTIVE one.
--
-- PostgreSQL admits a row when ANY permissive policy allows it AND EVERY
-- restrictive policy allows it. Until now atlantis put the tenant boundary in
-- the permissive slot, which works only while it is the sole policy on the
-- table: a second permissive policy ORs with it, so `USING (true)` beside the
-- boundary returns every tenant's rows.
--
-- The defence was emitForeignPolicyGuard, which aborted the migration whenever
-- the table carried any other permissive policy. That held the line and made
-- user-defined access control impossible — the first RBAC grant anyone wrote
-- stopped `tide apply` working on that table, and the guard's advice ("make
-- them RESTRICTIVE") inverts the logic, since restrictive policies AND and so
-- cannot express "admins OR auditors may read this".
--
-- With the boundary restrictive, a user may add whatever permissive policies
-- their authorization model needs. Each is a grant; none can reach outside the
-- tenant. Verified on PG 17 against a deliberately hostile
-- `AS PERMISSIVE USING (true) WITH CHECK (true)`: bound reads returned only the
-- bound tenant, unbound returned zero, a forged INSERT was refused naming the
-- boundary policy, and a cross-tenant UPDATE touched nothing.
--
-- What this migration touches
-- ---------------------------
-- Not an atlantis-owned table. Tenant policies live on CALLER tables, emitted
-- into caller migrations by codegen, so this finds them by name: every
-- permissive policy called `<table>_tenant_isolation`. That is the first infra
-- migration to reach into caller objects, which is why the selection is by
-- exact suffix rather than anything looser.
--
-- Isolation holds after EVERY statement, not just at commit
-- --------------------------------------------------------
-- The order below is chosen so that no intermediate state is wider than the
-- state before it. It runs in one transaction anyway, but a migration whose
-- correctness depends on the transaction is a migration that cannot be
-- reasoned about when someone runs it by hand.
--
--   after RENAME   permissive {legacy, tenant-scoped}          -> tenant-scoped
--   after CREATE   permissive {legacy}, restrictive {tenant}   -> tenant-scoped
--   after GRANT    permissive {legacy, true} = true,
--                  restrictive {tenant}                        -> tenant-scoped
--   after DROP     permissive {true}, restrictive {tenant}     -> tenant-scoped
--
-- The predicate is CARRIED, never regenerated
-- -------------------------------------------
-- pg_get_expr returns whatever the table actually has, including an operator's
-- own hardening — `((deleted_at IS NULL) AND (tenant = current_partition()))`.
-- Regenerating a predicate from the declared column would silently drop that
-- extra clause and make soft-deleted rows visible to every caller. Carrying it
-- cannot: the boundary ends up exactly as strict as it was, or stricter.
DO $atl0025$
DECLARE
    r          record;
    legacy     text;
    grant_name text;
    check_expr text;
BEGIN
    FOR r IN
        SELECT n.nspname                                   AS schema_name,
               c.relname                                   AS table_name,
               p.polname                                   AS pol_name,
               pg_get_expr(p.polqual, p.polrelid)          AS qual,
               pg_get_expr(p.polwithcheck, p.polrelid)     AS with_check
          FROM pg_policy p
          JOIN pg_class c     ON c.oid = p.polrelid
          JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE p.polpermissive
           AND p.polname LIKE '%\_tenant\_isolation'
         ORDER BY n.nspname, c.relname, p.polname
    LOOP
        -- A permissive policy with that name and no USING clause is not the
        -- boundary; leave anything unrecognised alone rather than rewriting it.
        CONTINUE WHEN r.qual IS NULL;

        -- Identifier names must match what internal/codegen emits, or the next
        -- `tide apply` will not recognise these as its own and will create a
        -- second default grant beside this one. codegen's truncateIdent cuts to
        -- 63 bytes and appends "_" plus the first 8 hex digits of the name's
        -- sha256. Replicated here. Table names are snake_case ASCII, so left()
        -- counting characters rather than bytes is exact for them.
        legacy := left(r.pol_name || '_legacy', 54)
                  || '_' || substr(encode(sha256((r.pol_name || '_legacy')::bytea), 'hex'), 1, 8);
        IF length(r.pol_name || '_legacy') <= 63 THEN
            legacy := r.pol_name || '_legacy';
        END IF;

        grant_name := left(r.table_name || '_default_access', 54)
                      || '_' || substr(encode(sha256((r.table_name || '_default_access')::bytea), 'hex'), 1, 8);
        IF length(r.table_name || '_default_access') <= 63 THEN
            grant_name := r.table_name || '_default_access';
        END IF;

        -- For ALL and UPDATE a null WITH CHECK means PostgreSQL reuses USING as
        -- the check. Making that explicit here keeps the restrictive policy's
        -- write half exactly as strict as the permissive one's was.
        check_expr := coalesce(r.with_check, r.qual);

        EXECUTE format('ALTER POLICY %I ON %I.%I RENAME TO %I',
                       r.pol_name, r.schema_name, r.table_name, legacy);

        EXECUTE format('CREATE POLICY %I ON %I.%I AS RESTRICTIVE USING (%s) WITH CHECK (%s)',
                       r.pol_name, r.schema_name, r.table_name, r.qual, check_expr);

        EXECUTE format('CREATE POLICY %I ON %I.%I AS PERMISSIVE USING (true) WITH CHECK (true)',
                       grant_name, r.schema_name, r.table_name);

        EXECUTE format('DROP POLICY %I ON %I.%I',
                       legacy, r.schema_name, r.table_name);

        RAISE NOTICE 'atlantis: tenant isolation on %.% is now RESTRICTIVE, with % as the replaceable grant',
                     r.schema_name, r.table_name, grant_name;
    END LOOP;
END
$atl0025$;
