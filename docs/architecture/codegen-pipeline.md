# Codegen pipeline

How a `.atl` file becomes Postgres migrations, proto definitions, and a typed Go client. Source lives under `internal/dsl/` (lex, parse, IR) and `internal/codegen/` (emitters and diff). Server-side entity handlers are no longer generated — entity CRUD is handled by runtime dispatch from the IR.

## Stages

```
.atl files → Lexer → AST → Validator → IR → Emitters → Outputs
                                              ├─ proto/     (.proto files)
                                              ├─ client/    (clients/go/pb/, clients/go/client/)
                                              └─ SQL        (migrations/tidectl/_staged/)
```

The runtime server does not use the generated `gen/go/server/` output; entity CRUD — create, read, update, delete, list, and predicate filtering — is handled by runtime dispatch from the IR. The `EmitGoServer` emitter still exists in `internal/codegen/server.go` for backward compatibility but is not called in the default server startup path. The server reads the IR checkpoint at startup and hot-reloads via PostgreSQL `LISTEN/NOTIFY` when `tide apply` persists a new checkpoint. Field changes to existing entities take effect within seconds; adding a brand-new entity requires a rolling restart because gRPC services are registered once at startup.

There are two ways to produce the typed Go client:

- **Caller-local (default for callers):** `tide generate`, run from the caller's repo, fetches the canonical IR over the `GetCanonicalIR` RPC, filters it to the namespaces in the caller's `tide.yaml`, and emits proto + typed wrappers into the caller's own module (`output_dir`). It writes its own `buf.gen.yaml` with a single `go_package_prefix = <module>/<output_dir>` and shells out to `buf generate`. The output compiles in the caller's module with no `replace` directive and no dependency on a central SDK for the generated types. See [schema flow](schema-flow.md#caller-local-sdk-generation).
- **Central (atlantis's own tests):** `tidectl codegen` + `buf generate` from the atlantis repo emit the full merged SDK into `clients/go/`. Still used by atlantis's integration tests; callers no longer consume it.

Both paths run the same emitters (`internal/codegen/`); caller-local generation just passes a `GenConfig{ModulePrefix}` and a namespace-filtered IR.

## Lexer and parser

`internal/dsl/lexer.go`, `internal/dsl/parser.go`. Hand-written recursive descent; no parser generator. Error messages are tuned by hand.

The parser produces an AST per file. Files are parsed independently — there is no cross-file resolution at this stage.

## IR

`internal/dsl/ir.go`. Lowering to IR is where the per-file ASTs union into a single symbol table; this is the first stage that can see all entities. The IR is what every downstream emitter consumes. A new emitter is a new pass over the IR.

## Validator

`internal/dsl/validate.go`. Walks the lowered IR and enforces:

- Every `references <Entity>.<field>` resolves to a declared entity.
- Every `$name` in a `query` or `procedure` body matches a declared input.
- Every `touches(<Entity>, ...)` matches a declared entity.
- Field types are in the supported set.
- `composite_pk` members are all `not null`.
- `partition by` fields exist on the entity.

`query` and `procedure` SQL bodies are validated through [`pg_query_go`](https://github.com/pganalyze/pg_query_go), which wraps the PostgreSQL parser. The validator runs `raw_parse` (syntactic check) plus a table-reference check against the merged schema; it does not run `parse_analyze` (full semantic analysis), so type-mismatch errors in expressions surface at migration runtime, not at apply time.

## Emitters

Each emitter is a Go package under `internal/codegen/`:

- `proto.go` — emits `<entity>.proto` per entity, including the per-field predicate messages (`StringPredicate`, etc.).
- `client.go` — emits the typed Go client SDK module under `clients/go/pb/<namespace>/` and typed wrappers under `clients/go/client/`.
- `sql.go` — emits `CREATE TABLE`, indexes, FKs, triggers; combined into one staged migration file under `migrations/tidectl/_staged/`.
- `coltype/` — shared column-type mapping consumed by every other emitter so a new type lands in one place.

`server.go` (`EmitGoServer`) still exists for backward compatibility but is not called in the default server startup path. The `gen/go/server/` output directory is no longer used. Per-entity scan helpers, bind helpers, and handler methods are now built at runtime from the IR — see `internal/server/entity/`.

## Diff and migration generation

`internal/codegen/diff.go` compares the previous applied IR to the new IR and classifies each change. The classifier returns a `ChangeClass` enum; see `diff.go` for the full set. The three top-level buckets:

- **Additive** — new table, new column with default, new index. Applies without backfill.
- **Backfill-required** — column type tightening, new `NOT NULL` without default. The caller must supply backfill SQL.
- **Cross-caller breaking** — removing a field another caller references, dropping an entity that's a foreign-key target. Rejected.

The migration SQL is staged under `migrations/tidectl/_staged/`. `tidectl approve` promotes it into `migrations/tidectl/` with sequential numbering.

## Transaction boundary

The DDL migration plus the bookkeeping write (recording the new schema version, the responsible caller, and the new IR checkpoint with its content hash) commit inside one Postgres transaction. A trigger fires `NOTIFY atl_schema_changed` on commit, and the server's schema listener hot-reloads entity metadata automatically. Client-side artifacts — proto, SDK — are written to their output paths **after** the transaction commits. A crash between commit and emission leaves the schema migrated but the on-disk client artifacts stale; regenerating from the now-canonical IR fixes them — a caller re-runs `tide generate`, and the central test SDK re-runs `tidectl codegen` + `buf generate`. The exact entry point for the apply transaction is the `Service.ApplyMigration` handler in `internal/server/admin/`.

## Adding a new declaration form

To add `enum` declarations:

1. Extend the lexer with the `enum` keyword.
2. Extend the parser with an `EnumDecl` production.
3. Add IR types and validator rules.
4. Extend the SQL emitter with `CREATE TYPE ... AS ENUM ('a', 'b')`.
5. Extend the proto emitter to map enums to proto `enum` messages.
6. Extend `coltype/` so enum-typed fields map correctly.
7. Extend `diff.go` to classify enum-value changes (adding a value is additive; removing one is breaking if any column uses it).

A new client language (Python, TypeScript) is a single new emitter that reads the IR.
