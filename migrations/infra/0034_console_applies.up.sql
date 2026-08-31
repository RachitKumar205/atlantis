-- The console applies the schema it plans.
--
-- 0019 withheld CAPABILITY_SCHEMA_APPLY so a human applied through their own
-- caller. That followed from the console being a window onto state rather than
-- a way to change it; the platform is its own source of truth now, and
-- onboarding an imported database has to reach a working atlantis without a
-- repository round-trip first.
--
-- What this does not open. ApplyMigration evaluates the change policy before
-- it runs anything (internal/server/admin/approval.go): a class carrying
-- require_approval is recorded as pending and refused, whoever submitted it.
-- Only classes the policy leaves ungated — PLAN_CLASS_ADDITIVE by default —
-- apply without a human, and an import of a database atlantis does not yet
-- have is additive.
--
-- The console holds CAPABILITY_SCHEMA_APPROVE, so it now holds both halves of
-- a gated change. Approving through the console is guarded elsewhere: the
-- route requires an admin role and a re-authentication, so the identity in the
-- capability table is the console and the identity that decides is a person.

INSERT INTO atlantis.caller_capabilities (caller, capability, granted_by)
VALUES ('atlantis-console', 'CAPABILITY_SCHEMA_APPLY', '<migration:0034>')
ON CONFLICT DO NOTHING;
