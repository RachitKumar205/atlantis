INSERT INTO atlantis.caller_capabilities (caller, capability, granted_by)
VALUES ('atlantis-console', 'CAPABILITY_SCHEMA_APPLY', '<migration:0034>')
ON CONFLICT DO NOTHING;
