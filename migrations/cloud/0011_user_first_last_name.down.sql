-- The generated column becomes a plain one holding what it last derived.
ALTER TABLE cloud.users DROP COLUMN name;

ALTER TABLE cloud.users ADD COLUMN name TEXT NOT NULL DEFAULT '';

UPDATE cloud.users SET name = btrim(first_name || ' ' || last_name);

ALTER TABLE cloud.users
    DROP COLUMN first_name,
    DROP COLUMN last_name;
