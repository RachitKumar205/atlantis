-- Drop kind discriminator. Queued generation_bump rows are lost on rollback
-- (worst case: stale tier-2 entries until TTL).

ALTER TABLE atlantis.cache_invalidations
    DROP CONSTRAINT IF EXISTS cache_invalidations_kind_check;

DROP INDEX IF EXISTS atlantis.cache_invalidations_kind_enqueued_idx;

ALTER TABLE atlantis.cache_invalidations
    DROP COLUMN IF EXISTS kind;
