DELETE FROM atlantis.caller_capabilities
 WHERE caller = 'atlantis-console'
   AND capability = 'CAPABILITY_SCHEMA_REHEARSE'
   AND granted_by = '<migration:0041>';
ALTER TABLE atlantis.schema_plans
    DROP COLUMN IF EXISTS rehearsal_id,
    DROP COLUMN IF EXISTS verdict;
DROP TABLE IF EXISTS atlantis.rehearsals;
DROP TABLE IF EXISTS atlantis.rehearsal_clones;
DROP TABLE IF EXISTS atlantis.rehearsal_database;
