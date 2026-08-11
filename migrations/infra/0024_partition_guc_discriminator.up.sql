-- Move the tenant discriminator from a table back to a run-time parameter.
--
-- Migration 0021 rejected a GUC and chose a table. That reasoning was sound
-- about the threat and wrong about the price, and the price is only visible
-- when you measure it. Measured on PostgreSQL 17.8, 200k rows over 4 tenants,
-- identical indexes, as a NOSUPERUSER NOBYPASSRLS role:
--
--                                       GUC          table
--   transaction ids per 100 reads         0            100
--   WAL per 500 binds                       0         135 kB
--   10,000 discriminator calls         1.14 ms      14.30 ms
--                                  (0.114 us/call) (1.43 us/call)
--   bound point read, 8 conns     21,399 tps     17,336 tps
--   dead rows after 20 s                  0        122,449
--
-- The transaction-id row is not a percentage. set_partition INSERTed, and every
-- write assigns a transaction id. Migration 0023 removed this burn from the
-- read path; binding put it back on the write path, where it is mandatory
-- rather than incidental.
--
-- The arithmetic, stated carefully because the round numbers invite a
-- factor-of-two error. At the measured 17,336 bound reads per second:
--
--   * autovacuum_freeze_max_age (200,000,000) is reached in 3.2 hours. That is
--     the trigger for the anti-wraparound vacuum, not a failure.
--   * The refusal comes at the 2^31 wraparound point — 1.43 days — NOT at 2^32,
--     and only if that vacuum cannot complete. A long-running transaction, a
--     stale replication slot or an orphaned prepared transaction arranges that.
--
-- So the failure is neither certain nor three days away. It is a database that
-- refuses writes, reachable in under two days by read volume alone, on a
-- feature whose entire purpose is to be safe to read from.
--
-- What 0021 got right, and this does not fix
-- ------------------------------------------
-- A custom GUC is PGC_USERSET. Caller-authored SQL can reassign it, and no
-- database-side lock exists: `REVOKE SET ON PARAMETER "atlantis.tenant" FROM
-- PUBLIC` does not even create a pg_parameter_acl row for a placeholder GUC,
-- re-verified on 17.8. So this migration alone would reopen exactly the hole
-- 0021 closed.
--
-- It does not ship alone. Two changes land with it and are what make it safe:
--
--   1. internal/dsl/sqlvalidate rejects set_config() and set_partition() in
--      five caller-authored SQL surfaces: query bodies, procedure steps, CHECK
--      expressions (table-level and per-field), partial-index predicates, and
--      `default raw`. The statement gate is already a permit-list, so `SET` was
--      refused before this; the function-call form was not. The first version
--      covered two of the five, and a review broke it in minutes: a CHECK
--      expression is emitted verbatim into DDL, PostgreSQL does not require it
--      to be IMMUTABLE, and a set_config planted there fires on every INSERT
--      and rebinds a transaction that was correctly bound.
--
--      Seven, in fact: `backfill` expressions, spliced verbatim into a live
--      UPDATE by internal/backfill/splicer.go, and `index by expr`, emitted
--      verbatim into CREATE INDEX. Both are gated now. The second was the worst
--      of the seven — its escape appended whole statements, one of which
--      dropped the very policy every other check here protects.
--
--      Do not read this list as exhaustive. It has been called exhaustive three
--      times and been wrong three times.
--   2. internal/storage/pg clears the parameter as each connection is opened,
--      so a value cannot arrive from a server default, a role default, or a
--      pooler handing back a backend somebody else used. (Transaction-locality
--      is what stops a value outliving its own request; this covers the values
--      that were never ours to begin with.)
--
-- Neither is optional. Applying this migration without them is a cross-tenant
-- read, not a performance change. Apply it AFTER the binary carrying them is
-- running, not before: a server predating the gate serves a checkpoint the gate
-- would refuse, and this migration is what makes that checkpoint exploitable.
-- Nothing can stop such a binary running, so the new one re-audits the stored
-- schema at boot (cmd/server, sqlvalidate.AuditForbiddenCalls) and declines to
-- serve what an older one let in. That audit is also what covers SQL stored
-- before the gate existed at all, which no amount of deploy ordering reaches.
--
-- The residual exposure, stated plainly: a person who can write a query body
-- and get it through `tide apply` may find a vector the validator does not
-- know about. That person can also drop the policy, redeclare the entity
-- without `partition by`, or write a body that reads whatever they like. The
-- boundary this protects is the accidental leak — a forgotten predicate, a new
-- handler — which is the leak that actually happens.

-- current_partition is what every RLS policy calls, so its cost is paid on
-- every read of every partitioned table.
--
-- LANGUAGE sql and STABLE with no SECURITY DEFINER and no SET search_path, all
-- three deliberately: those two attributes block planner inlining, and the
-- difference is 0.606 us per call against 0.114 us. Neither is needed any
-- more. SECURITY DEFINER existed to read a table the caller had no privilege
-- on; reading a GUC needs no privilege. search_path pinning guarded object
-- resolution inside the body; the only object left is schema-qualified to
-- pg_catalog below, which a caller cannot shadow.
--
-- nullif is not cosmetic. A transaction-local set does NOT revert to NULL when
-- the transaction ends — it reverts to the empty string, verified on 17.8. So
-- on the second and every later request on a pooled connection, an unbound
-- transaction would see '' rather than NULL, and `tenant_col = ''` matches a
-- row whose discriminator is the empty string instead of matching nothing.
-- The column is NOT NULL, which does not exclude ''. Mapping '' back to NULL
-- restores fail-closed for that case at no measurable cost.
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
