-- Grants for the two control-plane roles. Run once against the `atlantis`
-- database as the CNPG superuser, after the cluster reports healthy:
--
--   kubectl -n atlantis-system exec -i control-1 -c postgres -- \
--     psql -U postgres -d atlantis -v ON_ERROR_STOP=1 < deploy/gcp/control-roles.sql
--
-- The roles themselves are declared on the Cluster (managed.roles); this file
-- carries what a role declaration cannot: CREATE on the database, because
-- each service runs its own migrations and must own the schema it creates.
-- Every statement is idempotent.
GRANT CONNECT, CREATE ON DATABASE atlantis TO atlantis_cloud;
GRANT USAGE, CREATE ON SCHEMA public TO atlantis_cloud;
GRANT CONNECT, CREATE ON DATABASE atlantis TO atlantis_console;
GRANT USAGE, CREATE ON SCHEMA public TO atlantis_console;
