-- caller_identities: registered caller cert-CNs.
--
-- Today an identity comes into being implicitly on first `tide apply`
-- (a row appears in caller_registrations). That's fine for the schema
-- model but leaves two gaps:
--   1. The console can't onboard a caller before their first apply,
--      so cert issuance has no "registered caller" to bind to.
--   2. The signer has no way to check "is this CN a real caller?" —
--      anyone with operator access could mint a cert for any name.
--
-- This table is the canonical list of permitted CNs. The signer
-- consults it before issuing; the apply path consults it (via the
-- atlantis admin layer) to decide whether a caller may mutate schema
-- WITHOUT requiring a static env-var allowlist + restart.
--
-- can_mutate distinguishes "registered, can apply schema" (typically
-- CI cert CNs) from "registered, read-only" (typically app-server
-- runtime CNs that only need a typed client connection).
--
-- The legacy ATL_MUTATION_ALLOWED_CALLERS env var continues to work
-- alongside this table — the two are UNIONed at the gate.
--
-- cert_fingerprint binds the row to a single active leaf cert (SHA-256
-- of the DER). Every authenticated RPC verifies that the presented
-- cert's fingerprint matches; mismatch is rejected as Unauthenticated.
-- NULL means "no fingerprint recorded yet" — back-compat for callers
-- whose certs were issued before this column existed; the first
-- console-driven re-issue closes that gap. Combined with the existing
-- DELETE-on-revoke, this gives same-process certificate revocation
-- without standing up a CRL or OCSP responder.
CREATE TABLE IF NOT EXISTS atlantis.caller_identities (
    caller            TEXT        PRIMARY KEY,
    can_mutate        BOOLEAN     NOT NULL DEFAULT false,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by        TEXT        NOT NULL DEFAULT '',  -- operator email; empty for legacy implicit registrations
    cert_fingerprint  BYTEA  -- SHA-256(cert.Raw); NULL until first console-issued or re-issued cert
);

-- Backfill from caller_registrations with can_mutate=true so every
-- caller already applying schema before this migration keeps doing so
-- without an operator step. NEW callers added after this migration must
-- be inserted with can_mutate explicitly set — this backfill widens the
-- mutation allowlist exactly once, at the moment the table is created.
INSERT INTO atlantis.caller_identities (caller, can_mutate, created_by)
SELECT DISTINCT caller, true, '<implicit:backfill>'
FROM atlantis.caller_registrations
ON CONFLICT (caller) DO NOTHING;
