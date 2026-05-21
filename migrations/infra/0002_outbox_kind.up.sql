-- Add 'kind' discriminator to cache_invalidations. Two kinds:
--   'invalidation'    — per-row body invalidation (entity + row_id + new_version)
--   'generation_bump' — per-entity counter bump; row_id/new_version unused
--                       (set to entity / 0 to keep the table shape).

ALTER TABLE atlantis.cache_invalidations
    ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'invalidation';

-- Index for FOR UPDATE SKIP LOCKED scans within one kind.
CREATE INDEX IF NOT EXISTS cache_invalidations_kind_enqueued_idx
    ON atlantis.cache_invalidations (kind, enqueued_at);

-- Constrain known kinds so unknown values are rejected on insert.
ALTER TABLE atlantis.cache_invalidations
    DROP CONSTRAINT IF EXISTS cache_invalidations_kind_check;
ALTER TABLE atlantis.cache_invalidations
    ADD CONSTRAINT cache_invalidations_kind_check
    CHECK (kind IN ('invalidation', 'generation_bump'));
