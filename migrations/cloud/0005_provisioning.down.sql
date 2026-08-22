-- Reverse of 0005.
--
-- Dropping org_provisioning loses which organisations were queued, in flight or
-- failed, and every attempt count and error with them. What it does not lose is
-- anything provisioned: the Kubernetes objects and the console.orgs rows are
-- elsewhere, so an organisation that reached `ready` keeps working. Re-applying
-- 0005 starts every organisation's queue state empty.
--
-- Dropping audit_log destroys the record itself, which nothing else holds.

DROP TABLE IF EXISTS cloud.audit_log;
DROP TABLE IF EXISTS cloud.org_provisioning;
