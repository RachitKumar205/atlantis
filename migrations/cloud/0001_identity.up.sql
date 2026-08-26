-- Atlantis Cloud's own schema: who people are, and which organisations they
-- belong to.
--
-- Until now Cloud could sign an assertion for any organisation string handed to
-- `cloud mint`, because nothing recorded that organisations or memberships
-- exist. The console has been trusting an `org` claim that nothing wrote. This
-- is the record behind it.
--
-- A third migration tree beside infra/ and console/, each with its own history
-- table, because Cloud is deployed, upgraded and rolled back separately from
-- both and is the only writer of these tables.
--
-- It has its own DSN (CLOUD_PG_URL), which in development points at the same
-- PostgreSQL instance as the console. Nothing here joins across the boundary,
-- so separating them later is a connection-string change.

CREATE SCHEMA IF NOT EXISTS cloud;

-- None of these four tables carries a row-level boundary. The console polices
-- its tables with a transaction-local discriminator and a RESTRICTIVE policy
-- (migrations/console/0004); the same shape with the user in place of the
-- organisation does not work here:
--
--   users       The sign-in lookup is what *discovers* who the request is, so
--               it cannot be filtered by who the request is. Authentication is
--               the act of finding that out; there is nothing to bind until it
--               has happened.
--
--   identities  The same, one step along. An OAuth callback arrives carrying
--               `github:12345` and nobody signed in, and the whole point of the
--               query is to learn which account that is. Under a policy keyed
--               to the current user it would match nothing — and the caller
--               would read that as "no such link" and create a second account
--               for the same person, on every single sign-in. A boundary whose
--               failure mode is silent duplication is worse than none.
--
--   orgs        A registry. A list of organisations readable only by an
--               organisation would be useless, and it holds nothing that is not
--               already in every assertion.
--
--   memberships Read both ways: a user lists their own organisations, and an
--               organisation's admin lists its members. A policy on user_id
--               serves the first and breaks the second, and expressing "or an
--               admin of this org" needs a subquery against the same table,
--               which RLS resolves by recursing.
--
-- So the boundary arrives with the tables that can carry it — the second-factor
-- secrets in 0002, which are only ever read for an already-identified user.
-- What exists from today is the *guard*: internal/cloud/store/policyguard.go
-- refuses to start if any table in this schema is neither policed nor named in
-- its exemption list. That is what stops 0002 adding a TOTP secret without
-- anyone deciding, which is the failure this is actually shaped against.
--
-- The guard also checks the connecting role, but only once a policed table
-- exists — a role that bypasses row-level security bypasses nothing while every
-- table is exempt, so the check arms itself at exactly the point it starts
-- mattering.

-- Users.
--
-- password_hash is NULLABLE. An
-- account created by signing in with GitHub has no password and may never gain
-- one. What it may NOT do is skip the second factor — see 0002; Cloud cannot
-- verify that a provider's MFA happened, so it keeps its own regardless of how
-- the first factor was presented.
--
-- Email is stored lowercased, and the CHECK is what makes that true rather than
-- intended. Case-insensitive uniqueness enforced by normalising in Go is
-- uniqueness enforced by remembering: one INSERT that skips the helper and
-- `Foo@example.com` becomes a second account for the same person, with its own
-- memberships and its own password. The constraint moves that from a bug to a
-- refused write.
CREATE TABLE IF NOT EXISTS cloud.users (
    id                TEXT PRIMARY KEY,
    email             TEXT NOT NULL UNIQUE CHECK (email = lower(email) AND email <> ''),
    name              TEXT NOT NULL DEFAULT '',
    password_hash     TEXT,
    email_verified_at TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Organisations.
--
-- The name is constrained here because of everywhere it travels. It is the RLS
-- discriminator in the console's audit log, the associated data the console's
-- keyring seals each organisation's private key against, a path segment in
-- `tide login`'s ~/.atlantis/<org>/, and half of a URL parameter on Cloud's
-- /authorize. A name that is valid in one of those and not another produces a
-- failure a long way from its cause.
--
-- Lowercase, alphanumeric and hyphens, not starting with a hyphen, 63 chars
-- to match a DNS label — because a per-organisation subdomain is the obvious
-- next thing to want and discovering the limit afterwards means renaming
-- customers.
CREATE TABLE IF NOT EXISTS cloud.orgs (
    name         TEXT PRIMARY KEY
                 CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    display_name TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Memberships.
--
-- This is what the `org` claim has been asserting all along and what nothing
-- wrote. Cloud's /authorize checks it before minting, so it is the gate between
-- a signed-in user and an organisation's console.
--
-- The role CHECK duplicates internal/cloud/identity.Role. Duplication between
-- Go and SQL is normally a smell; here it is deliberate, because the console
-- refuses a role it does not recognise and a row that reached the database
-- would authenticate a user who could then do nothing. A test asserts every
-- identity.Role constant is accepted by this constraint, so the two cannot
-- drift silently.
CREATE TABLE IF NOT EXISTS cloud.memberships (
    user_id    TEXT NOT NULL REFERENCES cloud.users(id) ON DELETE CASCADE,
    org        TEXT NOT NULL REFERENCES cloud.orgs(name) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('admin', 'viewer')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, org)
);

CREATE INDEX IF NOT EXISTS memberships_org ON cloud.memberships (org);

-- OAuth identities.
--
-- One provider account links to exactly one user, which the primary key
-- enforces. Without that, two Cloud accounts could both claim the same GitHub
-- identity and a sign-in would have to pick one.
--
-- provider_email is what the provider asserted at link time, kept for display
-- only. It is deliberately not used for matching: an email a provider reports
-- is not proof of control of that mailbox, and matching on it would let someone
-- who can set an unverified provider email take over an existing account.
CREATE TABLE IF NOT EXISTS cloud.identities (
    provider         TEXT NOT NULL CHECK (provider IN ('github', 'google')),
    provider_subject TEXT NOT NULL,
    user_id          TEXT NOT NULL REFERENCES cloud.users(id) ON DELETE CASCADE,
    provider_email   TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (provider, provider_subject)
);

CREATE INDEX IF NOT EXISTS identities_user ON cloud.identities (user_id);
