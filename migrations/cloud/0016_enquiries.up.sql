-- A message left through the form on the marketing site.
--
-- An anonymous visitor writes it, so there is no user to police by;
-- store.unpolicedTables records the exemption.
--
-- email is folded by store.NormalizeEmail, as cloud.users.email is.
--
-- message is at most 4000 characters, the form's maxlength, counted after the
-- server folds CRLF to LF.
CREATE TABLE IF NOT EXISTS cloud.enquiries (
    id         BIGSERIAL   PRIMARY KEY,
    email      TEXT        NOT NULL
        CHECK (email = lower(btrim(email)) AND char_length(email) BETWEEN 3 AND 254),
    message    TEXT        NOT NULL
        CHECK (btrim(message) <> '' AND char_length(message) <= 4000),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- store.CreateEnquiry counts the last 24 hours on every insert.
CREATE INDEX IF NOT EXISTS enquiries_created_at ON cloud.enquiries (created_at);
