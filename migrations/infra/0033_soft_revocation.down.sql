-- Views first: both read revoked_at, so the column cannot be dropped while
-- they exist.
DROP VIEW IF EXISTS atlantis.active_caller_identities;
DROP VIEW IF EXISTS atlantis.active_callers;

DROP INDEX IF EXISTS atlantis.caller_identities_revoked_idx;

-- Any caller revoked while this migration was applied becomes live again on the
-- way down, because the column recording the revocation is what is being
-- removed. Down is not a rollback of intent here, and an operator going back
-- past this point has to re-cut off whatever they had revoked — the old
-- mechanism for that is RevokeCaller deleting the row.
ALTER TABLE atlantis.caller_identities
    DROP COLUMN IF EXISTS revoked_at;
