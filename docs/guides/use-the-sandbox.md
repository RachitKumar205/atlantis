# Use the sandbox

Boot an isolated test database holding your organisation's schema, seed it,
capture checkpoints, and diff two states — all through the console, no
`tide` invocation needed.

A sandbox holds the schema and only the rows you put in it, never
production data. To prove a migration against real rows, rehearse the plan
from the Approvals page instead — see
[Change approval](../concepts/change-approval.md).

## Prerequisites

- The `developer` or `admin` role — viewers cannot boot sandboxes.
- Each user holds up to 100 sandboxes at a time.

## Boot a sandbox

Navigate to `/sandbox`. The left rail has two backend options: **In-memory** (pure-Go simulator, sub-millisecond boot, supports state primitives) and **Postgres** (real PG child process, a few seconds to boot, SQL only). See [the sandbox concept page](../concepts/sandbox.md) for the full split.

Click `+ In-memory` for this walkthrough. The status strip at the top of the page shows the backend, a short opaque ID, the schema hash, and the boot time. After the first operation, it also shows server-side microsecond timings for the most recent query, checkpoint, fork, and seed.

The page has five tabs:

| Tab | What it does |
|---|---|
| Tables | Browse the schema; sample 10 rows of an entity. |
| Seed data | Insert N synthetic rows generated from each column's declared type. |
| SQL | Run arbitrary SQL. SELECT/RETURNING returns rows; everything else returns rows-affected. |
| Compare | Diff added / removed / modified rows per table between two checkpoints. |
| Forks | Clone the sandbox into N independent copies. |

## Capture a baseline checkpoint

In the left rail, find the **Checkpoints** section and click **+ Capture**. A row appears: `just now · captured in NNN µs`. This is the empty schema; the diff later compares against it.

## Seed some rows

There are two ways to populate data.

**Manually via SQL.** Open the SQL tab and insert against any table:

```sql
INSERT INTO "consumer"."accounts" ("id", "email")
VALUES ('user_001', 'alice@example.com'),
       ('user_002', 'bob@example.com');
```

**Synthetically via Seed data.** Pick an entity from the dropdown, click `+ 100 rows` / `+ 1,000` / `+ 10,000`. Values are produced from each column's declared type — email-shaped strings, monotonic timestamps, unit-norm vectors. If you booted the sandbox with **Deterministic** checked, the values are reproducible from the seed.

Capture a second checkpoint after seeding.

## Mutate further

Run an UPDATE and a DELETE so the diff has something to show:

```sql
UPDATE "consumer"."accounts"
SET "email" = 'alice@new.example.com'
WHERE "id" = 'user_001';
```

```sql
DELETE FROM "consumer"."accounts"
WHERE "id" = 'user_002';
```

Capture a third checkpoint.

## Compare

Switch to the **Compare** tab. Pick the second checkpoint (after seeding) and the third from the two dropdowns, then click **Compare**. A table appears:

| Table | Added | Removed | Modified |
|---|---|---|---|
| `consumer.accounts` | 0 | 1 | 1 |

The removed row is `user_002` (deleted) and the modified row is
`user_001` (email changed).

The latency for the compare itself (microseconds) shows next to the button.

## Restore

To rewind to a previous state, click the restore icon next to any checkpoint row in the left rail. The sandbox state is replaced — intermediate writes are discarded.

Restore preserves the checkpoint timeline, so you can move back and forth between captured states.

## Verify

Restore the second checkpoint (after seeding, before the update), then run:

```sql
SELECT "email" FROM "consumer"."accounts" WHERE "id" = 'user_001';
```

The row returns `alice@example.com` — the pre-update value — and
`user_002` is back.

## Fork

The **Forks** tab clones the current state into N independent sandboxes. Each fork has its own checkpoint history and its own destruction lifecycle. Enter a count (1–10), click `Fork into N`, and child sandboxes appear in the parent's tree visualisation. Click one to switch to it. Forking counts against your per-user sandbox limit.

Fork is available on the in-memory backend only. With a Postgres-backed sandbox active, the Fork button is disabled with an inline explanation.

## Snapshot

To persist a sandbox's state to a portable byte blob, use the **Snapshot** button in the status strip. The browser downloads a file containing the schema signature and every row. Restoring a snapshot happens over the [HTTP API](../reference/sandbox-api.md) (`PUT /api/sandbox/{id}/snapshot`) — the console has no upload control — and requires the same schema signature; a mismatch is rejected.

Snapshot is available on the in-memory backend only.

## Destroy

The **Destroy** button on the status strip removes the sandbox and frees its memory. Sandboxes also auto-evict after 30 idle minutes, so leaving a tab open without activity silently drops them, and a console restart drops all of them — nothing in a sandbox is durable.

## Test a custom query before `tide apply`

A `query` or `procedure` body is SQL. Boot a sandbox, seed enough data, paste the body into the SQL tab, and verify the result shape before submitting the migration.

The typical flow:

1. Boot an **in-memory** sandbox.
2. Use **Seed data** to populate the entities the query reads, with enough rows that `WHERE` filters and `ORDER BY` change the result.
3. Open the `.atl` file with the `query` block; copy the SQL body.
4. Paste it into the SQL tab. Replace `$name` parameters with positional `$1, $2, …` — the simulator parses positional placeholders only; `tide apply` does that rewrite for the real server.
5. Click **Execute** and inspect the rows.

If the query uses constructs the simulator's executor doesn't model — joins, CTEs, GROUP BY, window functions other than `COUNT(*) OVER ()` — switch to a **Postgres** backend sandbox at boot time. That backend runs a real Postgres process against the emitted DDL, minus pgvector and hypertable clauses (`vector(N)` becomes `BYTEA`, HNSW indexes and hypertable conversion are stripped).

## Drive the sandbox from an agent

Everything the console does travels over the JSON HTTP surface at
`/api/sandbox/*`, so a script or a coding agent can run the same loop
without a browser: boot, seed, execute SQL, checkpoint, compare, restore,
fork. The in-memory backend's sub-millisecond boot and `O(tables)`
checkpoints suit try-N-then-rewind loops; the per-user limit and idle
eviction apply the same as in the UI. The
[sandbox HTTP API](../reference/sandbox-api.md) lists every route with its
request and response shapes.

## Deep links

Other console pages link into the sandbox:

- The schema browser's "Try in sandbox" button on an entity detail opens `/sandbox?focus=ns.Entity&boot=sim` — it auto-boots an in-memory sandbox with the Tables tab pre-selecting that entity.
- A pending operation's "Preview in sandbox" opens `/sandbox?boot=sim` and auto-boots a fresh in-memory sandbox.

## Related

- [The sandbox](../concepts/sandbox.md) — what the two backends are and how the state model works.
- [Sandbox SQL coverage](../reference/sandbox-sql.md) — what the in-memory executor accepts.
- [Sandbox HTTP API](../reference/sandbox-api.md) — programmatic surface for agents and CI.
