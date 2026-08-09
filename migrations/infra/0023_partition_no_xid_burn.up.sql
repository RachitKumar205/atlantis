-- Stop the tenant discriminator from consuming a transaction ID on every read.
--
-- atlantis.current_partition() is called by every row-level security policy, so
-- it runs on every read of every partitioned entity. It scoped the discriminator
-- with txid_current(), and txid_current() has a side effect: it ASSIGNS a
-- transaction ID if the transaction does not already have one.
--
-- PostgreSQL deliberately avoids assigning transaction IDs to read-only
-- transactions, because the 32-bit ID space is a consumable resource — exhaust
-- it and the database stops accepting writes until an anti-wraparound vacuum
-- completes. Calling txid_current() from a read path opts out of that
-- protection and makes read volume drive wraparound pressure one-for-one.
--
-- Measured on PostgreSQL 17.8, as a non-superuser with the policy in place:
--
--     100 SELECTs against an RLS table     -> 100 transaction IDs consumed
--     100 SELECTs against a non-RLS table  ->   0 transaction IDs consumed
--
-- At ten thousand reads per second that is the entire 2^32 space in about five
-- days. The failure it leads to is not a slow query; it is a database that
-- refuses writes.
--
-- The fix is exact rather than a trade-off, because of what the discriminator
-- is. atlantis.set_partition() INSERTs a row, and any write assigns a
-- transaction ID. So a transaction with no ID assigned is, necessarily, a
-- transaction in which set_partition() was never called — and the correct
-- answer for it is NULL, which is precisely what the unmatched lookup already
-- returns. Asking whether an ID is assigned, instead of demanding one, gives
-- the same answer for every input and stops the side effect.
--
-- txid_current_if_assigned() rather than pg_current_xact_id_if_assigned():
-- both exist from PostgreSQL 13, and the txid_* form returns bigint, matching
-- the column type this migration was built with. The comparison is against
-- NULL when no ID is assigned, so no row matches and the policy fails closed —
-- the same behaviour as before, reached without the write.

CREATE OR REPLACE FUNCTION atlantis.current_partition()
RETURNS text
LANGUAGE sql
SECURITY DEFINER
STABLE
SET search_path = atlantis, pg_catalog
AS $$
    SELECT tenant
      FROM atlantis.session_partition
     WHERE pid = pg_backend_pid()
       AND xid = txid_current_if_assigned();
$$;

-- set_partition keeps txid_current(). It writes, so it assigns an ID either
-- way, and it needs a concrete value to store.

GRANT EXECUTE ON FUNCTION atlantis.current_partition() TO PUBLIC;
