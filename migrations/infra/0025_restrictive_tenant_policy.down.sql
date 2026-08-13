-- Put the tenant boundary back in the PERMISSIVE slot.
--
-- The reverse of the up migration, in the mirror order, and with the same
-- property: isolation is never wider after a statement than it was before it.
--
--   after RENAME   restrictive {legacy, tenant-scoped},
--                  permissive {true}                          -> tenant-scoped
--   after CREATE   permissive {true, tenant-scoped} = true,
--                  restrictive {legacy}                       -> tenant-scoped
--   after DROP grant   permissive {tenant-scoped},
--                      restrictive {legacy}                   -> tenant-scoped
--   after DROP legacy  permissive {tenant-scoped}             -> tenant-scoped
--
-- The grant is dropped BEFORE the restrictive boundary. Dropping the boundary
-- first would leave `USING (true)` as the only policy — every tenant's rows to
-- every caller — for the width of one statement.
--
-- Rolling back reinstates the limitation the up migration removed: a permissive
-- boundary can be widened by any other permissive policy, so a database with
-- user-defined access-control grants is NOT safe on this side of the migration.
-- Nothing here can detect that, because a grant written for RBAC is
-- indistinguishable from any other permissive policy. Check before rolling back.
DO $atl0025down$
DECLARE
    r          record;
    legacy     text;
    grant_name text;
    check_expr text;
BEGIN
    FOR r IN
        SELECT n.nspname                               AS schema_name,
               c.relname                               AS table_name,
               p.polname                               AS pol_name,
               pg_get_expr(p.polqual, p.polrelid)      AS qual,
               pg_get_expr(p.polwithcheck, p.polrelid) AS with_check
          FROM pg_policy p
          JOIN pg_class c     ON c.oid = p.polrelid
          JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE NOT p.polpermissive
           AND p.polname LIKE '%\_tenant\_isolation'
         ORDER BY n.nspname, c.relname, p.polname
    LOOP
        CONTINUE WHEN r.qual IS NULL;

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

        check_expr := coalesce(r.with_check, r.qual);

        EXECUTE format('ALTER POLICY %I ON %I.%I RENAME TO %I',
                       r.pol_name, r.schema_name, r.table_name, legacy);

        EXECUTE format('CREATE POLICY %I ON %I.%I AS PERMISSIVE USING (%s) WITH CHECK (%s)',
                       r.pol_name, r.schema_name, r.table_name, r.qual, check_expr);

        -- IF EXISTS: the grant is only present on tables the up migration
        -- touched or codegen created since. A table whose grant an operator
        -- already replaced with their own must still roll back.
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I.%I',
                       grant_name, r.schema_name, r.table_name);

        EXECUTE format('DROP POLICY %I ON %I.%I',
                       legacy, r.schema_name, r.table_name);
    END LOOP;
END
$atl0025down$;
