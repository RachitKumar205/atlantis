-- Where an organisation's machines enrol.
--
-- The poll response of a CLI grant carries this address, so `tide login`
-- reaches the right enrolment listener with no configuration. Read from a
-- column for the same reason /authorize reads console_url from one: the
-- request then carries no URL, and there is nothing to validate.
--
-- It is also the audience of a cli-enroll assertion, which the enrolment
-- listener requires to equal its own CONSOLE_ENROLL_PUBLIC_URL. One string for
-- the audience and the delivery address, as console_url is for sessions.
--
-- Empty string rather than NULL, so "not configured" is one value. The poll
-- refuses to mint against an organisation with an empty one.
ALTER TABLE cloud.orgs
    ADD COLUMN IF NOT EXISTS enroll_url TEXT NOT NULL DEFAULT '';

ALTER TABLE cloud.orgs
    ADD CONSTRAINT orgs_enroll_url_shape
    CHECK (enroll_url = '' OR enroll_url ~ '^https?://');
