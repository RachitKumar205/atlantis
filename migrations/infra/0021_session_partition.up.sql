-- SUPERSEDED BY 0024. The table this migration creates has been dropped and
-- the discriminator is a run-time parameter again. The reasoning below about
-- why a GUC is attackable is still correct and still worth reading; what it
-- omits is the price, which nobody had measured: the table costs one
-- transaction ID per bind, which exhausts the 32-bit space in under three days
-- at load. 0024 carries the numbers and the three defences that make the
-- parameter safe. Read this file as history, not as the mechanism.

-- The tenant discriminator for row-level security, deliberately NOT a GUC.
--
-- The obvious RLS design is `SET LOCAL atlantis.partition = $1` with a policy
-- on current_setting(). It does not work here, and the reason is specific to
-- this product: atlantis executes caller-authored SQL — custom-query bodies,
-- CHECK expressions, backfill expressions — in the same session as the reads
-- the policy is meant to constrain. A custom GUC is PGC_USERSET, so that SQL
-- can simply reassign it. Verified against PostgreSQL 16 with a role holding
-- NOSUPERUSER NOCREATEDB NOCREATEROLE and nothing but SELECT:
--
--   SET atlantis.partition = 'acme';
--   SELECT set_config('atlantis.partition', 'victim', false);
--   SELECT * FROM t;              -- returns the victim's rows
--
-- REVOKE SET ON PARAMETER (PG15+) does not close it: it is a no-op on a
-- placeholder GUC, verified the same way. Role-based RLS on current_user does
-- not close it either wherever a pooled connection holds membership in the
-- tenant roles, because RESET ROLE returns to the member role.
--
-- So the discriminator lives somewhere caller SQL cannot reach: a table it has
-- no privileges on, written only through a SECURITY DEFINER function that
-- refuses to overwrite within a transaction. The caller may call the setter —
-- it is granted to everyone — and gets an error rather than a new value.
--
-- Attacks this was verified against, all on live PostgreSQL 16:
--
--   set_config on the old GUC     no effect; the policy reads no GUC
--   calling the setter again      ERROR: partition already set
--   UPDATE on the table           ERROR: permission denied
--   savepoint-rollback, re-set    ERROR: partition already set
--   no partition set at all       returns zero rows — fails closed
--
-- The alternative was a C extension defining the GUC at PGC_SUSET. This needs
-- no extension, so it runs on stock Postgres and does not pin the tenant image
-- to a PostgreSQL major version.

CREATE TABLE IF NOT EXISTS atlantis.session_partition (
    -- Keyed by backend pid AND transaction id. The pid alone is not enough:
    -- pids are reused across connections, and a stale row would silently grant
    -- a new session the previous one's tenant. The xid scopes the value to the
    -- transaction that set it, which is also what makes "set once" meaningful.
    pid     integer PRIMARY KEY,
    xid     bigint  NOT NULL,
    tenant  text    NOT NULL,
    set_at  timestamptz NOT NULL DEFAULT now()
);

-- UNLOGGED would be preferable — this is per-session scratch state that must
-- not survive a restart — but an UNLOGGED table cannot be created IF NOT
-- EXISTS over a LOGGED one on re-run, and the write volume is one row per
-- transaction. Revisit with a benchmark rather than by assertion.

-- No privileges for anyone but the owner. This is the boundary: a caller that
-- could UPDATE this table could assign itself any tenant.
REVOKE ALL ON atlantis.session_partition FROM PUBLIC;

-- set_partition records the tenant for this transaction, once.
--
-- SECURITY DEFINER so it can write a table the caller cannot. The
-- already-set check is what makes it safe to grant to everyone: caller SQL may
-- invoke it and will be refused, because atlantis has already set the value at
-- the start of the transaction.
--
-- search_path is pinned. A SECURITY DEFINER function that resolves objects
-- through the caller's search_path is the classic privilege-escalation vector
-- (CVE-2018-1058); pg_temp is excluded so a caller cannot shadow a referenced
-- object with a temporary one.
CREATE OR REPLACE FUNCTION atlantis.set_partition(p_tenant text)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = atlantis, pg_catalog
AS $$
DECLARE
    existing text;
BEGIN
    IF p_tenant IS NULL OR p_tenant = '' THEN
        RAISE EXCEPTION 'atlantis: partition must be a non-empty value';
    END IF;

    SELECT tenant INTO existing
      FROM atlantis.session_partition
     WHERE pid = pg_backend_pid() AND xid = txid_current();

    IF existing IS NOT NULL THEN
        -- Not "silently ignore" and not "overwrite". Overwriting is the whole
        -- attack; ignoring would leave the caller believing it had switched
        -- tenants, which is a worse way to find out.
        RAISE EXCEPTION 'atlantis: partition already set for this transaction';
    END IF;

    -- Clear any row left by a previous transaction on this backend before
    -- claiming the pid.
    DELETE FROM atlantis.session_partition WHERE pid = pg_backend_pid();
    INSERT INTO atlantis.session_partition (pid, xid, tenant)
    VALUES (pg_backend_pid(), txid_current(), p_tenant);
END;
$$;

-- current_partition is what RLS policies call.
--
-- STABLE rather than IMMUTABLE: the value is fixed within a transaction, which
-- is exactly what STABLE promises, and marking it IMMUTABLE would let the
-- planner cache it across transactions on the same connection.
--
-- Returns NULL when nothing is set, so `tenant_col = current_partition()` is
-- NULL and the policy matches no rows. Absence fails closed.
CREATE OR REPLACE FUNCTION atlantis.current_partition()
RETURNS text
LANGUAGE sql
SECURITY DEFINER
STABLE
SET search_path = atlantis, pg_catalog
AS $$
    SELECT tenant
      FROM atlantis.session_partition
     WHERE pid = pg_backend_pid() AND xid = txid_current();
$$;

GRANT EXECUTE ON FUNCTION atlantis.set_partition(text) TO PUBLIC;
GRANT EXECUTE ON FUNCTION atlantis.current_partition() TO PUBLIC;
