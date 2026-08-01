# Changelog

Notable changes to atlantis, newest first.

This file exists mainly for **breaking changes**. atlantis generates the DDL
that migrates production databases, so a change to how something is named or
enforced can alter what a later migration does to data that already exists. Those
are called out explicitly, with what to check and what to do about it.

Unreleased entries describe work on `main` that has not been tagged.

## Unreleased

### Fixed

#### Scheduled jobs now actually run

`atlantis.job_schedules` was written by two functions and read by none: the
scheduler component that `migrations/infra/0006_jobs.up.sql` describes had never
been written. Everything downstream of it was inert —

- `ttl_field` deleted nothing. The TTL sweeper was registered by no call site
  and scheduled by nobody, and its `DELETE ... LIMIT` was MySQL syntax that
  Postgres rejects, so it would have failed on its first entity had it ever run.
  Both are fixed; expired rows are now deleted every five minutes.
- The DSL's `schedule "..."` modifier parsed, validated and produced no fires.

If you relied on `ttl_field`, expect a backlog of expired rows to be deleted
over the first few sweeps after upgrading. The sweeper deletes at most 1000 rows
per entity per fire, so a large backlog drains over several cycles rather than
in one statement.

### Breaking

#### Parked tables are named after their physical table, not the entity

Destructive migrations park objects rather than dropping them: a removed table
is moved into the `atlantis_tombstone` schema and kept for 30 days. The parked
name was derived from the entity's computed `<namespace>_<entity>` name, which
is wrong for any entity that overrides its table with `table "schema.name"` —
and 17 of the 19 schemas in this repository do.

The effect was that the real table was moved into the tombstone schema under
its own name, the rename that should have marked it as parked silently matched
nothing, and the register recorded an object that had never existed. The reaper
then "reaped" the phantom and recorded a successful drop, while the real table
sat unreferenced and unrestorable.

The parked name is now `<source schema>_<source table>__parked` and the register
records the source schema so a restore knows where to put the table back.

**What to check.** If you applied a destructive migration on a build between
`387656d` and this change, look for orphans:

```sql
SELECT c.relname
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'atlantis_tombstone'
   AND c.relname NOT LIKE '%\_\_parked';
```

Anything listed was parked but never registered. Rename it back into its
original schema to restore it; it is not otherwise reachable.

#### Unnamed entity-level CHECK constraints are named differently

A `check "..."` written at entity level without `as <name>` used to be named
positionally:

```
<table>_check_1, <table>_check_2, ...
```

It is now derived from the predicate:

```
<table>_check_<first 4 bytes of sha256(expression)>
```

**Why.** Positional naming renames constraints when they are reordered. Moving
one unnamed check above another changed both names, so the differ had to either
emit `DROP CONSTRAINT` + `ADD CONSTRAINT` — revalidating the whole table under
an `ACCESS EXCLUSIVE` lock for a schema that had not changed — or stay silent and
let a migrated database end up with different constraint names than a freshly
created one. Deriving the name from the predicate makes reordering the no-op it
actually is.

**Who is affected.** Only databases that already contain an unnamed
**entity-level** check. Two common cases are *not* affected:

- `check "..." as my_name` — an explicit name is used verbatim and is untouched.
- A `check` written as a field modifier — named `<table>_<column>_check`, unchanged.

**What goes wrong if you are affected.** Nothing immediately: the migration
diff compares schema to schema, so an unchanged schema produces no DDL. The
problem appears the first time you *remove* or *edit* that check. atlantis emits
`DROP CONSTRAINT IF EXISTS` against the new name, the old name is still what the
database holds, and `IF EXISTS` turns the miss into a silent success. The plan
reports the constraint gone; the database goes on enforcing it.

**How to check.** Grep will not answer this reliably — `.atl` files live in
caller repositories and may be excluded by `.gitignore`, and lowering files one
at a time fails because they cross-reference each other. Parse them all together
and inspect the IR:

```go
// Pass EVERY .atl file to a single Lower() call — they cross-reference each
// other, and lowering them individually fails with "references unknown entity"
// while silently reporting whatever the self-contained files happened to hold.
ir, err := dsl.Lower(files)
for i := range ir.Entities {
    e := &ir.Entities[i]
    for _, c := range e.Checks {   // Entity.Checks, not the cache block
        if c.Name == "" {
            // affected: an unnamed entity-level check
        }
    }
}
```

This repository's own schemas were checked this way: four entity-level checks,
all four explicitly named, none affected.

**What to do if you find one.** Either is fine:

1. **Name it** — `check "..." as the_name_already_in_your_database`. An explicit
   name is used verbatim, so this pins the existing constraint permanently and
   needs no DDL.
2. Drop the old constraint by hand during a maintenance window and let atlantis
   create the new one.

Option 1 is the durable fix, and worth preferring generally: a named constraint
is outside the generated-name scheme entirely.

### Added

- `chunk_time_interval` on hypertables. It was documented three times and
  implemented zero times; it now reaches `create_hypertable`, and changing it
  emits `set_chunk_time_interval` rather than silently doing nothing.
- Cross-entity cache invalidation. `invalidate_on: write(Child where fk =
  self.id)` now invalidates the parent when a child is written, including both
  parents when a child is reparented.
- `Get` is served through the read cache, which was previously built, wired into
  the invalidation worker, and never read from.
- Startup reports which TimescaleDB build the database runs.
  `ATL_REQUIRE_APACHE_TIMESCALE=true` refuses to start on the Community (TSL)
  build.

### Fixed

- CHECK constraints are now diffed at all. Adding one previously produced an
  empty diff and DDL containing no CHECK.
- `partition by` no longer claims tenant isolation it does not provide. It is
  enforced by Postgres row-level security, in the database rather than in each
  generated read.
- No-rows errors from the Postgres adapter are visible to `runtime.IsNoRows`
  again, so a `Get` for a missing row returns NotFound rather than a raw driver
  error.
- Entities written by a custom procedure are no longer served from the read
  cache. A procedure cannot invalidate the row bodies it changes, so caching
  them served stale rows.

### Changed

- **The Postgres image is now the Apache-2.0 TimescaleDB build**
  (`timescale/timescaledb-ha:pg16-oss`) in local dev, the self-host bundle and
  CI. It was `-all`, which is the Community (TSL) edition.

  The Timescale License forbids using TSL software to provide a
  database-as-a-service, and its "Value Added" exception does not cover
  atlantis: that exception requires users be prohibited from modifying the
  database schema via DDL, which is the one thing this product exists to allow.
  Nothing is lost — atlantis uses hypertables only, and `create_hypertable` and
  `set_chunk_time_interval` are both Apache-2.0. `-oss` still carries `vector`
  and `postgis`; the only omission is `timescaledb_toolkit`, which nothing here
  uses.

  Dev and CI match production deliberately: developing against TSL features that
  cannot be shipped is how a licence dependency arrives unnoticed.

  **If you self-host**, running the Community build for your own use remains
  entirely permitted — the licence restricts offering the software as a service,
  not running it. Pin whichever image you prefer.

- Whether a `check` binds to the preceding field or to the entity is now decided
  by indentation rather than by what happens to precede it. A `check` at or left
  of its field's column is an entity-level constraint; indented past it, it is
  that field's. Every schema in this repository parsed identically before and
  after.
- A field may declare only one `check`. A second was previously accepted and
  silently discarded.
