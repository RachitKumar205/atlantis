-- Reverse of 0004. Removes the organisation boundary.
--
-- Be clear about what this does: it takes the isolation off. After it runs,
-- every organisation's audit rows are readable by every organisation again.
-- It exists so a failed migration can be unwound to a known shape, which is
-- what golang-migrate needs — not as an operational step on a console that has
-- more than one organisation on it.
--
-- The `org` column is kept rather than dropped. Dropping it would destroy the
-- only record of which organisation each audit row belongs to, and that
-- attribution cannot be reconstructed. A column nothing reads is harmless; an
-- audit trail that has lost its subject is not.

-- Order matters, and it is the reverse of the up migration's: the switches come
-- off BEFORE the policies, so the intermediate state is "isolation off, policy
-- present" rather than "policy absent, isolation still forced" — which is
-- deny-all, and would make the console unusable partway through.
DO $atl0004_down_parts$
DECLARE child regclass;
BEGIN
    FOR child IN
        SELECT inhrelid::regclass
          FROM pg_inherits
         WHERE inhparent = 'console.audit_log'::regclass
    LOOP
        EXECUTE format('ALTER TABLE %s NO FORCE ROW LEVEL SECURITY', child);
        EXECUTE format('ALTER TABLE %s DISABLE ROW LEVEL SECURITY', child);
    END LOOP;
END
$atl0004_down_parts$;

ALTER TABLE console.audit_log NO FORCE ROW LEVEL SECURITY;
ALTER TABLE console.audit_log DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS audit_log_org_isolation ON console.audit_log;
DROP POLICY IF EXISTS audit_log_default_access ON console.audit_log;

DROP INDEX IF EXISTS console.console_audit_log_org_idx;

-- Restore the default so inserts that omit the organisation keep working
-- against the pre-0004 code, which does not set it.
ALTER TABLE console.audit_log ALTER COLUMN org SET DEFAULT '';

DROP FUNCTION IF EXISTS console.set_org(text);
DROP FUNCTION IF EXISTS console.current_org();

DROP TABLE IF EXISTS console.orgs;
