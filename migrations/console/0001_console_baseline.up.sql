-- Baseline: the console schema as it stood before it had migrations.
--
-- Every statement is IF NOT EXISTS, and deliberately so. Until now the console
-- built its schema by re-running one idempotent block on every boot, so a
-- database that has been running already carries all of this. This migration
-- has to converge that database as well as build a fresh one — on the former
-- every statement is a no-op and golang-migrate simply records version 1, with
-- no operator having to run `migrate force`.
--
-- Reproduce changes here only by adding a NEW numbered migration. Editing this
-- file changes what a fresh install gets without changing what an existing one
-- has, which is how two installs of the same version come to differ.

CREATE SCHEMA IF NOT EXISTS console;

CREATE TABLE IF NOT EXISTS console.users (
    id            BIGSERIAL PRIMARY KEY,
    email         TEXT        NOT NULL UNIQUE,
    password_hash TEXT        NOT NULL,
    role          TEXT        NOT NULL DEFAULT 'admin',
    first_name    TEXT        NOT NULL DEFAULT '',
    last_name     TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- sudo_until: NULL outside sudo mode. Set by /api/auth/sudo when the user
-- re-authenticates with their password; checked by the requireSudo middleware
-- on destructive endpoints. Grants a short window of elevated permission like
-- sudo on a shell, so a stolen session cookie cannot trigger sign-out-all or
-- revoke-all without also producing the password.
CREATE TABLE IF NOT EXISTS console.sessions (
    token      TEXT        PRIMARY KEY,
    user_id    BIGINT      NOT NULL REFERENCES console.users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sudo_until TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS console_sessions_user_id_idx
    ON console.sessions(user_id);
CREATE INDEX IF NOT EXISTS console_sessions_expires_idx
    ON console.sessions(expires_at);

-- audit_log is range-partitioned on created_at, one partition per calendar
-- month, so the retention worker can DROP whole months atomically (no
-- DELETE-induced bloat). The PK includes created_at because Postgres requires
-- every unique constraint on a partitioned table to cover the partition key.
--
-- No FK to console.users: Postgres does not permit one from a partitioned table
-- here, so referential integrity is enforced at the application layer instead.
-- users.id is BIGSERIAL and never reused, so a stale user_id in audit history
-- just shows as user_email='' in the listing.
--
-- The partitions themselves are NOT created here. They are per-month and
-- created at runtime by store.ensureAuditPartition, which makes the current and
-- next month on every boot — a static migration cannot create next March's.
CREATE TABLE IF NOT EXISTS console.audit_log (
    id         BIGSERIAL,
    user_id    BIGINT      NOT NULL,
    action     TEXT        NOT NULL,
    detail     JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE INDEX IF NOT EXISTS console_audit_log_user_id_idx
    ON console.audit_log(user_id);
CREATE INDEX IF NOT EXISTS console_audit_log_created_at_idx
    ON console.audit_log(created_at DESC);
