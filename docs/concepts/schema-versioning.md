# Schema versioning

Every schema change creates a numbered version in an append-only registry.
The `.atl` files in your repositories are authoritative for authoring; the
registry records what is deployed, and when, and by whom.

Each version stores the full IR snapshot of the merged schema, a
structural diff from the previous version, the generated SQL, the caller
identity, a content hash, and a timestamp. The hash is sha256 of the
canonical IR JSON, and it is the version's identity. The event that wrote
the version is recorded too: `apply`, `rollback`, `adopt`, or `seed`, the
registry's starting record. How a new checkpoint reaches the serving path
is covered in [The apply path](how-atlantis-runs-your-schema.md).

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
plan valid.

Two more records bind to the same plan identity, each with its own clock.
An **approval** covers the exact files and SQL a reviewer saw and stays
actionable until the plan's seven-day expiry; content that moves gets a
new plan id, and the old request is superseded. A **rehearsal verdict**
covers the exact content it executed and is consumed by the apply gate
only within an hour. See [Change approval](change-approval.md).

## Append-only history

Versions are never deleted and never mutated; version numbers always
increase. A rollback adds a version whose IR snapshot matches the target
version and whose diff describes the reversal, and it removes none.

## Per-entity lineage

The registry records which caller introduced each entity and each field,
so you can attribute a column you did not write. `tide blame` resolves one
entity to its per-field attribution, and `tide owners` reports ownership
at the entity level. [Inspect schema history](../guides/schema-history.md)
shows both, with their output.

## Version diffs

`tide diff` compares any two versions structurally rather than by line.
Each changed entity gets its own section, `+` and `-` mark added and
removed fields, a modified field prints as its old line then its new one,
and a trailing line counts the changes per plan class. The diff covers
type, constraint, and composite `unique by` changes, and custom query and
procedure additions, removals, and edits.

## Registry against `.atl` files

The `.atl` files in caller repos are the authoring surface: you edit them,
review them in PRs, and `git blame` them. The version registry is the
deployment record: what was applied to the server and when.

The analogy to git:

| git | atlantis |
|---|---|
| Source files | `.atl` files in caller repos |
| Commits | Schema versions |
| Remote repository | Version registry on the server |

The `.atl` files can diverge temporarily from the registry, as when a PR
merges before `tide apply` runs. The registry reflects what is deployed,
and `tide plan` shows the delta between the local files and the registry's
latest version.

## Related

- [Schema as code](schema-as-code.md) — why `.atl` files are authoritative
  for authoring.
- [The apply path](how-atlantis-runs-your-schema.md) — the checkpoint and
  how it reaches the serving path.
- [Inspect schema history](../guides/schema-history.md) — history, diff,
  blame, owners, and rollback at the terminal.
