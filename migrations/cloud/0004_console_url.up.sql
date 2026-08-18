-- Where an organisation's console lives.
--
-- Until now nothing in the product recorded this. The console knows how to
-- reach its own atlantis (console.orgs.atl_endpoint), and Cloud knows which
-- organisations exist, but no row anywhere said "acme's console is at
-- https://acme.example". `cloud mint` sidestepped the question by taking the
-- audience as a flag, which works for an operator at a terminal and not for a
-- browser being sent somewhere.
--
-- ── Why this is what /authorize redirects to, and not a parameter ────────────
--
-- The obvious design is /authorize?console=<url>&org=<name>, with the URL
-- checked against a list of permitted ones. That is an open redirect with a
-- guard in front of it, and the guard is a thing that can be got wrong: a
-- prefix match that admits https://acme.example.evil.test, a comparison that
-- forgets the scheme, a later refactor that relaxes it for a staging host.
--
-- With the destination stored per organisation, the request carries no
-- destination at all. /authorize?org=acme looks the value up. There is nothing
-- to validate because there is nothing to choose, which is a stronger position
-- than any validation.
--
-- ── One value, two jobs ─────────────────────────────────────────────────────
--
-- This is also the assertion's `aud`. The console already requires `aud` to
-- equal its own CLOUD_AUDIENCE, which is documented as naming that console and
-- is its origin by convention. Minting with aud = console_url means the token's
-- audience and its delivery address are one string rather than two that an
-- operator keeps in step by hand. An assertion therefore cannot be delivered
-- anywhere it would not verify, and the mismatch that would otherwise present
-- as "sign-in silently fails for one organisation" cannot be expressed.
--
-- ── Why a column and not a table ────────────────────────────────────────────
--
-- One organisation, one console. A table would let an organisation have two,
-- which nothing wants yet and which would require deciding which one a bare
-- /authorize?org=acme means. When that changes — a regional split, a migration
-- between deployments — this becomes a table and this column moves into it.
-- Recorded here so that is a deliberate migration rather than a surprise.
--
-- Empty rather than NULL and NOT NULL, so the "not configured yet" state is one
-- value rather than two. /authorize refuses an empty string; `cloud org
-- register` is what sets it.
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
