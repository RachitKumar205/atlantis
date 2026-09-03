# The console

The console is the browser view of your organisation's schema, its
history, and the controls around applying it. It shows state and is where
approvals are decided; schema changes themselves travel through `tide`
from a repository — there is no schema editor.

You sign in at Atlantis Cloud; the console holds no accounts of its own.
Every page is scoped to one organisation — an account in more than one
switches between them in the sidebar — and your role in it (`admin`,
`developer`, or `viewer`) decides what the console lets you do.

## Pages

| Page | What it shows and does |
|---|---|
| Schema | The merged schema: namespaces, entities, fields, keys, foreign keys, and the physical table names. Entities with changes waiting for approval carry a marker linking to Approvals. |
| History | Every schema version, newest first, with its change classes, the person or caller behind it, the diff, and the SQL that ran. Unattended applies show the policy tier and rehearsal verdict that authorized them. |
| Sandbox | Isolated test databases holding the organisation's schema and no production data: seeding, SQL, checkpoints, compare, fork. |
| Workers | Connected job-worker sessions and per-queue summaries, with per-session drain and evict. |
| Health | Live server activity — the most recent log lines, polled about every second. |
| Callers | The repositories enrolled with this organisation: registration, certificate expiry, enrolment (single-use tokens, the developers-may-enrol flag, CI federation rules), aliases, and each caller's apply policy and rehearsal grant. |
| Approvals | Changes waiting on a human. Each shows the proposed `.atl` source first, then the SQL, the change class, the requester, and any rehearsal verdict. Actions: rehearse, approve, reject, override. |
| Parked | Objects destructive migrations kept instead of dropping, and how long each remains recoverable. |
| Operations | Rollback (preview and execute), the dead-letter queue with per-job retry, and the audit log. |
| Settings | The organisation's policies and security controls, below. |

An imported database gets its own review page: entities read from the live
database, suggested tightenings, and the commit and apply steps that
record the baseline.
See [Adopt an existing database](../guides/adopt-an-existing-database.md).

## Settings

- **General** — the organisation's endpoint and current schema version.
- **Members** — who you are signed in as, the organisation, and your role.
  The panel is read-only; atlantis support adds members and sets roles.
- **Security** — your password (managed at Cloud) and your other sessions.
- **Change policy** — three panels: the per-class approval rules, protected
  entities, and freeze windows. See
  [Change approval](change-approval.md).
- **Danger zone** — sign out every session; **Revoke all caller
  certificates**, which revokes every caller: all of them stop
  authenticating and their registered files are removed. Each is restored
  individually from the Callers page, and its files re-register on the
  caller's next apply.

## What needs re-authentication

Reading is open to every signed-in user: the schema, history, plans,
rehearsal results, policies, workers, audit log. Deciding is gated twice —
by role, and for the actions below by a fresh re-authentication at Cloud
("sudo"), so a stolen session cookie alone cannot exercise them:

- Approving or overriding a plan
- Editing the change policy, protected entities, or freeze windows
- Setting a caller's apply policy or rehearsal grant
- Minting an enrolment token; changing enrolment policy or federation rules
- Restoring a revoked caller
- Setting caller aliases; draining or evicting a worker
- Applying a schema import
- Signing out all sessions; revoking all caller certificates

Rejecting a plan needs the role but no re-authentication.

## Roles

| Role | Adds |
|---|---|
| `viewer` | Every read. |
| `developer` | Booting sandboxes, triggering rehearsals, and deciding plans for any change class whose policy names `developer` as the approver. |
| `admin` | Everything: registration, enrolment, policies, overrides, import plan and apply, rollback, the danger zone. |

## Related

- [Change approval](change-approval.md) — the gates the Approvals page
  decides.
- [Use the sandbox](../guides/use-the-sandbox.md) — the Sandbox page,
  step by step.
- [Manage callers](../guides/manage-members-and-callers.md) — registration,
  enrolment, and per-caller policy.
