-- Reverse of 0003. Restores the shape of the tables, not their contents.
--
-- Be clear about what this can and cannot do. The structure comes back; the
-- accounts do not. console.users held email addresses and bcrypt hashes, and
-- 0003 dropped that table — the hashes are gone, and nothing here can invent
-- them. After a down migration the console has its old schema and no operator
-- who can sign in, so recovering a working install means restoring from a
-- backup taken before 0003 ran.
--
-- It exists so a failed migration can be unwound to a known shape, which is
-- what golang-migrate needs. It is not a rollback plan for a deployment that
-- has been serving traffic.

CREATE TABLE IF NOT EXISTS console.users (
    id            BIGSERIAL PRIMARY KEY,
    email         TEXT        NOT NULL UNIQUE,
    password_hash TEXT        NOT NULL,
    role          TEXT        NOT NULL DEFAULT 'admin',
    first_name    TEXT        NOT NULL DEFAULT '',
    last_name     TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

DROP TABLE IF EXISTS console.spent_assertions;

-- Sessions again reference a local user. None can be reconstructed, so the
-- table is emptied rather than left holding rows with a user_id pointing at a
-- table that has just been recreated empty.
DELETE FROM console.sessions;

DROP INDEX IF EXISTS console.console_sessions_subject_idx;
ALTER TABLE console.sessions DROP COLUMN IF EXISTS subject;
ALTER TABLE console.sessions DROP COLUMN IF EXISTS org;
ALTER TABLE console.sessions DROP COLUMN IF EXISTS role;
ALTER TABLE console.sessions DROP COLUMN IF EXISTS email;
ALTER TABLE console.sessions DROP COLUMN IF EXISTS name;

ALTER TABLE console.sessions
    ADD COLUMN IF NOT EXISTS user_id BIGINT NOT NULL
        REFERENCES console.users(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS console_sessions_user_id_idx
    ON console.sessions(user_id);

-- Audit rows keep whatever actor they carry. The 'local:N' form is parsed back
-- into the integer it came from; a Cloud subject has no integer to become, so
-- those rows land on 0 — visible as an unresolvable actor rather than silently
-- attributed to whichever user happens to hold id 1.
ALTER TABLE console.audit_log ADD COLUMN IF NOT EXISTS user_id BIGINT;

UPDATE console.audit_log
   SET user_id = CASE
       WHEN actor ~ '^local:[0-9]+$' THEN substring(actor from 7)::BIGINT
       ELSE 0
   END
 WHERE user_id IS NULL;

ALTER TABLE console.audit_log ALTER COLUMN user_id SET NOT NULL;

DROP INDEX IF EXISTS console.console_audit_log_actor_idx;
ALTER TABLE console.audit_log DROP COLUMN IF EXISTS actor;
ALTER TABLE console.audit_log DROP COLUMN IF EXISTS actor_email;

CREATE INDEX IF NOT EXISTS console_audit_log_user_id_idx
    ON console.audit_log(user_id);
