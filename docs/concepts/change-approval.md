# Change approval

Some schema changes should not reach a production database because a pipeline decided they could. Dropping a column, or changing a type another team reads, is a decision — and the moment it becomes routine is the moment nobody is deciding it.

atlantis classifies every apply and looks the class up in a policy you control. The rule says whether that class applies unattended or waits for a person.

## The gate

`tide apply` submits files; the server does the rest:

1. Diff the submitted `.atl` files against the current schema and classify the result.
2. Read the rule for that class from `atlantis.change_policy`.
3. Apply it, or record the plan and refuse.

The check runs inside the same advisory-locked transaction as the apply, alongside the drift checks. It is not the CLI's decision: a direct gRPC client holding `CAPABILITY_SCHEMA_APPLY` reaches the same gate, because the gate is in the handler rather than in `tide`.

A refused apply is not an error. The plan is stored, `tide apply` exits 2, and the change waits.

## The four classes and their defaults

| Class | Default | What it covers |
|---|---|---|
| `additive` | applies unattended | New entities, new nullable fields, new indexes |
| `backfill_required` | depends on install age — see below | A change that needs data written before it completes |
| `cross_caller_breaking` | **waits for approval** | A change another caller reads and would break on |
| `destructive` | **waits for approval** | Anything that removes something holding data |

`backfill_required` is the one default that is not the same everywhere. A **fresh install** gets "waits for approval"; an **existing deployment upgrading** gets "applies unattended".

The asymmetry is deliberate. Breaking and destructive changes were refused outright before this policy existed, so seeding them to "waits" is strictly *more* permissive than what the deployment had — nothing that used to work stops working. Backfill-required changes did apply unattended, so seeding them to "waits" would be a new gate over an existing workflow, appearing at upgrade time with no warning. New installs get the stricter default because they have no workflow to interrupt.

If you want the strict rule on an upgraded deployment, set it yourself in the console — that way it is a decision somebody made, on a day they chose.

## It fails closed

Three things all mean the same thing: a class with no row, a row whose class name nobody recognises, and a class outside the set a diff can produce. All of them require approval, by the default role.

They are one rule on purpose. Every way of failing to find a rule fails the same way, and the safe way. A deployment where somebody deleted a row, or mistyped one, is a deployment where **more** applies stop for a human — never one where fewer do.

Deleting rows from `atlantis.change_policy` makes a deployment stricter. It is not a way to turn the gate off.

## The caller's own ceiling: apply policy

The change policy is one rule per class, for the whole deployment. Each caller also carries an **apply policy** — a tier that says how much of what *it* submits may run unattended. The gate takes the most restrictive answer of the two; neither layer can widen what the other closed.

| Tier | What applies unattended |
|---|---|
| `sandbox_only` | Nothing. Plan, generate and rehearse remain available. |
| `always_ask` | Nothing; every class waits for a person. |
| `auto_safe` | Additive changes. The default. |
| `auto_verified` | Additive, and backfills covered by a passing rehearsal. |
| `auto_all` | Everything a passing rehearsal covers — except cross-caller breaking changes, which always wait: one caller's policy cannot consent on behalf of the callers it breaks. |

`sandbox_only` refuses rather than queues. There is no approval that makes such a caller one that applies; an admin raises the tier from the console's Callers page. Setting the tier is admin work, behind a re-authentication, and every change is recorded in `atlantis.policy_events`.

A deployment can also set a floor: `ATLANTIS_APPLY_POLICY_FLOOR` caps every caller's tier, and a floor that names no tier refuses to boot.

## Rehearsal: proof against the real data

The verified tiers turn on a **rehearsal**: the server clones the managed database — schema and rows, under one snapshot — into a disposable database, executes the migration's exact SQL there, rolls it back, and destroys the clone. The verdict is what Postgres actually did, not a prediction of it:

| Verdict | Meaning |
|---|---|
| `pass` | The SQL completed against the real rows. |
| `pass_with_warnings` | Completed, with something worth reading first. |
| `fail_data` | Existing rows violate the change — the real apply would fail the same way. |
| `fail_structural` | The SQL itself does not run. |
| `unverified` | The rehearsal could not answer: too large, timed out, quota. Never treated as a pass. |

`tide rehearse` runs one from a caller's repository; the console runs one against any queued plan. A verdict binds to the exact content it rehearsed — the same identity approvals bind to — and the gate consumes it only within an hour, so a stale pass is simply not found. Data can still drift inside that hour; the apply's own transactional DDL is the final validator, so the worst a stale pass costs is a failed apply, never a half-applied one.

Rehearsing is a capability of its own, in no default bundle: the clone holds every caller's rows, and a rehearsal's failure text is a row-value surface. An admin grants it per caller, beside the tier. Listings redact the lines that embed row values; the full record is behind the same capability that can run one.

## Protected entities and freeze windows

Two more gates compose with the class rules, both admin-set from the console's Settings page and both recorded in `atlantis.policy_events`:

- **Protected entities** — per-entity approval floors: `payments.Invoice`, or `payments.*` for a namespace. A change touching one waits for a person at every tier, and an `admin_only` floor demands an admin's decision whatever role the class rule names. When the diff cannot say what it touches, the gate assumes it touches one.
- **Freeze windows** — absolute intervals during which matching classes do not apply. Approving stays possible during a freeze — the decision is not the action — so an approved change queues behind the window and `tide apply --wait-for-approval` picks it up when the window lifts.

## Who approves

Each class names an `approver_role`, which is a console role: `admin` or `developer`. The default is `admin`, and the set is closed — a rule naming anything else is refused, because a mistyped role is a class nobody can ever decide.

The server cannot authenticate a console user — that identity system belongs to the console — so the console tells the server which role the approver held. The server then re-checks that claim against the class's `approver_role` before writing anything. It cannot prove the console asserted the role honestly, but it can refuse a role the class does not name, so a middleware bug cannot quietly produce an under-privileged approval.

## Overrides

An admin can approve a plan **past** its gates — a freeze window, a protected-entity floor, the self-approval refusal below. An override is an approval with extra ceremony, never a bypass: it requires the admin role, a re-authentication, and a reason, and it is recorded as `override` on the plan and in both audit ledgers. It cannot waive a capability, the `sandbox_only` tier, or the binding of an approval to its exact content — those are edited, not overridden.

The override is what keeps the gates honest. A gate with no sanctioned way past it gets switched off deployment-wide the first time it is inconvenient; a gate with a loud, recorded exception survives the incident that tests it.

## Nobody approves their own change

This is structural, not a convention.

The identity that applies is a machine certificate's common name. The identity that approves is a console user. The capability that permits approving, `CAPABILITY_SCHEMA_APPROVE`, is in neither bundle a caller receives when it registers — and the console, which holds it, was deliberately never granted `CAPABILITY_SCHEMA_APPLY`.

So the two identities cannot be the same one. A test fails the build if somebody adds the approve capability to a registration bundle to make a pipeline stop asking.

There is a person-level rule beside the structural one. An apply can carry an attribution — which human asked for this — and a plan records it. A decision from the same person is refused: a request is not approved by whoever made it. The attribution is not authenticated, so this stops the honest loop rather than a determined liar, and an admin's override passes it with a reason on record. A plan whose apply named nobody shows as *unattributed* in the queue, which is what an unattended pipeline's request is.

## What an approval is attached to

An approval is granted against **the exact file contents you reviewed**, not against a plan id or a set of paths. Submitting different content under the same paths produces a different plan, and inherits nothing.

It is also scoped to what the submitting caller actually depends on: the members it owns, the members it reads, and everything in the diff. An unrelated team applying their own schema does not expire your approval. See [schema versioning](schema-versioning.md) for how that scoping is derived.

## Approvals expire

A plan stays actionable for **seven days**, whether it is waiting or already approved.

An approval is a statement about a schema somebody read at a particular moment. Left open-ended it becomes a standing permission that outlives the reasoning behind it — the familiar shape being an approval granted in April and used in September, against a schema that has moved underneath it in every way except the ones the token happens to cover.

Expiry is evaluated on every read rather than by a background job, so nothing about it depends on a sweeper having run.

## Rollback is not gated

`tide rollback` returns the schema to a state that already passed this gate once. It requires `CAPABILITY_OPERATOR` rather than `CAPABILITY_SCHEMA_APPLY`, and it is what somebody reaches for at three in the morning.

Requiring a second person there is how a gate gets switched off permanently after the first incident. The rollback is recorded, and an auditor can see it was single-party.

## Related

- [Approve a schema change](../guides/approve-a-schema-change.md) — the recipe, from `tide plan` to re-running the apply.
- [Schema versioning](schema-versioning.md) — what makes a plan go stale.
- [`tide` reference](../reference/cli-tide.md) — exit codes, and `--wait-for-approval`.
