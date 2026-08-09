-- Restores the txid_current() form. This reinstates a transaction ID being
-- consumed on every read of every partitioned entity — see the up migration.
CREATE OR REPLACE FUNCTION atlantis.current_partition()
RETURNS text
LANGUAGE sql
SECURITY DEFINER
STABLE
SET search_path = atlantis, pg_catalog
AS $$
    SELECT tenant
      FROM atlantis.session_partition
     WHERE pid = pg_backend_pid() AND xid = txid_current();
$$;

GRANT EXECUTE ON FUNCTION atlantis.current_partition() TO PUBLIC;
