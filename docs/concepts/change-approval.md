# Change approval

atlantis classifies every apply and looks the class up in policies you control. The rules say what applies unattended and what waits for a person: dropping a column, or changing a type another team reads, waits for someone to decide it.

## The gate

`tide apply` submits files; the server does the rest. It diffs the submitted `.atl` files against the current schema and classifies the result, reads the rules that govern that class — the class's policy, the caller's tier, protected entities, freeze windows — and then either applies the change or records the plan and refuses.

The check runs in the server's handler, inside the same advisory-locked transaction as the apply: a direct gRPC client holding `CAPABILITY_SCHEMA_APPLY` reaches the same gate `tide` does.

A refused apply stores the plan, exits 2, and the change waits in the console's Approvals queue.

## The four classes and their defaults

| Class | Default | What it covers |
|---|---|---|
| `additive` | applies unattended | New entities, new nullable fields, new indexes |
| `backfill_required` | **waits for approval** | A change that needs data written before it completes |
| `cross_caller_breaking` | **waits for approval** | A change another caller reads and would break on |
| `destructive` | **waits for approval** | Anything that removes something holding data |

You can relax any class from the console's Settings page; every edit is recorded with who made it.

## Missing and unrecognised rules

A class with no stored rule, a rule whose class name is not recognised, and a class outside the set a diff can produce all behave the same way: the change requires approval, by the default role. Removing a rule makes the gate stricter, never looser.

## Apply policy

The change policy is one rule per class, for the whole deployment. Each caller also carries an **apply policy** — a tier that says how much of what *it* submits may run unattended. The gate takes the most restrictive answer of the two; neither layer can widen what the other closed.

| Tier | What applies unattended |
|---|---|
| `sandbox_only` | Nothing. Plan, generate and rehearse remain available. |
| `always_ask` | Nothing; every class waits for a person. |
| `auto_safe` | Additive changes. The default. |
| `auto_verified` | Additive, and backfills covered by a passing rehearsal. |
| `auto_all` | Everything a passing rehearsal covers — except cross-caller breaking changes, which always wait: one caller's policy cannot consent on behalf of the callers it breaks. |

`sandbox_only` refuses; it does not queue. No approval makes such a caller one that applies; an admin raises the tier from the console's Callers page. Setting the tier is admin work, behind a re-authentication, and every change is recorded in `atlantis.policy_events`.

The platform can also cap every caller's tier with a deployment-wide floor. When a floor is set, the apply-policy dialog says so, a stored tier above the floor is kept, and the gate uses the cap.

## Rehearsal

The verified tiers turn on a **rehearsal**: the server clones the managed database — schema and rows, under one snapshot — into a disposable database, executes the migration's exact SQL there, rolls it back, and destroys the clone. The verdict reports what Postgres did:

| Verdict | Meaning |
|---|---|
| `pass` | The SQL completed against the real rows. |
| `pass_with_warnings` | Completed, with something worth reading first. |
| `fail_data` | Existing rows violate the change — the real apply would fail the same way. |
| `fail_structural` | The SQL itself does not run. |
| `unverified` | The rehearsal could not answer: too large, timed out, quota. Never treated as a pass. |

`tide rehearse` runs one from a caller's repository; the console runs one against any queued plan. A verdict binds to the exact content it rehearsed — the same identity approvals bind to — and the gate consumes it only within an hour; older is the same as absent. Data can still drift inside that hour: the apply's own transactional DDL validates the real rows, so a stale pass produces a failed apply and a rollback, not a partial migration.

Rehearsing is a capability of its own, in no default bundle: the clone holds every caller's rows, and a rehearsal's failure text is a row-value surface. An admin grants it per caller, beside the tier. Listings redact the lines that embed row values; the full record is behind the same capability that can run one.

## Protected entities and freeze windows

Two more gates compose with the class rules, both admin-set from the console's Settings page and both recorded in `atlantis.policy_events`:

- **Protected entities** — per-entity approval floors: `payments.Invoice`, or `payments.*` for a namespace. A change touching one waits for a person at every tier, and an `admin_only` floor demands an admin's decision whatever role the class rule names. When the diff cannot say what it touches, the gate assumes it touches one.
- **Freeze windows** — absolute intervals during which matching classes do not apply. Approving stays possible during a freeze; the approved change queues behind the window, and `tide apply --wait-for-approval` picks it up when the window lifts.

## Who approves

Each class names an `approver_role`, which is a console role: `admin` or `developer`. The default is `admin`, and the set is closed — a rule naming anything else is refused, because a mistyped role is a class nobody can ever decide.

The server cannot authenticate a console user — that identity system belongs to the console — so the console tells the server which role the approver held. The server then re-checks that claim against the class's `approver_role` before writing anything. It cannot prove the console asserted the role honestly, but it can refuse a role the class does not name, so a middleware bug cannot quietly produce an under-privileged approval.

## Overrides

An admin can approve a plan **past** its gates — a freeze window, a protected-entity floor, the self-approval refusal below. An override is an approval with extra ceremony: it requires the admin role, a re-authentication, and a reason, and it is recorded as `override` on the plan and in both audit ledgers. It cannot waive a capability, the `sandbox_only` tier, or the binding of an approval to its exact content.

Use an override for the incident that cannot wait. Every override is visible in the audit log and on the plan itself.

## Self-approval

The identity that applies is a machine certificate's common name. The identity that approves is a console user. The capability that permits approving, `CAPABILITY_SCHEMA_APPROVE`, is in no bundle a caller receives when it registers, and none of the identities the platform seeds holds it together with `CAPABILITY_SCHEMA_APPLY`.

There is a person-level rule beside the identity split. An apply can carry an attribution — which human asked for this — and a plan records it. A decision from the same person is refused: a request is not approved by whoever made it. The attribution is not authenticated, so this stops the honest loop rather than a determined liar, and an admin's override passes it with a reason on record. A plan whose apply named nobody shows as *unattributed* in the queue, which is what an unattended pipeline's request is.

## What an approval is attached to

An approval is granted against **the exact file contents you reviewed**, not against a plan id or a set of paths. Submitting different content under the same paths produces a different plan, and inherits nothing.

It is also scoped to what the submitting caller actually depends on: the members it owns, the members it reads, and everything in the diff. An unrelated team applying their own schema does not expire your approval. See [schema versioning](schema-versioning.md) for how that scoping is derived.

## Expiry

A plan stays actionable for **seven days**, whether it is waiting or already approved.

An approval is a statement about a schema somebody read at a particular moment. Left open-ended it becomes a standing permission that outlives the reasoning behind it.

Expiry is evaluated on every read rather than by a background job, so nothing about it depends on a sweeper having run.

## Rollback

`tide rollback` returns the schema to a state that already passed this gate once, and it is the incident-response path, so it takes no second person. It requires `CAPABILITY_OPERATOR`, is recorded like any other schema version, and an auditor can see it was single-party.

## Related

- [Approve a schema change](../guides/approve-a-schema-change.md) — the recipe, from `tide plan` to re-running the apply.
- [Schema versioning](schema-versioning.md) — what makes a plan go stale.
- [`tide` reference](../reference/cli-tide.md) — exit codes, and `--wait-for-approval`.
