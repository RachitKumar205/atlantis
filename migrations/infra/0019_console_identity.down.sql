-- Remove the seeded console identity, but only if it is still the seed.
--
-- created_by is the discriminator: '<migration:0019>' means nothing has touched
-- the row since this migration wrote it. If an operator has since issued the
-- console a cert, the row carries a cert_fingerprint and deleting it would
-- revoke that cert — a rollback of an authorization migration must not lock the
-- operator out of the console they would use to fix things.
--
-- The two deletes therefore share one condition rather than running
-- independently. Removing the grants while the identity survives would leave
-- the console able to authenticate and authorized for nothing, which is the
-- same lockout by a different route: it would reach the login page and be
-- refused by every action behind it.
DELETE FROM atlantis.caller_capabilities
 WHERE caller = 'atlantis-console'
   AND granted_by = '<migration:0019>'
   AND EXISTS (
       SELECT 1 FROM atlantis.caller_identities ci
        WHERE ci.caller = 'atlantis-console'
          AND ci.created_by = '<migration:0019>'
          AND ci.cert_fingerprint IS NULL
   );

DELETE FROM atlantis.caller_identities
 WHERE caller = 'atlantis-console'
   AND created_by = '<migration:0019>'
   AND cert_fingerprint IS NULL;
