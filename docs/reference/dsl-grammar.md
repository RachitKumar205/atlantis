# DSL reference

Syntax reference for `.atl` files.

## Notation

In the grammar productions below:

- `[X]` is optional.
- `{X}` is zero or more.
- `X | Y` is alternation.
- Quoted text appears literally; `Ident`, `Type`, etc. are productions.

Whitespace separates tokens but is otherwise insignificant. Line comments start with `//` and run to end of line. There are no block comments.

## Top-level

```
File         = { Declaration }
Declaration  = Entity | Hypertable | Query | Procedure
```

## Entities

```
Entity = "entity" Ident "in" Ident "{" EntityBody "}"

EntityBody =
    { FieldDecl }
    [ "primary" "by" IdentList ]
    { "unique" "by" IdentList }
    { "index" "by" IndexFieldList }
    { [ "unique" ] "index" "partial" "by" IdentList "where" PartialPredicate }
    { "index" "hnsw" "on" Ident "ops" VectorOps }
    { "index" "gin" "on" Ident }
    [ "soft_delete" "by" Ident ]
    [ "touch_on_update" "by" Ident ]
    [ "partition" "by" Ident ]
    [ "table" StringLiteral ]
    [ "ttl_field" Ident ]
    { "check" "\"" SQLExpr "\"" [ "as" Ident ] }
    [ CacheBlock ]

IdentList      = Ident { "," Ident }

IndexFieldList = IndexField { "," IndexField }
IndexField     = ( Ident | "expr" StringLiteral ) [ "asc" | "desc" ]

VectorOps      = "cosine" | "l2" | "ip"

PartialPredicate = <any SQL boolean expression valid in a Postgres index predicate>
```

The `where` predicate is **a SQL expression**, parsed by Postgres's own parser
(`pg_query`) rather than a fixed DSL grammar. It accepts the full surface of a
legal index predicate: boolean operators (`and`/`or`/`not`), comparisons,
arithmetic, `||`, `like`/`~`, JSON operators, `is [not] null`, `[not] in (...)`,
`between`, array literals with `any`/`all`, `is distinct from`, immutable function
calls (`lower(email)`), casts (`amount::numeric`), and `case … end`. The predicate
runs to the first newline, entity-closing `}`, or `//` comment outside any
`"..."` string.

String literals use the DSL convention — double quotes (`where status = "active"`)
— consistent with the rest of the `.atl`; atlantis converts them to SQL on the way
in. Because `"..."` is always a string (never a quoted identifier), a column whose
name is a SQL reserved word can't be referenced: `where "order" > 0` reads as a
string, and a bare `where order > 0` is a Postgres syntax error.

It must be a *legal Postgres index predicate*. Subqueries, window functions, and
aggregate syntax (`count(*)`, `… filter (…)`) are rejected at parse with a clear
error. A plain aggregate (`sum(col)`) or a volatile function (`now()`, `random()`)
is caught by Postgres when the index is built at apply time.

Entity names use `PascalIdent`; namespaces use `SnakeIdent`. Underscores are syntactically valid in namespaces but not conventional.

The namespace becomes the package segment under `output_dir/` and the schema prefix on the generated table name: `<namespace>_<snake_entity>`.

### Fields

```
FieldDecl = Ident Type { Modifier }

Modifier =
    "primary"
  | "serial"
  | "not" "null"
  | "default" DefaultExpr
  | "unique"
  | "references" QualifiedField [ "on" "delete" RefAction ]
  | "check" "\"" SQLExpr "\""     // must share the field's line, or be indented past it

DefaultExpr =
    FunctionCall      // e.g. now(), gen_random_uuid()
  | NumericLiteral    // e.g. 0, 3.14
  | StringLiteral     // single-quoted, e.g. 'pending'
  | BooleanLiteral    // true, false

QualifiedField = [ Namespace "." ] Entity "." Field

RefAction = "cascade" | "set" "null" | "restrict"
```

Field names use `SnakeIdent`. Modifier order is flexible, but the lexer rejects incompatible combinations: `serial` with `default`, two `primary` modifiers on different fields, etc.

`QualifiedField`: same-namespace references can omit the namespace (`Customer.id`); cross-namespace references qualify (`vendor.Product.id`). The referenced field must be declared `primary` or have a column-level `unique`.

`check`: the string body is parsed as a Postgres boolean expression. Anything valid inside `CREATE TABLE ... CHECK (...)` is accepted.

#### Where a `check` binds

`check` is the one keyword that is valid both as a field modifier and as an entity member, and both spellings are `check` followed by a string. Indentation decides which you get:

```
entity Order in shop {
  id      bigint primary
  status  varchar(20) not null
          check "status IN ('open','closed')"   // the field's — indented past it

  total   int not null
  qty     int not null

  check "total >= qty" as total_covers_qty      // the entity's — at member indent
}
```

The rule: **a field's modifiers may continue on following lines, and a continuation line must be indented past the field it belongs to.** A `check` at or left of its field's column starts a new member, so it is an entity-level constraint.

Two consequences worth knowing:

- Only an entity-level `check` accepts `as <name>`. On a field's continuation line, `as` is a syntax error: a field modifier has no name of its own, so atlantis generates one — usually `<table>_<column>_check`, shortened with a hash suffix when that would exceed Postgres's 63-byte identifier limit, or suffixed with a digit if an entity-level check already claimed the name. Name a constraint yourself if you need to depend on what it is called.
- A field carries exactly one `check`. Declaring a second is an error rather than a silent replacement; express additional constraints as entity-level checks.

Columns count bytes, so one tab is one column. Indent consistently — a file mixing tabs and spaces inside one entity can bind a `check` differently from how it reads.

### Field types

| Type | PostgreSQL | Notes |
|---|---|---|
| `bigint` | `BIGINT` | |
| `int` | `INTEGER` | |
| `smallint` | `SMALLINT` | |
| `real` | `REAL` | 32-bit float |
| `double` | `DOUBLE PRECISION` | 64-bit float |
| `boolean` | `BOOLEAN` | |
| `varchar(N)` | `VARCHAR(N)` | |
| `text` | `TEXT` | |
| `citext` | `CITEXT` | case-insensitive text |
| `jsonb` | `JSONB` | |
| `bytea` | `BYTEA` | binary |
| `timestamptz` | `TIMESTAMPTZ` | timestamp with timezone |
| `date` | `DATE` | |
| `interval` | `INTERVAL` | |
| `numeric(p, s)` | `NUMERIC(p,s)` | arbitrary precision |
| `uuid` | `UUID` | |
| `vector(N)` | `vector(N)` | pgvector extension; index with `index hnsw on <field> ops <...>` |
| `[]T` | `T[]` | array; element type `T` is any scalar above except `vector` and `[]T` |

Go and proto mappings are in [the type mapping reference](dsl-types.md).

### Modifier semantics

- `primary` — primary key. Exactly one field, unless `primary by` is used at the entity level. The two are mutually exclusive.
- `serial` — Postgres assigns the value via a sequence. Valid only with `bigint primary` or `int primary`. Incompatible with `default`.
- `not null` — disallows null. Implied by `primary`.
- `default <expr>` — Postgres default expression. See `DefaultExpr` above.
- `unique` — Postgres column-level `UNIQUE`. For multi-column, use `unique by` at the entity level.
- `references <Entity>.<field>` — foreign key. `on delete` accepts `cascade`, `set null`, `restrict`. `on update` is not supported; see [Known gaps](#known-gaps).
- `check "<predicate>"` — Postgres `CHECK` constraint on this column, given a generated name. Must be on the field's line or indented past it; see [Where a `check` binds](#where-a-check-binds). One per field — declare further constraints at the entity level.

### Entity-level clauses

- `primary by f1, f2` — composite primary key. Member fields must each be `not null`. Mutually exclusive with per-field `primary`.
- `check "<predicate>" [as <name>]` — table-level `CHECK` constraint. Unlike the field modifier this may reference several columns, and `as <name>` sets the constraint name in the database. Must sit at member indentation; see [Where a `check` binds](#where-a-check-binds).
- `unique by f1, f2` — multi-column unique constraint. May appear multiple times. For a single column, use the per-field `unique` modifier instead.
- `index by f1, f2` — non-unique B-tree index. May appear multiple times. Each field may instead be an expression (`expr "lower(email)"`) and may carry a per-field `asc` or `desc` (e.g. `index by created_at desc`).
- `index partial by f1, f2 where <predicate>` — partial index. `<predicate>` is a [`PartialPredicate`](#entities) (any SQL boolean expression valid in a Postgres index predicate). e.g. `index partial by sku where deleted_at is null`, `index partial by id where status = "active" and lower(sku) like "a%"`.
- `unique index partial by f1, f2 where <predicate>` — partial **unique** index (`CREATE UNIQUE INDEX … WHERE …`). Use it for uniqueness scoped by a predicate — e.g. `unique index partial by sku where deleted_at is null` makes `sku` unique among non-soft-deleted rows, or `unique index partial by user_id where is_default` for one default per user. A Postgres UNIQUE *constraint* can't be partial, so `unique` / `unique by` can't express this. Same predicate grammar as `index partial`.
- `index hnsw on <field> ops <cosine|l2|ip>` — pgvector HNSW index over a `vector(N)` field. `ops` picks the operator class: `cosine`, `l2` (Euclidean), or `ip` (inner product).
- `index gin on <field>` — GIN index, for `jsonb` and array fields.

The only unique-index form is `unique index partial`; `index by`, `index hnsw`, `index gin`, and the non-`unique` `index partial` are all non-unique. Non-partial uniqueness is declared with the per-field `unique` modifier or entity-level `unique by` (which emit UNIQUE constraints). A live `CREATE UNIQUE INDEX` the schema doesn't account for is treated as drift — `tide apply` refuses it unless `ATLANTIS_ALLOW_INDEX_DRIFT=1`; a declared `unique index partial` whose predicate matches the live one is recognized and not drift. See [`tide apply`](cli-tide.md).
- `soft_delete by <field>` — replaces row deletion with setting `<field>` (must be `timestamptz`) to `now()`. Reads filter `<field> IS NULL` automatically.
- `touch_on_update by <field>` — Postgres trigger sets `<field>` (must be `timestamptz`) to `now()` on every `UPDATE`.
- `partition by <field>` — Atlantis-level multi-tenant partition. Not Postgres table partitioning. Generated read RPCs inject `<field> = <caller-partition>` into the predicate; callers cannot override. The caller partition is read from the auth context.
- `table "<schema.table>"` — overrides the physical table name. Without it, atlantis stores the entity at `atlantis.<namespace>_<snake_entity>`. The value's shape is `[schema.]table`, each segment matching `[A-Za-z_][A-Za-z0-9_]*`; a bare name (`table "vendors"`) lands in `public`. Foreign keys whose target carries the modifier render `REFERENCES "<schema>"."<table>"`. Changing the value on a previously-applied entity is classified `cross_caller_breaking` and rejected by `tide plan`; atlantis does not auto-rename. Used when adopting an existing database — see [Adopt an existing database](../guides/adopt-an-existing-database.md).

### Cache block

```
CacheBlock = "cache" "{" "read_through" "ttl" "=" Duration [ "tag" "=" StringLiteral ] "}"

Duration = Integer DurationUnit
DurationUnit = "ns" | "us" | "ms" | "s" | "m" | "h"
```

`read_through` is currently the only supported caching mode.

The `tag` is a double-quoted string with `{field_name}` interpolation placeholders. Field names inside `{...}` must exist on the entity. Cache entries with the same resolved tag are invalidated as a group. See [Caching and invalidation](../concepts/caching-and-invalidation.md).

## Queries

```
Query = "query" Ident "for" Ident "{" QueryBody "}"

QueryBody =
    "input"  "{" ParamList "}"
    OutputDecl
    SqlBlock

OutputDecl = "output" "as" Ident
           | "output" "{" ParamList "}"

ParamList = Ident ":" Type { "," Ident ":" Type }

SqlBlock = "sql" "touches" "(" IdentList ")" "{" SQL "}"
```

The `for <Ident>` after the query name names the entity the query semantically belongs to. It becomes a method on that entity's generated client and must appear in `touches(...)`.

- `output as <Entity>` returns rows of that entity. The SQL must project every column the entity declares.
- `output { ... }` returns an ad-hoc row type. The SQL must project columns matching the declared names and types.

Parameters in the SQL body use `$name` syntax. Atlantis rewrites them to Postgres positional placeholders (`$1`, `$2`, ...) before execution. The body is validated when you run `tide apply`.

`touches(...)` lists the entities the query reads. The cache layer uses it for query-result invalidation.

## Procedures

```
Procedure = "procedure" Ident "for" Ident "{" ProcedureBody "}"

ProcedureBody =
    "input" "{" ParamList "}"
    "steps" "{" SqlBlock { SqlBlock } "}"
```

The `for <Ident>` after the procedure name names the entity the procedure belongs to (same semantics as a query). It becomes a method on that entity's generated client.

Steps run inside one Postgres transaction. The transaction commits when every step succeeds; any error rolls back the entire transaction. Each step's `touches(...)` declares the write set the cache outbox invalidates after commit.

Procedures do not return rows. Read the result with a separate query.

## Hypertables

```
Hypertable = "hypertable" Ident "in" Ident "on" Ident "{" HypertableBody "}"

HypertableBody =
    { FieldDecl }
    [ "chunk_time_interval" Duration ]
    [ other EntityBody clauses... ]
```

The time column is named in the header — `hypertable Reading in iot on recorded_at { ... }` — and must be a `timestamptz` field declared in the body. It becomes the time dimension passed to `create_hypertable`. The entity-level `partition by` clause is a different mechanism (atlantis multi-tenant partitioning); both may coexist on one hypertable.

`chunk_time_interval` sizes each chunk and uses the same `Duration` syntax as cache TTLs. Omit it to take TimescaleDB's default (7 days). Changing it later emits `set_chunk_time_interval`, which applies to chunks created from that point on — existing chunks keep the size they were made with.

Only Apache-2.0-licensed TimescaleDB functionality is emitted (`create_hypertable`, `set_chunk_time_interval`), so a hypertable schema imposes no Timescale License obligation.

Hypertables accept every entity-body clause (indexes, unique constraints, soft delete, cache block, the multi-tenant `partition by`).

## Identifiers

```
PascalIdent = [A-Z][A-Za-z0-9]*
SnakeIdent  = [a-z][a-z0-9_]*
```

Entity, namespace, query, and procedure names use `PascalIdent`. Field, input, and output names use `SnakeIdent`.

## Reserved words

The following are reserved everywhere and cannot be used as identifiers:

```
entity, hypertable, query, procedure,
in, for, input, output, steps, sql, touches, as,
primary, serial, not, null, default, unique, references, check,
on, update, delete, cascade, set, restrict,
index, partial, where, is,
hnsw, ops, cosine, l2, ip, gin, asc, desc, expr,
soft_delete, touch_on_update, partition, by,
table,
cache
```

The following are contextual — they are keywords only inside specific blocks and may otherwise be used as identifiers:

```
read_through, ttl, tag         // only inside cache { ... }
chunk_time_interval            // only inside hypertable { ... }
```

## Known gaps

This reference does not yet cover: the `ivfflat` vector-index method (only `hnsw` is supported), GiST indexes, `on update` foreign-key actions, enum types, view declarations, and import statements. Tracked in the project issue tracker.
