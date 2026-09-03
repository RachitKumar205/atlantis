# Sandbox HTTP API

The sandbox's JSON-over-HTTP surface at `/api/sandbox/*`: the routes the
console's Sandbox page uses, and the supported programmatic surface for
scripts and agent loops that drive sandboxes directly.

A sandbox holds the organisation's schema and only the rows its owner puts
in it — never production data.

| Route | Method | Purpose |
|---|---|---|
| `/api/sandbox` | POST | Boot a sandbox |
| `/api/sandbox` | GET | List your sandboxes |
| `/api/sandbox/{pub_id}` | DELETE | Destroy a sandbox |
| `/api/sandbox/{pub_id}/sql/exec` | POST | Run a non-returning statement |
| `/api/sandbox/{pub_id}/sql/query` | POST | Run a row-returning query |
| `/api/sandbox/{pub_id}/inspect/*` | GET/POST | Catalog, describe, sample, find, diff (`sim` only) |
| `/api/sandbox/{pub_id}/mark` | POST | Capture a checkpoint (`sim` only) |
| `/api/sandbox/{pub_id}/restore` | POST | Rewind to a checkpoint (`sim` only) |
| `/api/sandbox/{pub_id}/fork` | POST | Clone into N sandboxes (`sim` only) |
| `/api/sandbox/{pub_id}/snapshot` | GET/PUT | Download / restore a portable blob (`sim` only) |
| `/api/sandbox/{pub_id}/fixtures/bulk` | POST | Insert synthetic rows (`sim` only) |

Timing: JSON responses from the sandbox runtime carry a `t_server_us`
field; binary responses and `204` responses carry an `X-Atl-Server-Us`
header instead. The boot response carries `boot_ms`, and the list and
destroy responses carry no timing at all.

## Authentication and ownership

Every `/api/sandbox/*` route requires an authenticated console session
cookie; a programmatic client reuses a session obtained by signing in to
the console. On mutating routes the server compares the `Origin` header
against the console host and rejects a mismatch; a request that sends no
`Origin` header passes. There is no separate token to fetch.

Booting and forking require the `developer` or `admin` role; the other
routes act only on sandboxes their caller owns, so a member whose role
cannot boot owns no sandbox for the remaining routes to act on.

Each sandbox is minted a 128-bit opaque `pub_id` at boot. A request whose
`pub_id` doesn't resolve to the caller's owner record returns `404` — the
same response shape as "doesn't exist," so enumeration is blocked.

## Limits

| Limit | Value |
|---|---|
| Concurrent sandboxes per user, per organisation | 100 |
| Idle TTL before eviction | 30 minutes |
| `PUT /snapshot` request body | 256 MiB |

A boot beyond the per-user limit returns `429 Too Many Requests`. A
snapshot body beyond 256 MiB returns `400` with a read error.

## Lifecycle endpoints

### `POST /api/sandbox`

Boots a sandbox. Request fields, all optional:

| Field | Values | Default |
|---|---|---|
| `backend` | `"sim"` or `"embedded"` | `"sim"` |
| `determinism` | `"strict"` | off |
| `seed` | integer | none |

```json
{ "backend": "sim", "determinism": "strict", "seed": 42 }
```

An empty body boots the `sim` (in-memory) backend. The embedded backend —
a real Postgres process — ignores `determinism`.

Response `200`:

```json
{
  "pub_id": "5fa3b21c…",
  "backend": "sim",
  "boot_ms": 0,
  "schema_version": "f1f9b85…",
  "entity_count": 37
}
```

Errors: `429` (per-user limit hit), `502` (admin RPC for current IR
failed), `400` (validation).

### `GET /api/sandbox`

Lists the caller's active sandboxes. List requests do **not** touch the
activity timestamp, so polling here doesn't keep an idle sandbox alive.
The response carries no timing field.

Response `200`:

```json
{
  "sandboxes": [
    {
      "pub_id": "5fa3b21c…",
      "backend": "sim",
      "schema_version": "f1f9b85…",
      "created_at": "2026-06-04T18:00:00Z",
      "last_active": "2026-06-04T18:12:30Z",
      "boot_ms": 0
    }
  ]
}
```

### `DELETE /api/sandbox/{pub_id}`

Destroys a sandbox, freeing memory and (for embedded) the on-disk Postgres
data directory. Response `204`, no timing header.

## SQL execution

### `POST /api/sandbox/{pub_id}/sql/exec`

Runs a non-returning statement (UPDATE / DELETE / INSERT without RETURNING).

Request:

```json
{ "sql": "UPDATE \"s\".\"t\" SET \"a\" = $1 WHERE \"id\" = $2",
  "args": [1, "user_001"] }
```

Response `200`:

```json
{ "rows_affected": 1, "t_server_us": 87 }
```

### `POST /api/sandbox/{pub_id}/sql/query`

Runs a query that returns rows (SELECT, or INSERT with RETURNING on the
`sim` backend; the embedded backend accepts any row-returning statement).

Response `200`:

```json
{
  "rows": [
    { "id": 1, "email": "a@x" },
    { "id": 2, "email": "b@x" }
  ],
  "t_server_us": 143
}
```

`SELECT *` returns the catalog's declared column names, not a literal `*`.

`pub_id` accepts both backends. For SQL the `sim` backend covers the
subset documented in [sandbox-sql.md](sandbox-sql.md); the embedded
backend runs a real Postgres process.

## Inspect (`sim` only)

Inspect endpoints are `sim`-only — Catalog, Describe, Sample, Find, and
Diff. A call against an embedded sandbox returns `400 Bad Request` with
`<feature> is in-memory only; boot a Sim sandbox to use it`.

### `GET /api/sandbox/{pub_id}/inspect/catalog`

Returns the qualified entity names the catalog has registered.

```json
{ "entities": ["consumer.accounts", "vendor.vendors", …], "t_server_us": 4 }
```

### `GET /api/sandbox/{pub_id}/inspect/describe?q={qualified}`

Returns the column shape + row count for one entity.

```json
{
  "qualified": "consumer.accounts",
  "schema": "consumer", "name": "accounts",
  "columns": [
    { "name": "id", "kind": "string", "nullable": false },
    { "name": "email", "kind": "string", "nullable": false }
  ],
  "primary_key": ["id"],
  "row_count": 0,
  "t_server_us": 6
}
```

`404` when the qualified name isn't registered.

### `GET /api/sandbox/{pub_id}/inspect/sample?q={qualified}&n={N}`

Returns up to N rows. Default N is 5 when the parameter is omitted.

Response `200`:

```json
{ "rows": [ { "id": "a1", "email": "a@x" } ], "t_server_us": 9 }
```

### `POST /api/sandbox/{pub_id}/inspect/find`

Body: `{"qualified": "...", "predicates": [{"column": "...", "op": "=", "value": ...}, ...]}`. Predicates are AND-ed. Supported `op` values are `=`, `!=`, `<`, `<=`, `>`, `>=`, `is null`, `is not null`.

### `POST /api/sandbox/{pub_id}/inspect/diff`

Body: `{"before_mark_id": "1", "after_mark_id": "2"}`. Returns per-table counts:

```json
{
  "tables": {
    "consumer.accounts": { "added": 3, "removed": 1, "modified": 0 }
  },
  "t_server_us": 12
}
```

Empty `tables` (`{}`) means no differences. `404` when either mark id is
unknown to this sandbox.

## Checkpoint and restore (`sim` only)

### `POST /api/sandbox/{pub_id}/mark`

Captures a checkpoint. Empty body. Response `201`:

```json
{ "mark_id": "1", "t_server_us": 142 }
```

Mark ids are sequential integers rendered in base 36, scoped to the
sandbox.

### `POST /api/sandbox/{pub_id}/restore`

Rewinds to a previously captured checkpoint. Body: `{"mark_id": "1"}`.
Response `204` with the timing on the `X-Atl-Server-Us` header.

`404` when the mark id is unknown — including a mark captured before the
sandbox host restarted.

## Fork (`sim` only)

### `POST /api/sandbox/{pub_id}/fork`

Clones the current state into N independent sandboxes. Body: `{"n": 3}`.
Each child gets a fresh `pub_id`, becomes the caller's own (the per-user
limit applies to the total), and has its own checkpoint history.

Response `201`:

```json
{ "ids": ["abc…", "def…", "ghi…"], "backend": "sim", "t_server_us": 4 }
```

`400` when called on an embedded-backed sandbox. `429` when minting N
children would push the caller above the per-user limit.

## Snapshot (`sim` only)

### `GET /api/sandbox/{pub_id}/snapshot`

Returns the sandbox's portable byte blob. `Content-Type:
application/octet-stream`, timing on the `X-Atl-Server-Us` header. The
blob encodes a schema signature alongside the rows; restore against a
mismatched schema is rejected.

### `PUT /api/sandbox/{pub_id}/snapshot`

Restores a sandbox's state from a previously-downloaded blob. The body is
the raw blob bytes (octet-stream), at most 256 MiB.

Response `204` with the timing on the `X-Atl-Server-Us` header.

`400` when the body exceeds the size limit or the blob's schema signature
doesn't match the sandbox's current catalog.

## Seed data (`sim` only)

### `POST /api/sandbox/{pub_id}/fixtures/bulk`

Generates and inserts N synthetic rows for an entity. Body:

```json
{ "qualified": "consumer.accounts", "n": 1000, "seed": 42, "pk_start": 1 }
```

`seed` and `pk_start` are optional. Values are produced from each column's
declared type — email-shaped strings, monotonic timestamps, unit-norm
vectors. When the sandbox was booted with `determinism: "strict"`, the
values are reproducible from `seed`.

Response `200`:

```json
{ "inserted": 1000, "t_server_us": 21834 }
```

## Status codes

| Code | Meaning |
|---|---|
| `200` | Read, single-statement write, or boot succeeded. |
| `201` | Mark captured or fork minted. |
| `204` | Destroy, restore, or snapshot upload succeeded with no body. |
| `400` | Validation, `sim`-only feature called on embedded, unsupported SQL, oversize or schema-mismatched snapshot. |
| `404` | Sandbox or mark not found, or sample/describe target absent. Returned for ownership mismatches too. |
| `405` | Wrong method on a route. |
| `429` | Per-user sandbox limit reached (boot or fork). |
| `500` | Internal error or marshal failure. |
| `502` | Admin RPC for the current IR failed during boot. |

## Related

- [The sandbox](../concepts/sandbox.md) — backend split, state model.
- [Sandbox SQL coverage](../reference/sandbox-sql.md) — what the `sim` executor runs.
- [Use the sandbox](../guides/use-the-sandbox.md) — console walkthrough.
