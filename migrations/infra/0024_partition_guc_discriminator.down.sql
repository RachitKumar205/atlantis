-- Restores the table discriminator, and with it the transaction id consumed by
-- every bind. See the up migration for the measurements.
--
-- Recreates the table because the up migration dropped it. Its contents were
-- transient per-backend rows, so nothing is lost by the round trip: any
-- transaction in flight across this migration has to rebind anyway.

CREATE TABLE IF NOT EXISTS atlantis.session_partition (
    pid     integer PRIMARY KEY,
    xid     bigint  NOT NULL,
    tenant  text    NOT NULL,
    set_at  timestamptz NOT NULL DEFAULT now()
);

REVOKE ALL ON atlantis.session_partition FROM PUBLIC;

CREATE OR REPLACE FUNCTION atlantis.set_partition(p_tenant text)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = atlantis, pg_catalog
AS $$
DECLARE
    existing text;
BEGIN
    IF p_tenant IS NULL OR p_tenant = '' THEN
        RAISE EXCEPTION 'atlantis: partition must be a non-empty value';
    END IF;

    SELECT tenant INTO existing
      FROM atlantis.session_partition
     WHERE pid = pg_backend_pid() AND xid = txid_current();

    IF existing IS NOT NULL THEN
        RAISE EXCEPTION 'atlantis: partition already set for this transaction';
    END IF;

    DELETE FROM atlantis.session_partition WHERE pid = pg_backend_pid();
    INSERT INTO atlantis.session_partition (pid, xid, tenant)
    VALUES (pg_backend_pid(), txid_current(), p_tenant);
END;
$$;

GRANT EXECUTE ON FUNCTION atlantis.set_partition(text) TO PUBLIC;

-- The 0023 form: txid_current_if_assigned() rather than txid_current(), so the
-- read path does not assign an id. Reverting past this file and then past 0023
-- reinstates the read-path burn as well.
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

GRANT EXECUTE ON FUNCTION atlantis.current_partition() TO PUBLIC;
