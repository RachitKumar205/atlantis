# Schema versioning

Every schema change creates a numbered version in an append-only registry.
The `.atl` files in your repositories are authoritative for authoring; the
registry records what is deployed, and when, and by whom.

Each version stores the full IR snapshot of the merged schema, a
structural diff from the previous version, the generated SQL, the caller
identity, a content hash — sha256 of the canonical IR JSON, the version's
identity — and a timestamp. The event that wrote it is recorded too:
`apply`, `rollback`, `adopt`, or `seed`. How a new checkpoint reaches the
serving path is covered in
[How atlantis runs your schema](how-atlantis-runs-your-schema.md).

## When a plan goes stale

`tide apply` refuses a plan when anything it depends on changed after
`tide plan` ran, so two applies racing cannot lose one another's work.
The refused plan is superseded; re-run `tide plan` and apply again.

What counts as a dependency is scoped to the caller:

- the caller's own schema,
- an entity it references, a job its workflows run, or an entity its
  queries and procedures touch,
- anything the migration is about to emit DDL for.

A change to another caller's schema that meets none of those leaves the
plan valid: an unrelated service shipping a column does not send everybody
else back to `tide plan`.

Two more records bind to the same plan identity, each with its own clock.
An **approval** covers the exact files and SQL a reviewer saw and stays
actionable until the plan's seven-day expiry; content that moves gets a
new plan id, and the old request is superseded. A **rehearsal verdict**
covers the exact content it executed and is consumed by the apply gate
only within an hour — old enough to be stale is the same as absent. See
[Change approval](change-approval.md).

## Append-only history

Versions are never deleted and never mutated; version numbers always
increase. A rollback creates a new version whose IR snapshot matches the
target version, and whose diff describes the reversal.

```
$ tide history --limit=3

  Schema History

  ● v9  rollback  atlantis-console  just now
  │      +5 change(s)
  │
  ● v8  apply     vendor     3h ago
  │      +1 change(s)
  │
  ● v7  apply     vendor     Jun 14, 09:12
         +4 change(s)

  … more versions available — use --limit
```

Version 9 is a new version, not a deletion of versions 7 and 8. A `seed`
version — the registry's starting record — renders as ◌; every other
event renders as ●.

## Per-entity lineage

The registry tracks which caller introduced each entity and each field.
`tide blame` resolves a single entity to its per-field attribution:

```
$ tide blame consumer.Order
Blame: consumer.Order

FIELD                    INTRODUCED BY    AT       MODIFIED BY      AT       STATUS
(entity)                 consumer         1        consumer         1        active
id                       consumer         1        consumer         1        active
user_id                  consumer         1        consumer         1        active
item_ids                 consumer         1        consumer         1        active
total                    consumer         3        consumer         3        active
shipping_label           vendor           5        vendor           5        active
```

`tide owners` shows ownership at the entity level — which caller declared
each entity in the merged schema:

```
$ tide owners
ENTITY                           OWNER            SINCE   FIELDS
auth.User                        auth             v1      6
auth.Session                     auth             v1      4
auth.ApiKey                      auth             v4      3
consumer.Order                   consumer         v1      5
consumer.Invoice                 consumer         v3      4
vendor.Product                   vendor           v2      8
vendor.Variant                   vendor           v2      6
vendor.Collection                vendor           v2      5
vendor.VendorImport              vendor           v5      3
```

## Registry vs `.atl` files

The `.atl` files in caller repos are the authoring surface: you edit them,
review them in PRs, and `git blame` them. The version registry is the
deployment record: what was applied to the server and when.

The analogy to git:

| git | atlantis |
|---|---|
| Source files | `.atl` files in caller repos |
| Commits | Schema versions |
| Remote repository | Version registry on the server |
| `git log` | `tide history` |
| `git diff` | `tide diff` |
| `git blame` | `tide blame` |

The `.atl` files can diverge temporarily from the registry — a PR merged
before `tide apply` runs. The registry reflects what is deployed, and
`tide plan` shows the delta between the local files and the registry's
latest version.

## Version diffs

`tide diff` compares any two versions structurally:

```
$ tide diff 5 7
diff v5 → v7

─── vendor.VendorImport (new entity)
  + vendor_id       varchar(7)  not null
  + import_strategy varchar(20) not null
  + status          varchar(10) not null

─── consumer.Order
    id              varchar(8) primary
  + shipping_label  text
  - total           int not null
  + total           bigint not null

─── consumer.LegacyCart (removed)

+4 additive  ~1 backfill  ✗1 destructive
```

The diff is entity-level and field-level, not line-level: each changed
entity gets a header, `+` and `-` mark added and removed fields, a
modified field prints as its old line then its new one, and the last line
counts changes per plan class. The diff covers type, constraint, and
composite `unique by` changes, and custom query and procedure additions,
removals, and edits.

## Related

- [Schema as code](schema-as-code.md) — why `.atl` files are authoritative for authoring
- [How atlantis runs your schema](how-atlantis-runs-your-schema.md) — the checkpoint and the apply path
- [Schema history](../guides/schema-history.md) — step-by-step usage of history, diff, blame, owners, and rollback
