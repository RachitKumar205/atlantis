-- The console holds CAPABILITY_SCHEMA_APPROVE and must not also hold
-- CAPABILITY_SCHEMA_APPLY: an identity holding both is a caller that approves
-- its own changes (capability.proto documents the separation).
--
-- No console code calls ApplyMigration. The import flow applies through
-- AdoptBaseline, which rides the console's CAPABILITY_OPERATOR from 0019.

DELETE FROM atlantis.caller_capabilities
 WHERE caller = 'atlantis-console'
   AND capability = 'CAPABILITY_SCHEMA_APPLY'
   AND granted_by = '<migration:0034>';
