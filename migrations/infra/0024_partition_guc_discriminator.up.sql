-- Move the tenant discriminator from a table back to a run-time parameter.
--
-- Measured on PostgreSQL 17.8, 200k rows over 4 tenants, identical indexes, as
-- a NOSUPERUSER NOBYPASSRLS role:
--
--                                       GUC          table
--   transaction ids per 100 reads         0            100
--   WAL per 500 binds                       0         135 kB
--   10,000 discriminator calls         1.14 ms      14.30 ms
--                                  (0.114 us/call) (1.43 us/call)
--   bound point read, 8 conns     21,399 tps     17,336 tps
--   dead rows after 20 s                  0        122,449
--
-- The transaction-id row is a count, not a percentage: set_partition INSERTed,
-- and every write assigns a transaction id.
--
-- At the measured 17,336 bound reads per second:
--
--   * autovacuum_freeze_max_age (200,000,000) is reached in 3.2 hours. That is
--     the trigger for the anti-wraparound vacuum, not a failure.
--   * The refusal comes at the 2^31 wraparound point — 1.43 days — NOT at 2^32,
--     and only if that vacuum cannot complete. A long-running transaction, a
--     stale replication slot or an orphaned prepared transaction arranges that.
--
-- A custom GUC is PGC_USERSET, so caller-authored SQL can reassign it and no
-- database-side lock exists: `REVOKE SET ON PARAMETER "atlantis.tenant" FROM
-- PUBLIC` does not create a pg_parameter_acl row for a placeholder GUC,
-- verified on 17.8. Two changes ship with this migration and are what make it
-- safe:
--
--   1. internal/dsl/sqlvalidate rejects set_config() and set_partition() in
--      seven caller-authored SQL surfaces: query bodies, procedure steps,
--      table-level and per-field CHECK expressions, partial-index predicates,
--      `default raw`, `backfill` expressions spliced into a live UPDATE by
--      internal/backfill/splicer.go, and `index by expr` emitted into CREATE
--      INDEX. The statement gate is a permit-list, so `SET` was already
--      refused; the function-call form was not. A CHECK expression is emitted
--      verbatim into DDL and PostgreSQL does not require it to be IMMUTABLE,
--      so a set_config planted there fires on every INSERT and rebinds a
--      correctly bound transaction. This list has been believed exhaustive and
--      been wrong three times.
--   2. internal/storage/pg clears the parameter as each connection is opened,
--      so a value cannot arrive from a server default, a role default, or a
--      pooler returning a backend another session used. Transaction-locality
--      stops a value outliving its own request; this covers values that were
--      never set by this process.
--
-- Applying this migration without both is a cross-tenant read. Apply it AFTER
-- the binary carrying them is running: a server predating the gate serves a
-- checkpoint the gate would refuse, and this migration makes that checkpoint
-- exploitable. The new binary re-audits the stored schema at boot (cmd/server,
-- sqlvalidate.AuditForbiddenCalls), which also covers SQL stored before the
-- gate existed.
--
-- Residual exposure: anyone who can get a query body through `tide apply` may
-- find a vector the validator does not know about. That person can equally
-- drop the policy or redeclare the entity without `partition by`. The boundary
-- this protects is the accidental leak — a forgotten predicate, a new handler.

-- current_partition is what every RLS policy calls, so its cost is paid on
-- every read of every partitioned table.
--
-- LANGUAGE sql and STABLE, with no SECURITY DEFINER and no SET search_path.
-- Both of those attributes block planner inlining, at 0.606 us per call against
-- 0.114 us, and neither is needed: reading a GUC requires no privilege, and the
-- only object in the body is schema-qualified to pg_catalog below, which a
-- caller cannot shadow.
--
-- nullif is required. A transaction-local set reverts to the empty string, not
-- to NULL, when the transaction ends — verified on 17.8. On the second and
-- every later request on a pooled connection an unbound transaction would see
-- '', and `tenant_col = ''` matches a row whose discriminator is the empty
-- string rather than matching nothing. The column is NOT NULL, which does not
-- exclude ''. Mapping '' back to NULL keeps that case fail-closed.
CREATE OR REPLACE FUNCTION atlantis.current_partition()
RETURNS text
LANGUAGE sql
STABLE
AS $$
    SELECT nullif(pg_catalog.current_setting('atlantis.tenant', true), '');
$$;

GRANT EXECUTE ON FUNCTION atlantis.current_partition() TO PUBLIC;

-- set_partition keeps its name and signature.
--
-- It could be inlined into the Go caller as a bare set_config, and that would
-- be marginally faster. Keeping the function keeps one named seam for the
-- bind, and keeps a rolling deploy safe: a pod still running the previous
-- build calls atlantis.set_partition($1) and gets the new mechanism.
--
-- The already-set check from 0021 is kept and is NO LONGER a security
-- boundary. It cannot be one: caller SQL can set the parameter to '' and then
-- set it again, and this function would never see it. It is retained because
-- it still catches the accidental double bind — two code paths both binding
-- one transaction — which is a real defect and worth an error rather than a
-- silent overwrite. Do not cite it as protection.
CREATE OR REPLACE FUNCTION atlantis.set_partition(p_tenant text)
RETURNS void
LANGUAGE plpgsql
SET search_path = atlantis, pg_catalog
AS $$
BEGIN
    IF p_tenant IS NULL OR p_tenant = '' THEN
        RAISE EXCEPTION 'atlantis: partition must be a non-empty value';
    END IF;

    IF nullif(pg_catalog.current_setting('atlantis.tenant', true), '') IS NOT NULL THEN
        RAISE EXCEPTION 'atlantis: partition already set for this transaction';
    END IF;

    -- true = transaction-local. A session-level set would outlive the request
    -- on a pooled connection and hand this tenant to the next caller.
    PERFORM pg_catalog.set_config('atlantis.tenant', p_tenant, true);
END;
$$;

GRANT EXECUTE ON FUNCTION atlantis.set_partition(text) TO PUBLIC;

-- The discriminator table holds no state worth keeping: at most one live row
-- per backend, plus one orphan per backend that ever connected and exited.
-- Dropping it loses nothing a transaction needs. Leaving it would leave a table with
-- a SECURITY DEFINER writer that nothing reads, which is how a reader six
-- months from now concludes the table is still the mechanism.
--
-- It also had a defect worth recording: set_partition deleted only the row for
-- the current backend, so a backend that disconnected orphaned its row and no
-- job ever removed it.
DROP TABLE IF EXISTS atlantis.session_partition;
