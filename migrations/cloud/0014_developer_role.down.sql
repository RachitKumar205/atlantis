-- Developers become viewers: the narrower CHECK refuses the value, and a
-- membership that vanishes on downgrade is a lockout rather than a demotion.
UPDATE cloud.memberships SET role = 'viewer' WHERE role = 'developer';

ALTER TABLE cloud.memberships
    DROP CONSTRAINT memberships_role_check;
ALTER TABLE cloud.memberships
    ADD CONSTRAINT memberships_role_check
    CHECK (role IN ('admin', 'viewer'));
