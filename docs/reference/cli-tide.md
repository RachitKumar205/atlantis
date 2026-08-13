# `tide` CLI

```
tide <command> [flags]
```

The caller-side CLI. Run from a service repo containing one or more `.atl` files and a `tide.yaml`.

`-h` and `--help` are accepted by `tide` and every subcommand.

## Configuration file

`tide` reads `./tide.yaml` from the current directory (overridable per command with `--config <path>`). The file must contain at minimum:

```yaml
caller: <name>                # required
endpoint: <host:port>         # required
schema_paths:                 # required — at least one directory
  - internal/foo
  - internal/bar
output_dir: internal/gen/pcclient   # required for `tide generate`
generate:                     # required for `tide generate` — namespaces to emit
  - consumer
  - vendor
tls:                          # optional
  cert: <file>
  key:  <file>
  ca:   <file>
```

`output_dir` is the directory inside the caller's own Go module where `tide generate` writes the typed client. `generate` lists the namespaces the caller consumes (its own plus any it reads cross-namespace). Both are only required for `tide generate`; the other commands ignore them.

YAML does not expand `${VAR}` placeholders. The config loader rejects literal `${VAR}` strings in the three TLS fields specifically (other fields are passed through verbatim).

### `schema_paths` semantics

Each path is walked recursively; every file with extension `.atl` is included. Order does not affect schema resolution. Paths are recorded relative to the caller's repo root so server-side error messages stay useful in the caller's context.

## Environment variables

| Variable | Overrides | Notes |
|---|---|---|
| `ATL_CALLER` | `caller` | |
| `ATL_ENDPOINT` | `endpoint` | |
| `TIDE_TLS_CERT` | `tls.cert` | Path to mTLS client certificate. |
| `TIDE_TLS_KEY` | `tls.key` | Path to mTLS client key. |
| `TIDE_TLS_CA` | `tls.ca` | Path to CA certificate for verifying the server. |
| `ATL_GENERATE` | `generate` | Comma-separated namespace list; replaces the `generate:` field for `tide generate`. |

`TIDE_CALLER` and `TIDE_ENDPOINT` are not consulted; use `ATL_CALLER` / `ATL_ENDPOINT`.

Three additional variables carry inline PEM material so CI runners never write certs to disk. When set they take precedence over the file-path fields; setting both an inline PEM and its file-path counterpart for the same material is a config error.

| Variable | Overrides | Notes |
|---|---|---|
| `TIDE_TLS_CERT_PEM` | `tls.cert` | Inline PEM client certificate. |
| `TIDE_TLS_KEY_PEM` | `tls.key` | Inline PEM client key. |
| `TIDE_TLS_CA_PEM` | `tls.ca` | Inline PEM CA certificate. |

`tide job` and `tide workflow` read `$USER` to stamp the submitting principal on jobs and workflow runs.

## Commands

Every command accepts `--config <path>` (default `tide.yaml`) and `--timeout <duration>`. Duration uses Go's `time.ParseDuration` format (e.g. `30s`, `1m`, `500ms`). Default `30s`.

### `tide apply`

Submits the local `.atl` files to the server, runs the migration, and prints a hint for the caller to regenerate the typed Go client. No endpoint override flag — `apply` always targets the configured `endpoint`.

```
tide apply [--backfill] [--dry-run] [--no-pull]
```

| Flag | Description |
|---|---|
| `--backfill` | Boolean. Kick off the declarative backfill flow for a `backfill_required` plan (calls `BeginBackfillPlan`). Monitor progress with `tide backfill status`. |
| `--dry-run` | Plan only; do not apply. Same exit codes as a real apply. |
| `--no-pull` | Skip the automatic `tide pull` before the apply. Use when offline or when the local cache is known-current. |

The default flow runs `tide pull` first so cross-caller references resolve against the freshest merged schema.

If the database carries a bare unique index the schema doesn't declare — a `CREATE UNIQUE INDEX` with no backing constraint — `apply` refuses with a `DROP INDEX` remediation. Set [`ATLANTIS_ALLOW_INDEX_DRIFT=1`](configuration.md#schema-drift) to apply anyway. This doesn't change the plan class or exit code.

### `tide plan`

Validates the local schema against the server and reports what would change. Performs no server-side writes.

```
tide plan [--against <host:port>] [--format {table|json}] [--no-pull]
```

| Flag | Description |
|---|---|
| `--against <host:port>` | Override the configured endpoint for this command only. |
| `--format {table|json}` | Default `table`. `json` emits the raw planning response for downstream tools. |
| `--no-pull` | Skip the pre-plan refresh of `.tide-cache/`. |

A bare unique index the schema doesn't declare surfaces as an index-drift warning, but only under `--format=json` — in the `index_drift`, `index_drift_notes`, and `index_drift_error` fields. The `table` output does not render it. Drift never blocks `plan` or changes its exit code; `tide apply` is where it refuses unless [`ATLANTIS_ALLOW_INDEX_DRIFT=1`](configuration.md#schema-drift).

### `tide pull`

Downloads the merged schema into `.tide-cache/schema/` and records the server's schema version in `.tide-cache/version.json`. Subsequent pulls short-circuit when the version matches.

```
tide pull [--force]
```

| Flag | Description |
|---|---|
| `--force` | Pull even if the local cache version equals the server's. |

`.tide-cache/` mirrors every caller's currently-registered `.atl` files. It is not the generated Go client. Add it to `.gitignore`.

### `tide generate`

Generates the typed Go client SDK into the caller's own repo, scoped to the namespaces in `generate:`. Run from the caller repo root.

```
tide generate
```

The flow:

1. Fetch the canonical IR from the server (the `GetCanonicalIR` admin RPC). The canonical IR is the server's persisted schema checkpoint, with proto field numbers already assigned. Pulling those numbers from the server means the generated wire format matches it exactly — the caller never re-derives numbers locally.
2. Filter the IR to the `generate:` namespaces. The caller gets typed clients only for what it consumes, not every caller's entities.
3. Read the caller's `go.mod` to compute the package prefix `<module>/<output_dir>`.
4. Emit proto sources (the scoped namespaces plus the embedded `atlantis/common/v1` protos) and the typed Go wrappers into `output_dir`, then shell out to `buf generate` for the `.pb.go` wire types and `gofmt` the result.

Requirements:

- `output_dir` and a non-empty `generate:` list in `tide.yaml`.
- [`buf`](https://buf.build/docs/installation) on `PATH`.
- Run from the caller repo root (so `go.mod` is readable).

The generated tree lives in the caller's module (e.g. `internal/gen/pcclient/{pb,client}/...`) and is imported with the caller's own import path. Commit it like any other generated code — it is the caller's source, not a shared artifact. There is no dependency on a central `atlantis-go` SDK for the generated types; only the hand-written `atlantis-go/jobs` runtime remains a normal library dependency for callers that run job workers.

Re-run `tide generate` after any `tide apply` that changes a namespace the caller consumes.

### `tide list`

Fetches the merged schema and prints the path of every `.atl` file, sorted lexically.

```
tide list
```

### `tide show <substring>`

Fetches the merged schema and prints the canonical `.atl` text of every file whose full path contains the substring. Case-sensitive match.

```
tide show <substring>
```

Exits non-zero if no file matches.

### `tide backfill status [<plan-hash>]`

Reports the progress of a declarative backfill started by `tide apply --backfill`. With no argument, shows the latest backfill plan for the configured caller; pass a plan hash to inspect a specific one.

```
tide backfill status [<plan-hash>]
```

### `tide job submit|status|dead|retry`

Submits and inspects background jobs.

```
tide job submit <job-name> [--args=JSON] [--scheduled-at=RFC3339]
tide job status <job-id>
tide job dead   [--job-name=...] [--limit=N]
tide job retry  <dead-job-id>
```

`submit` enqueues a job (optionally scheduled for a future time); `status` reports one job's state; `dead` lists jobs in the dead-letter queue; `retry` re-enqueues a dead job. The submitting principal is stamped from `$USER`.

### `tide workflow start|status`

Starts and inspects multi-step workflows.

```
tide workflow start  <workflow-name> [--state=JSON]
tide workflow status <workflow-id>
```

The submitting principal is stamped from `$USER`.

### `tide history`

Prints schema versions newest-first: version number, caller, event type, change count, and timestamp.

```
tide history [--limit N] [--caller X] [--format json]
```

### `tide diff <from-version> <to-version>`

Computes the structural diff between two historical schema versions. The server loads both IR snapshots and runs the diff.

```
tide diff <from-version> <to-version>
```

### `tide blame <entity-id>`

Shows per-field provenance for an entity: who introduced each field, who last modified it, and the schema versions those events map to.

```
tide blame <entity-id>
```

### `tide owners`

Prints every active entity and the caller that introduced it — answers "who owns this table?" without reading version history.

```
tide owners
```

### `tide rollback`

Reverts the live schema to the state captured by a prior version. The server diffs current → target and emits the migration.

```
tide rollback --to=<version> [--dry-run] [--yes]
```

| Flag | Description |
|---|---|
| `--to=<version>` | Target schema version to revert to. |
| `--dry-run` | Emit the rollback plan without applying it. |
| `--yes` | Skip the interactive confirmation. |

### `tide sandbox boot|shell|spawn`

Drives the schema-true in-memory simulator bound to a local IR.

```
tide sandbox boot  <path> [--addr ADDR]   # start the HTTP control plane
tide sandbox shell <path>                 # interactive SQL REPL
tide sandbox spawn <path> -n N            # fork N children, time it, exit
```

`boot` defaults to `127.0.0.1:0` (kernel-chosen port) unless `--addr` pins one. See the [Sandbox HTTP API](sandbox-api.md).

### `tide caller alias list|add|rm`

Manages caller identity aliases.

```
tide caller alias list <caller>
tide caller alias add  <caller> <alias>
tide caller alias rm   <caller> <alias>
```

### `tide version`

Prints the tide logo banner followed by the version. Does not contact the server (there is no `pc` prefix).

```
tide version
```

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success, or no-op (e.g., `tide pull` with the local cache already current) |
| 1 | Backfill required — `tide apply` or `tide plan` returned a backfill-required class |
| 2 | Unknown subcommand passed to `tide` itself, **or** cross-caller breaking change returned by `apply`/`plan` |
| 3 | Operational error: parse/validation failure, network error, config error, or unknown plan class |
| 4 | Destructive change — the plan drops something that may hold data |

`tide apply` and `tide plan` share their code map exactly.

Code 2 covers two unrelated conditions: an unknown subcommand passed to `tide`, and a cross-caller breaking change. CI scripts that need to distinguish them must parse stderr.

Destructive plans exit 4 rather than joining code 2, because the two call for different responses. A breaking change is resolved by shipping the other callers' updates first; a destructive change is resolved by deciding whether losing those rows is intended. A CI script can gate on them separately:

```bash
tide plan
case $? in
  0) echo "additive — safe to merge" ;;
  1) echo "needs --backfill" ;;
  2) echo "blocked: breaks another caller" ;;
  4) echo "blocked: destroys data" ;;
  *) echo "tide failed"; exit 1 ;;
esac
```

Dropped objects are parked rather than deleted, and reaped after the retention window. `tide parked` lists what is held and until when.

## Cache layout

`tide pull` writes to a local cache at `.tide-cache/`:

```
.tide-cache/
├── schema/
│   └── <namespace>/
│       └── <entity>.atl
└── version.json
```

`tide list` and `tide show` fetch from the server on every invocation; they do not read the cache. Deleting `.tide-cache/` only affects the next `tide pull` (and the automatic pre-pull inside `tide apply`/`plan`).

## Output

Diagnostic and progress messages are prefixed `tide:` on stderr and stdout.

### `--format=json`

JSON output is the [proto3 canonical JSON mapping](https://protobuf.dev/programming-guides/json/) of the admin API messages defined in `atlantis/admin/v1/admin.proto`. `tidectl` uses the same encoding.

Four properties are worth knowing before you parse it:

- **64-bit integers are strings.** `"version": "7"`, not `"version": 7`. JSON numbers are IEEE-754 doubles and lose precision above 2^53; every 64-bit field here — schema versions, timestamps, row counts — is one you may compare for equality, so the mapping quotes them.
- **Enums are their full names.** A plan class is `"PLAN_CLASS_ADDITIVE"`, not `"additive"`; a check-drift kind is `"CHECK_DRIFT_KIND_LIVE_NOT_DECLARED"`. The human-readable table output still prints the short form.
- **Empty lists and maps are `[]` and `{}`**, never `null` or an absent key, so a consumer can iterate without a nil check. Fields declared `optional` in the proto are still omitted when unset, which is how "not set" stays distinguishable from "set to empty".
- **Field names are `snake_case`** — the proto field names, matching the `.atl` grammar and the SQL columns rather than protojson's default `lowerCamelCase`.

Whitespace is not stable. The encoder varies it between builds on purpose; compare parsed values, never bytes.

Fields that carry a JSON document — `args`, `state`, `diff`, `from_ir`, `to_ir`, `ir_snapshot` — are inlined as JSON, not base64. They are `bytes` on the wire because they are content-hash inputs and must stay byte-exact, but rendering them base64 in a terminal would make `tide diff --format=json | jq '.diff.additive'` useless. A payload that is not valid JSON is left as the base64 string rather than silently nulled.

> **Changed.** Before the admin API was defined in protobuf, each command marshalled a hand-written struct. Three things moved:
>
> - 64-bit integers were bare numbers; a script reading `version` as a number needs updating.
> - Plan classes were bare words; `"class": "additive"` is now `"class": "PLAN_CLASS_ADDITIVE"`, and `check_drift[].kind` changed the same way.
> - `tide backfill status`, `tide job status`, `tide job dead`, `tide workflow status`, and `tidectl adopt` used **PascalCase** keys (`PlanHash`, `JobID`, `Jobs`, `WorkflowID`, `CheckpointWritten`). They are now snake_case like every other command. A `jq '.PlanHash'` returns null; use `.plan_hash`.
>
> `tide plan --format=json` keeps its key names — it was already snake_case — and gains `entity_id` on each `index_drift` entry.
