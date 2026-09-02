-- Workload federation: which CI identities may obtain which caller's
-- certificate, and for how long the result renews.
--
-- A rule binds an OIDC issuer, the audience its tokens must carry, and a
-- pattern the token's subject must match, to one caller. A GitHub Actions
-- run for repo acme/api presents its id_token; the listener verifies it
-- against the issuer's published keys and matches
-- sub = "repo:acme/api:ref:refs/heads/main" against the pattern.
--
-- The pattern's literal prefix must be non-empty and contain ':' — enforced
-- in code, where the glob is defined — so "*" cannot be written as a rule.
CREATE TABLE IF NOT EXISTS console.federation_rules (
    id              BIGSERIAL   PRIMARY KEY,
    org             TEXT        NOT NULL,
    caller          TEXT        NOT NULL,
    issuer_url      TEXT        NOT NULL CHECK (issuer_url ~ '^https://'),
    audience        TEXT        NOT NULL CHECK (audience <> ''),
    subject_pattern TEXT        NOT NULL CHECK (subject_pattern <> ''),
    -- How many times a certificate issued under this rule may renew. Spent
    -- down through caller_certs.renewals_remaining; see below.
    renewal_budget  INTEGER     NOT NULL DEFAULT 1 CHECK (renewal_budget >= 0),
    created_by      TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS console_federation_rules_caller_idx
    ON console.federation_rules (org, caller) WHERE revoked_at IS NULL;

-- Organisation isolation, the 0007 shape.
ALTER TABLE console.federation_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE console.federation_rules FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS federation_rules_org_isolation ON console.federation_rules;
CREATE POLICY federation_rules_org_isolation ON console.federation_rules
    AS RESTRICTIVE
    USING (org = console.current_org())
    WITH CHECK (org = console.current_org());

DROP POLICY IF EXISTS federation_rules_default_access ON console.federation_rules;
CREATE POLICY federation_rules_default_access ON console.federation_rules
    AS PERMISSIVE USING (true) WITH CHECK (true);

-- How many renewals a certificate has left. NULL is unlimited, which every
-- existing row keeps and every human enrolment records — today's behaviour.
--
-- A certificate issued to a CI workload lives an hour and carries the rule's
-- budget. Renewal copies budget - 1 onto the successor row and refuses at
-- zero, so a certificate stolen off a runner is worth at most the budget's
-- worth of further hours rather than an indefinite chain of renewals.
ALTER TABLE console.caller_certs
    ADD COLUMN IF NOT EXISTS renewals_remaining INTEGER;
