DROP INDEX IF EXISTS cloud.org_provisioning_console_rotate_idx;

-- An outstanding request is dropped on the way down rather than honoured. The
-- provisioner on the other side of this migration has no column to read it
-- from, so the alternative is a request nothing will ever act on — and an
-- operator who asked for a rotation should re-ask rather than assume one is
-- still queued.
ALTER TABLE cloud.org_provisioning
    DROP COLUMN IF EXISTS console_rotate_requested_at;
