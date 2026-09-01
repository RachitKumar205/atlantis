-- actor: the human a schema event is attributed to, beside the caller.
--
-- atlantis.schema_versions already had caller, and this looks like that column
-- but is not. caller is the mTLS CN the request arrived under, which the
-- caller cannot choose. actor is written from the request body, and the server
-- cannot authenticate it: that identity system belongs to the console. The two
-- sit side by side rather than one replacing the other, the same way
-- jobs.owner_caller sits beside jobs.submitted_by. They answer different
-- questions and only one of them is trustworthy.
--
-- On schema_versions rather than entity_lineage, because a human acts on an
-- event and not on a table. entity_lineage.introduced_at and .last_modified_at
-- are already foreign keys to this table, so blame is one join away and no row
-- carries a second copy that can disagree with the first.
--
-- actor carries a scheme: console:usr_04f1, cli:rachit. The CHECK is what
-- makes that load-bearing rather than a habit, and it is the whole of the
-- kind — a separate actor_kind column would be a second thing to keep in
-- step, and the first write that set one without the other would disagree
-- with itself in silence.
--
-- actor_email is denormalised at write time, as console.audit_log does with
-- actor and actor_email, and for the reason written there: this server has no
-- access to the console's user table and cannot resolve an id to a person, not
-- now and not when the row is read years later. A name is not stored — it is
-- the most changeable of the three and the least able to identify anyone.
--
-- '' means the event named no human. That is the honest reading of an
-- unattended CI apply, and of every row written before this migration.
--
-- Nothing is backfilled. The only value this migration could invent for the
-- existing rows is the event's own caller, which is not a person, and the
-- lineage seeding added alongside it will not overwrite what it finds. An
-- organisation wanting attribution on an import it already ran re-runs Apply
-- against it, which is idempotent and writes real per-caller ownership.
ALTER TABLE atlantis.schema_versions
    ADD COLUMN IF NOT EXISTS actor       TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS actor_email TEXT NOT NULL DEFAULT '';

ALTER TABLE atlantis.schema_versions
    ADD CONSTRAINT schema_versions_actor_scheme
    CHECK (actor = '' OR actor ~ '^[a-z][a-z0-9]*:');
