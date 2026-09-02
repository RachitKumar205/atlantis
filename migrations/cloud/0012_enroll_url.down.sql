ALTER TABLE cloud.orgs
    DROP CONSTRAINT IF EXISTS orgs_enroll_url_shape;

ALTER TABLE cloud.orgs
    DROP COLUMN IF EXISTS enroll_url;
