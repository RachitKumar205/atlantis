-- Tie the row-level-security tables to the organisation registry.
--
-- Housekeeping across a RESTRICTIVE policy has to run bound to an organisation:
-- an unbound `DELETE FROM console.enroll_tokens WHERE expires_at < now()` binds
-- nothing, current_org() is NULL, the policy admits no row to the scan the
-- WHERE clause runs, and the statement reports 0 rows and no error. So the
-- sweeps now visit each organisation in console.orgs in turn.
--
-- That is only complete if no row can name an organisation the registry does
-- not hold. These foreign keys are what make it so, and the cascade removes an
-- organisation's rows when its registration goes — referential integrity
-- actions bypass row security, so the cascade reaches rows the deleting session
-- could not select.
--
-- console.orgs needs only its primary key; every column added since 0005 is
-- nullable, so a registry row is cheap and there is no shape a caller has to
-- fill in to satisfy this.

-- Rows naming an unregistered organisation, removed before the constraint.
--
-- Unreachable, whatever wrote them: a session binds the organisation from a
-- Cloud assertion, and an organisation the console cannot describe has nothing
-- to serve. The sweeps could never see them either.
DELETE FROM console.enroll_tokens t
 WHERE NOT EXISTS (SELECT 1 FROM console.orgs o WHERE o.org = t.org);

DELETE FROM console.schema_imports i
 WHERE NOT EXISTS (SELECT 1 FROM console.orgs o WHERE o.org = i.org);

-- Drives the cascade. console.schema_imports already has (org, created_at DESC)
-- from 0010, which serves the same purpose.
CREATE INDEX IF NOT EXISTS console_enroll_tokens_org_idx
    ON console.enroll_tokens (org);

ALTER TABLE console.enroll_tokens
    DROP CONSTRAINT IF EXISTS enroll_tokens_org_fkey;
ALTER TABLE console.enroll_tokens
    ADD CONSTRAINT enroll_tokens_org_fkey
    FOREIGN KEY (org) REFERENCES console.orgs(org) ON DELETE CASCADE;

ALTER TABLE console.schema_imports
    DROP CONSTRAINT IF EXISTS schema_imports_org_fkey;
ALTER TABLE console.schema_imports
    ADD CONSTRAINT schema_imports_org_fkey
    FOREIGN KEY (org) REFERENCES console.orgs(org) ON DELETE CASCADE;
