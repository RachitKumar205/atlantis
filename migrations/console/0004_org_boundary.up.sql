-- The organisation becomes a database boundary.
--
-- One console process serves many organisations. Until now nothing separated
-- their rows: `listAuditLog` was `SELECT … FROM console.audit_log ORDER BY
-- created_at DESC LIMIT $1` with no filter of any kind, so the Activity page
-- would have shown every organisation's actions to every organisation.
--
-- This mirrors the boundary the caller side already runs — a RESTRICTIVE policy
-- reading a transaction-local discriminator through a function. See
-- migrations/infra/0024 and 0025 for the reasoning behind each of those
-- choices; the notes below cover only what differs here.

-- The registry.
--
-- Deliberately NOT org-scoped: it is the list of organisations, so a policy
-- keyed on the organisation would make it unreadable by design. Nothing
-- sensitive lives here — the name is already in every assertion.
--
-- Step 5 hangs per-organisation endpoints and client certificates off this, and
-- a step-6 organisation switcher lists it.
CREATE TABLE IF NOT EXISTS console.orgs (
    org        TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The discriminator.
--
-- A transaction-local GUC, read through a function.
--
-- The server rejected exactly this shape in migration 0021 and then adopted it
-- in 0024. Worth understanding before touching it: a custom GUC is PGC_USERSET,
-- so any SQL running in the session can reassign it, and `REVOKE SET ON
-- PARAMETER` does not help — it is a no-op on a placeholder GUC. On the server
-- that mattered because atlantis executes caller-authored SQL (query bodies,
-- CHECK expressions, backfill expressions) in the same session as the reads the
-- policy constrains, and 0024 was only safe because internal/dsl/sqlvalidate
-- closes that hole outside the database.
--
-- The console executes no caller-authored SQL at all. Every statement it issues
-- is a literal in internal/console with bound parameters. The premise of that
-- attack is absent here, which is why this needs no equivalent of sqlvalidate.
-- Said plainly so nobody later "fixes" the inconsistency by importing a defence
-- against a threat this process does not have.
--
-- STABLE and LANGUAGE sql, with no SECURITY DEFINER and no SET search_path:
-- those two attributes block planner inlining, and the only object referenced
-- is schema-qualified to pg_catalog so no search_path pin is needed.
CREATE OR REPLACE FUNCTION console.current_org()
RETURNS text LANGUAGE sql STABLE AS $$
    SELECT nullif(pg_catalog.current_setting('console.org', true), '')
$$;

-- The nullif is load-bearing, and for a reason that is not obvious.
--
-- A transaction-local set_config does NOT revert to NULL when the transaction
-- ends — it reverts to the EMPTY STRING. Without the nullif, the second and
-- every later request on a pooled connection would compare against '', and
-- `org = ''` matches any row whose org is '' — which is exactly what the
-- parked legacy audit rows below are. One forgotten bind would show them.
COMMENT ON FUNCTION console.current_org() IS
    'The organisation bound to the current transaction, or NULL. Never the empty string.';

CREATE OR REPLACE FUNCTION console.set_org(p_org text)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    IF p_org IS NULL OR p_org = '' THEN
        RAISE EXCEPTION 'console.set_org: organisation must not be empty';
    END IF;
    -- true = transaction-local. That is what makes this safe on a pooled
    -- connection: it reverts when the transaction ends, so a later request
    -- borrowing the same backend cannot inherit it.
    PERFORM pg_catalog.set_config('console.org', p_org, true);
END
$$;

GRANT EXECUTE ON FUNCTION console.current_org() TO PUBLIC;
GRANT EXECUTE ON FUNCTION console.set_org(text) TO PUBLIC;

-- The boundary on console.audit_log.

ALTER TABLE console.audit_log ADD COLUMN IF NOT EXISTS org TEXT NOT NULL DEFAULT '';

-- Legacy rows keep ''.
--
-- Rows written before this migration have no recoverable organisation:
-- console.users was dropped by 0003 and every session was deleted by it, so
-- there is nothing left to join against. They are parked at '' — a value
-- console.current_org() never returns — which means they stay in the table,
-- remain readable by an operator with direct database access, and appear in no
-- organisation's console.
--
-- Destroying audit history to make a migration tidy is not on the table.
-- Showing one organisation another's history is worse than showing neither.
DO $atl0004_notice$
DECLARE n bigint;
BEGIN
    SELECT count(*) INTO n FROM console.audit_log WHERE org = '';
    IF n > 0 THEN
        RAISE NOTICE 'atlantis: % audit row(s) predate organisation scoping and are parked at org = ''''. They are visible to no organisation. To attribute them, UPDATE console.audit_log SET org = ''<your-org>'' WHERE org = ''''.', n;
    END IF;
END
$atl0004_notice$;

-- Dropped so a future INSERT that forgets the organisation fails loudly rather
-- than quietly writing a row nobody can see.
ALTER TABLE console.audit_log ALTER COLUMN org DROP DEFAULT;

ALTER TABLE console.audit_log ENABLE ROW LEVEL SECURITY;

-- FORCE, not merely ENABLE. The console's role OWNS these tables, and the
-- ordinary owner exemption would leave the policy applying to everyone except
-- the one role that actually connects. `\d` would list the policy and every
-- read would return every organisation's rows.
--
-- FORCE does not close BYPASSRLS or superuser. internal/console/roleguard.go
-- refuses to start on either, which is the half of this that lives in Go.
ALTER TABLE console.audit_log FORCE ROW LEVEL SECURITY;

-- The boundary goes in the RESTRICTIVE slot.
--
-- PostgreSQL admits a row when ANY permissive policy allows it AND EVERY
-- restrictive policy allows it. A permissive boundary works only while it is
-- the sole policy on the table — a second permissive policy ORs with it, so
-- `USING (true)` beside it returns everything. Restrictive policies AND, so
-- nothing added later can widen past this one.
--
-- WITH CHECK carries the same predicate as USING. USING gates what a statement
-- may READ; WITH CHECK what it may WRITE.
--
-- Be precise about why both are written, because the obvious reason is wrong
-- and was measured on PostgreSQL 17 before this went in: omitting WITH CHECK
-- entirely does NOT open a write hole. PostgreSQL reuses USING as the write
-- check when WITH CHECK is absent, and a cross-organisation INSERT is refused
-- naming this policy either way.
--
-- Writing both is still right, for a weaker reason: it states the intent, and
-- it pins the write half if somebody later gives the read half a different
-- predicate. The shape that genuinely leaks is an explicit `WITH CHECK (true)`
-- — which is what a well-meaning "let writes through" edit produces, and which
-- this line makes it obvious you are choosing.
DROP POLICY IF EXISTS audit_log_org_isolation ON console.audit_log;
CREATE POLICY audit_log_org_isolation ON console.audit_log
    AS RESTRICTIVE
    USING (org = console.current_org())
    WITH CHECK (org = console.current_org());

-- Restrictive policies only subtract, so the boundary alone is deny-all. This
-- is the grant that admits anything at all, and the one an operator may
-- replace with something narrower.
DROP POLICY IF EXISTS audit_log_default_access ON console.audit_log;
CREATE POLICY audit_log_default_access ON console.audit_log
    AS PERMISSIVE USING (true) WITH CHECK (true);

CREATE INDEX IF NOT EXISTS console_audit_log_org_idx ON console.audit_log (org);

-- The partitions.
--
-- Measured on PostgreSQL 17, because this is not what one would assume: a
-- child created by CREATE TABLE … PARTITION OF inherits NOTHING. Not
-- relrowsecurity, not relforcerowsecurity, not the policies. Reading a child
-- table DIRECTLY returned every organisation's rows — bound or unbound — while
-- the parent behaved correctly.
--
-- Enabling and forcing RLS on the child, with no policy of its own, makes a
-- direct read return zero either way. Nothing reads children directly: the
-- console only ever queries the parent, and dropAuditPartitionsOlderThan uses
-- DROP TABLE, which is DDL and outside RLS entirely. So a direct read is a bug
-- or an attack, and deny-all is the right answer to both.
--
-- This covers the partitions that exist now. store.ensureAuditPartition does
-- the same for every partition it creates from here on, because partitions are
-- a function of the calendar and cannot live in a migration.
DO $atl0004_parts$
DECLARE child regclass;
BEGIN
    FOR child IN
        SELECT inhrelid::regclass
          FROM pg_inherits
         WHERE inhparent = 'console.audit_log'::regclass
    LOOP
        EXECUTE format('ALTER TABLE %s ENABLE ROW LEVEL SECURITY', child);
        EXECUTE format('ALTER TABLE %s FORCE ROW LEVEL SECURITY', child);
    END LOOP;
END
$atl0004_parts$;
