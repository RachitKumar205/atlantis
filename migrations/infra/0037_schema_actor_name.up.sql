-- actor_name: the person's display name as it stood when the event was
-- recorded, beside actor and actor_email.
--
-- 0036 stored no name, on the grounds that a name is the most changeable of
-- the three. That reasoning holds for a directory, which answers who someone
-- is now, and not for this table, which answers who acted then. A commit
-- stores its author's name for the same reason: re-reading it years later must
-- not depend on an identity service that has since renamed or deleted them.
--
-- This server cannot resolve actor to a person. Console identity moved to
-- Cloud, so there is no local user table to join, and a read that had to call
-- Cloud would make history unreadable whenever Cloud was down.
--
-- '' where no name was given. The console falls back to actor_email, then to
-- the caller, so a row with none of the three still renders.
ALTER TABLE atlantis.schema_versions
    ADD COLUMN IF NOT EXISTS actor_name TEXT NOT NULL DEFAULT '';
