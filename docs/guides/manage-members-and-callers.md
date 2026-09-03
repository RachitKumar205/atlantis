# Manage members and callers

Give people the right role, connect repositories as callers, and decide how
much each caller may do on its own.

## Prerequisites

- The `admin` role, for every step past reading.

## Members and roles

Membership is per organisation, and a member holds one of three roles —
`viewer`, `developer`, or `admin`. [The console](../concepts/the-console.md#roles)
lists what each role can do.

The console's Settings → Members panel shows your own membership and role.
The panel is read-only: contact atlantis support to add a teammate to your
organisation or set their role.

## Register a caller

A caller is a repository's identity: what it may read, plan, and apply is
granted to the caller, and every schema version records which caller made
it.

1. Open **Callers** and select **Add caller**.
2. Name the caller after the repository. Names are lowercase letters,
   digits, and interior hyphens.
3. Enable mutate permission if the repository will plan and apply schema.
   Without it the caller reads only.

A registered caller has no credential yet — enrolment issues one.

## Enrol a machine

Choose one route per machine:

- **Self-enrol** — the person at the machine runs `tide login` and
  approves it in the browser. Admins can always self-enrol a caller;
  for others, enable **Developers may enrol** in the caller's enrolment
  dialog first.
- **Minted token** — in the caller's row, select **Enrol**. The console
  re-authenticates you, mints a single-use token, and prints the whole
  `tide login --url … --org … --token …` command to hand to the machine's
  owner. The machine generates its own key; no private key travels.
- **CI federation** — add a federation rule (issuer, audience, subject
  pattern) in the caller's enrolment dialog, and the CI job runs
  `tide login --oidc` with no repository secrets. See
  [Set up CI](set-up-ci.md).

Certificates renew themselves; the Callers page shows each caller's
expiry.

## Set a caller's apply policy

The apply policy is the caller's tier: how much of what it submits may run
unattended. In the caller's row, open the apply-policy dialog. The change
re-authenticates you and is recorded.

| Tier | Applies unattended |
|---|---|
| `sandbox_only` | Nothing — plan, generate, and rehearse stay available. |
| `always_ask` | Nothing — every class waits for approval. |
| `auto_safe` | Additive changes. The default. |
| `auto_verified` | Additive, plus backfills covered by a fresh passing rehearsal. |
| `auto_all` | Everything a passing rehearsal covers, except cross-caller breaking changes. |

The same dialog holds **May rehearse**, the per-caller grant to run
rehearsals. It is a separate decision from the tier: a rehearsal clones the
managed database, so the grant hands the caller a surface over every
caller's rows. The verified tiers do nothing until the caller can rehearse.

## Revoke a caller

**Callers → Revoke** revokes the caller's identity: every certificate for
it stops authenticating within seconds, reads and writes both, and its
registered schema files are removed. The schema history remains.

A revoked caller stays in the list with a `revoked` badge. **Restore** on
its card — behind a re-authentication — clears the revocation: unexpired
certificates work again within seconds, and the caller's files re-register
with its next `tide apply`. Registering the same name again is refused
while the caller is revoked; restore is the way back.

There is no per-machine revocation. The granularities are the caller
(**Callers → Revoke**) and the whole organisation (Settings → Danger zone
→ **Revoke all caller certificates**, which revokes every caller at once;
each is restored individually from its card).

## Verify

- The Callers page lists the caller with a certificate expiry meter after
  its first enrolment.
- From the repository, `tide plan` succeeds; with mutate permission off it
  is refused with a message naming the missing grant.

## Related

- [The console](../concepts/the-console.md) — every page and what it does.
- [Change approval](../concepts/change-approval.md) — how tiers compose
  with the change policy.
- [Set up CI](set-up-ci.md) — federation rules end to end.
