DROP INDEX IF EXISTS cloud.org_provisioning_image_roll_idx;

ALTER TABLE cloud.org_provisioning
    DROP CONSTRAINT IF EXISTS org_provisioning_image_roll_complete;

-- An outstanding request is dropped on the way down, as with
-- console_rotate_requested_at: the provisioner on the other side of this
-- migration has no column to read it from, so keeping it would leave a request
-- nothing will act on.
ALTER TABLE cloud.org_provisioning
    DROP COLUMN IF EXISTS image_roll_requested_at,
    DROP COLUMN IF EXISTS image_roll_kinds,
    DROP COLUMN IF EXISTS image_roll_attempts;
