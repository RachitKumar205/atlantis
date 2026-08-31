-- What each organisation calls itself, alongside the names already on the row.
--
-- console.sessions.orgs carries the names a switcher lists; this carries the
-- display names for the ones that have a different one. The assertion supplies
-- both, so neither costs a call to Cloud per page.
--
-- JSONB and not a second array: the two would have to stay the same length and
-- in the same order to be read as pairs, and nothing in the schema could say
-- so. A map cannot fall out of step with itself.
--
-- DEFAULT '{}' rather than NULL. Every session open when this runs was written
-- before the column existed, and a NULL would make the read decide between "no
-- display names" and "not recorded" — which are the same thing here and should
-- not need telling apart.
ALTER TABLE console.sessions
    ADD COLUMN IF NOT EXISTS org_names JSONB NOT NULL DEFAULT '{}'::jsonb;
