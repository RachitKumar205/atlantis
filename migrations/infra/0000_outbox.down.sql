DROP TRIGGER IF EXISTS cache_invalidations_notify ON atlantis.cache_invalidations;
DROP FUNCTION IF EXISTS atlantis.notify_cache_invalidation();
DROP INDEX IF EXISTS atlantis.cache_invalidations_attempts_idx;
DROP INDEX IF EXISTS atlantis.cache_invalidations_enqueued_idx;
DROP TABLE IF EXISTS atlantis.cache_invalidations;
-- Leave SCHEMA atlantis; other migrations own it.
