DELETE FROM atlantis.caller_capabilities
 WHERE caller = 'atlantis-console'
   AND capability = 'CAPABILITY_SCHEMA_APPLY'
   AND granted_by = '<migration:0034>';
