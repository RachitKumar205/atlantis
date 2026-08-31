-- Sessions stop carrying display names. The switcher falls back to the
-- organisation names, which are on the row either way.

ALTER TABLE console.sessions
    DROP COLUMN IF EXISTS org_names;
