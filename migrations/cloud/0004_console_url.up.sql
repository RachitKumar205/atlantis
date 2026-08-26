-- Where an organisation's console lives.
--
-- /authorize reads the destination from this column rather than taking it as a
-- parameter, so the request carries no URL and there is nothing to validate.
-- The alternative, /authorize?console=<url> checked against an allowlist, is an
-- open redirect behind a guard that can be written wrongly: a prefix match
-- admitting https://acme.example.evil.test, a comparison that omits the scheme.
--
-- This is also the assertion's `aud`, which the console requires to equal its
-- own CLOUD_AUDIENCE. Minting with aud = console_url makes the token's audience
-- and its delivery address one string, so an assertion cannot be delivered
-- anywhere it would not verify.
--
-- A column, not a table: one organisation has one console, and a table would
-- require deciding which one a bare /authorize?org=acme means.
--
-- Empty string rather than NULL, so "not configured" is one value.
-- /authorize refuses an empty string; `cloud org register` sets it.
ALTER TABLE cloud.orgs
    ADD COLUMN IF NOT EXISTS console_url TEXT NOT NULL DEFAULT '';

-- Absolute, or empty. A relative URL in a Location header resolves against
-- Cloud's own origin, which would send a browser carrying a fresh assertion to
-- a Cloud path rather than to the console — a failure that looks like a broken
-- console rather than like a bad row.
--
-- The check is deliberately shallow: scheme and something after it. Postgres is
-- the wrong place to parse a URL, and Go parses it again at registration time.
-- What this stops is the class the database can see — a bare hostname, a path,
-- an empty scheme — landing in a column whose only consumer is a redirect.
ALTER TABLE cloud.orgs
    ADD CONSTRAINT orgs_console_url_absolute
    CHECK (console_url = '' OR console_url ~ '^https?://[^/]+');
