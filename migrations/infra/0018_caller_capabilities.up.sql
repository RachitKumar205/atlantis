-- caller_capabilities: what each registered caller is allowed to do.
--
-- Replaces can_mutate as the authorization source. That boolean conflated
-- two different powers — "may apply schema to its own namespace" and "may
-- operate on every caller's schema" — and the gate that read it fell back to
-- a global wildcard whenever ATL_OPERATOR_ALLOWED_CALLERS was unset, which
-- was the shipped default. The result was that every caller able to apply was
-- also able to roll back the shared checkpoint for everyone else. A boolean
-- cannot express the difference, so it is replaced rather than extended.
--
-- One row per granted capability rather than an array column or a bitmask,
-- because an authorization record is something you audit: granted_at and
-- granted_by answer "when did this caller become an operator, and who did
-- that" without a separate log to correlate against. An array column answers
-- neither.
--
-- capability holds the proto enum's name ('CAPABILITY_SCHEMA_READ'), not its
-- number. Numbers are a wire detail that a future proto edit could reorder;
-- the name is self-describing when someone reads this table during an
-- incident. Deliberately no CHECK constraint: coupling the schema to the enum
-- would mean a migration every time a capability is added, and that friction
-- is how capability sets stop growing. A value the server does not recognize
-- is ignored — grants fail closed, and the server logs the unrecognized name
-- so a typo surfaces as a warning rather than as silence.
CREATE TABLE IF NOT EXISTS atlantis.caller_capabilities (
    caller      TEXT        NOT NULL
                            REFERENCES atlantis.caller_identities (caller)
                            ON DELETE CASCADE,
    capability  TEXT        NOT NULL,
    granted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    granted_by  TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (caller, capability)
);

-- The interceptor reads every capability for one caller on each cache miss.
CREATE INDEX IF NOT EXISTS caller_capabilities_caller_idx
    ON atlantis.caller_capabilities (caller);

-- Backfill.
--
-- This deliberately does NOT reproduce the access callers previously had.
-- Under the wildcard, a can_mutate caller was effectively an operator; that
-- is the defect being fixed, so preserving it would carry the bug across the
-- migration and leave no moment at which it was ever corrected. Existing
-- callers get what their can_mutate flag actually meant, and nothing more.
--
-- Read-only callers (can_mutate = false) — typically app-server runtime CNs
-- holding a typed client connection.
INSERT INTO atlantis.caller_capabilities (caller, capability, granted_by)
SELECT ci.caller, c.capability, '<migration:0018>'
FROM atlantis.caller_identities ci
CROSS JOIN (VALUES
    ('CAPABILITY_SCHEMA_READ'),
    ('CAPABILITY_JOBS_READ'),
    ('CAPABILITY_WORKERS_READ')
) AS c(capability)
ON CONFLICT DO NOTHING;

-- Mutating callers (can_mutate = true) — typically CI cert CNs. They add the
-- ability to plan, apply, and drive jobs. They do NOT get OPERATOR: rolling
-- back the shared checkpoint, registering or revoking callers, and evicting
-- workers all affect other callers, and no existing row records that anyone
-- was deliberately given that authority.
INSERT INTO atlantis.caller_capabilities (caller, capability, granted_by)
SELECT ci.caller, c.capability, '<migration:0018>'
FROM atlantis.caller_identities ci
CROSS JOIN (VALUES
    ('CAPABILITY_SCHEMA_PLAN'),
    ('CAPABILITY_SCHEMA_APPLY'),
    ('CAPABILITY_JOBS_WRITE')
) AS c(capability)
WHERE ci.can_mutate
ON CONFLICT DO NOTHING;

-- The console is the one identity that legitimately needs operator authority:
-- it is the surface through which a human registers callers, issues certs,
-- and rolls schema back. 'atlantis-console' is not a guess — it is a reserved
-- CN the signer refuses to issue to anyone else, so granting it here cannot
-- collide with a customer caller.
--
-- LOGS_READ is granted alongside because the console's health page tails the
-- server's log ring. It is a separate capability precisely so that granting a
-- schema reader access to logs has to be a deliberate act; the console is the
-- deliberate act.
--
-- An operator running the console under a different CN must grant these
-- explicitly. That is intended: this migration is the point at which operator
-- authority stops being implicit.
INSERT INTO atlantis.caller_capabilities (caller, capability, granted_by)
SELECT ci.caller, c.capability, '<migration:0018>'
FROM atlantis.caller_identities ci
CROSS JOIN (VALUES
    ('CAPABILITY_OPERATOR'),
    ('CAPABILITY_LOGS_READ')
) AS c(capability)
WHERE ci.caller = 'atlantis-console'
ON CONFLICT DO NOTHING;
