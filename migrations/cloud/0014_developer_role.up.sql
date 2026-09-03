-- The developer role: between viewer and admin. The CHECK duplicates
-- internal/cloud/identity.Role on purpose — the constants say what the
-- application refuses, this says what the database refuses, and the two must
-- name the same set.

ALTER TABLE cloud.memberships
    DROP CONSTRAINT memberships_role_check;
ALTER TABLE cloud.memberships
    ADD CONSTRAINT memberships_role_check
    CHECK (role IN ('admin', 'developer', 'viewer'));
