-- first_name and last_name replace a single name field on the sign-up form.
--
-- name stays, as a generated column, because it is what leaves this service:
-- it is the display string in the profile response and in the identity
-- assertion, which the console stores on the session and the org server now
-- records against a schema event. Deriving it here keeps one value for every
-- reader to agree on, and a stored copy could disagree with the two columns it
-- was built from.
--
-- btrim, so a person who gives only one of the two does not get a name with a
-- space at one end. Both empty yields '', which every consumer already reads as
-- "no name given" and falls back to the email address for.
ALTER TABLE cloud.users
    ADD COLUMN IF NOT EXISTS first_name TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_name  TEXT NOT NULL DEFAULT '';

-- Existing rows hold one string. Everything before the first space is the
-- given name and the remainder is the family name, which is wrong for a great
-- many names but is the only division the stored value supports. A row with no
-- space keeps its whole value in first_name, where it renders unchanged.
UPDATE cloud.users
   SET first_name = CASE WHEN position(' ' in btrim(name)) > 0
                         THEN split_part(btrim(name), ' ', 1)
                         ELSE btrim(name) END,
       last_name  = CASE WHEN position(' ' in btrim(name)) > 0
                         THEN btrim(substring(btrim(name) from position(' ' in btrim(name)) + 1))
                         ELSE '' END
 WHERE btrim(name) <> '';

ALTER TABLE cloud.users DROP COLUMN name;

ALTER TABLE cloud.users
    ADD COLUMN name TEXT
    GENERATED ALWAYS AS (btrim(first_name || ' ' || last_name)) STORED;
