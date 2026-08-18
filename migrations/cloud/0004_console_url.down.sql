ALTER TABLE cloud.orgs DROP CONSTRAINT IF EXISTS orgs_console_url_absolute;
ALTER TABLE cloud.orgs DROP COLUMN IF EXISTS console_url;
