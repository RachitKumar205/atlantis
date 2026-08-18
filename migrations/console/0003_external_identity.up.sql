-- Identity moves to Atlantis Cloud.
--
-- The console stops holding accounts. It has no password hashes, no user rows
-- and no way to create either; a browser arrives with a signed assertion from
-- Cloud and the console verifies it. What a session used to look up in
-- console.users it now carries on the session row itself.

-- audit_log first, while console.users still exists to be read.
--
-- Rows recorded before this migration reference a user by id. Dropping the
-- table under them would leave every historical entry pointing at nothing, and
-- an audit trail that cannot say who acted is not an audit trail. So the email
-- is copied onto each row, and the id is preserved in a form that is visibly
-- not a Cloud subject.
--
-- 'local:' is the marker. Nobody should later mistake one of these for an
-- identity Cloud can resolve, and a bare integer would invite exactly that.
ALTER TABLE console.audit_log ADD COLUMN IF NOT EXISTS actor       TEXT;
ALTER TABLE console.audit_log ADD COLUMN IF NOT EXISTS actor_email TEXT;

UPDATE console.audit_log al
   SET actor       = 'local:' || al.user_id::TEXT,
       actor_email = COALESCE(u.email, '')
  FROM console.users u
 WHERE u.id = al.user_id
   AND al.actor IS NULL;

-- Rows whose user was already deleted: the id is all that survives, and it is
-- still worth more than NULL.
UPDATE console.audit_log
   SET actor       = 'local:' || user_id::TEXT,
       actor_email = ''
 WHERE actor IS NULL;

ALTER TABLE console.audit_log ALTER COLUMN actor       SET NOT NULL;
ALTER TABLE console.audit_log ALTER COLUMN actor_email SET NOT NULL;
ALTER TABLE console.audit_log ALTER COLUMN actor_email SET DEFAULT '';

DROP INDEX IF EXISTS console.console_audit_log_user_id_idx;
ALTER TABLE console.audit_log DROP COLUMN IF EXISTS user_id;
CREATE INDEX IF NOT EXISTS console_audit_log_actor_idx
    ON console.audit_log(actor);

-- Sessions cannot be carried across.
--
-- A session row means "this cookie belongs to console.users row N", and there
-- is no mapping from a local row to a Cloud subject — the console never knew
-- one. Anything kept would be a session whose identity fields were invented
-- here. Everyone signs in again, once.
DELETE FROM console.sessions;

ALTER TABLE console.sessions DROP CONSTRAINT IF EXISTS sessions_user_id_fkey;
DROP INDEX IF EXISTS console.console_sessions_user_id_idx;
ALTER TABLE console.sessions DROP COLUMN IF EXISTS user_id;

-- The verified claims, denormalised onto the session.
--
-- Denormalised rather than pointing at a local row, because there is no local
-- row any more: Cloud holds the user record. The session is a snapshot of what
-- Cloud asserted at sign-in, and it stays fixed for the session's life — a
-- role change at Cloud takes effect at the next sign-in, not mid-session.
ALTER TABLE console.sessions ADD COLUMN IF NOT EXISTS subject TEXT NOT NULL DEFAULT '';
ALTER TABLE console.sessions ADD COLUMN IF NOT EXISTS org     TEXT NOT NULL DEFAULT '';
ALTER TABLE console.sessions ADD COLUMN IF NOT EXISTS role    TEXT NOT NULL DEFAULT '';
ALTER TABLE console.sessions ADD COLUMN IF NOT EXISTS email   TEXT NOT NULL DEFAULT '';
ALTER TABLE console.sessions ADD COLUMN IF NOT EXISTS name    TEXT NOT NULL DEFAULT '';

-- The DEFAULTs above exist only so the ADD COLUMN succeeds on a table that
-- still has rows in some other deployment's ordering. Dropping them now means
-- a future INSERT that forgets one of these fails loudly instead of quietly
-- writing an empty subject, which would be a session belonging to nobody.
ALTER TABLE console.sessions ALTER COLUMN subject DROP DEFAULT;
ALTER TABLE console.sessions ALTER COLUMN org     DROP DEFAULT;
ALTER TABLE console.sessions ALTER COLUMN role    DROP DEFAULT;
ALTER TABLE console.sessions ALTER COLUMN email   DROP DEFAULT;

CREATE INDEX IF NOT EXISTS console_sessions_subject_idx
    ON console.sessions(subject);

-- Spent assertions, so one cannot be used twice.
--
-- An assertion reaches the console through a browser, which is not a place a
-- bearer credential can be kept away from whatever else runs on the page. A
-- replayable one could be exchanged for a second session, or re-posted to
-- /api/auth/sudo to obtain step-up without the trip back to Cloud that step-up
-- exists to force.
--
-- expires_at is the assertion's own exp. Once past, the assertion is refused
-- on its expiry anyway and the row stops carrying information, so the session
-- GC deletes it.
CREATE TABLE IF NOT EXISTS console.spent_assertions (
    jti        TEXT        PRIMARY KEY,
    spent_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS console_spent_assertions_expires_idx
    ON console.spent_assertions(expires_at);

DROP TABLE IF EXISTS console.users;
