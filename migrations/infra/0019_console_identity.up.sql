-- Make the console a registered identity instead of an exempt one.
--
-- Until now 'atlantis-console' worked without a caller_identities row. It was
-- reachable because /atlantis.admin.v1.Admin/ was exempt from the caller
-- allowlist, and it was authorized because ATL_OPERATOR_ALLOWED_CALLERS
-- defaulted to a wildcard. Both of those are being removed: the admin prefix is
-- now governed by the capability interceptor, and authority comes from
-- caller_capabilities. With no row here, a fresh install would have no identity
-- able to reach any admin RPC — including RegisterCaller, the RPC that creates
-- the first identity. The install would be unrecoverable over gRPC.
--
-- 0018 granted the console OPERATOR and LOGS_READ, but only WHERE the row
-- already existed, so it covered upgrades from a deployment where an operator
-- had registered the console by hand and did nothing for a fresh one. This
-- makes the row unconditional.
--
-- 'atlantis-console' is a reserved CN: RegisterCaller refuses it and the signer
-- will not issue it to anyone else, so seeding it cannot collide with a
-- customer caller. cert_fingerprint stays NULL, which the cert-binding
-- interceptor treats as the bootstrap window — any CA-signed cert presenting
-- this CN is accepted until the first console-issued cert binds a fingerprint.
-- That is the same posture the exempt list gave it, now recorded as data.
--
-- can_mutate stays false. The console does not apply schema on its own behalf;
-- it acts on other callers' schema, which is what OPERATOR expresses. Setting
-- it true would hand the console a namespace it never writes to.
INSERT INTO atlantis.caller_identities (caller, can_mutate, created_by)
VALUES ('atlantis-console', false, '<migration:0019>')
ON CONFLICT (caller) DO NOTHING;

-- Operator authority, plus the log ring the console's health page tails.
-- LOGS_READ is separate from SCHEMA_READ because the ring carries pgx error
-- text, and pgx renders a unique violation as
-- `DETAIL: Key (email)=(alice@example.com)` — granting a schema reader access
-- to it has to be a deliberate act.
INSERT INTO atlantis.caller_capabilities (caller, capability, granted_by)
VALUES
    ('atlantis-console', 'CAPABILITY_OPERATOR',  '<migration:0019>'),
    ('atlantis-console', 'CAPABILITY_LOGS_READ', '<migration:0019>')
ON CONFLICT DO NOTHING;

-- Everything else the console's routes actually invoke.
--
-- This list is not "the read bundle" — it is derived from the RPCs
-- internal/console calls, and a test in internal/server/authz recomputes it
-- from the console's source against the proto's declarations, so a new console
-- feature calling a new RPC fails the build until the grant is added here.
-- Deriving it matters because the failure mode is quiet: a missing grant does
-- not stop the server, it just makes one page return PermissionDenied for
-- whoever opens it.
--
-- SCHEMA_PLAN covers the schema editor's preview and PreviewRollback, both
-- pure reads that compute a diff without writing. JOBS_WRITE covers the dead-
-- job retry button. Note the console holds neither SCHEMA_APPLY nor the
-- ability to apply on another caller's behalf: bindCallerIdentity requires
-- req.caller to equal the connecting CN, so the console can preview a change
-- and roll one back, but a human still applies through their own caller.
INSERT INTO atlantis.caller_capabilities (caller, capability, granted_by)
VALUES
    ('atlantis-console', 'CAPABILITY_SCHEMA_READ',  '<migration:0019>'),
    ('atlantis-console', 'CAPABILITY_SCHEMA_PLAN',  '<migration:0019>'),
    ('atlantis-console', 'CAPABILITY_JOBS_READ',    '<migration:0019>'),
    ('atlantis-console', 'CAPABILITY_JOBS_WRITE',   '<migration:0019>'),
    ('atlantis-console', 'CAPABILITY_WORKERS_READ', '<migration:0019>')
ON CONFLICT DO NOTHING;
