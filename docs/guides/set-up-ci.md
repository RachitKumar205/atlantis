# Set up CI

Run `tide plan` on pull requests and `tide apply` on merge, with no
repository secrets: the CI job authenticates with the identity its runner
already has.

## Prerequisites

- A registered caller with mutate permission
  ([Manage members and callers](manage-members-and-callers.md)).
- The `admin` role, with a re-authenticated session, to add the
  federation rule.
- `org:` set in the repository's `tide.yaml` — `tide login --oidc` reads
  the organisation from it (or from `--org`) and exits 3 without one.

## 1. Add a federation rule

On the console's **Callers** page, open the caller's enrolment dialog and
add a rule: the issuer (`https://token.actions.githubusercontent.com` for
GitHub Actions), the audience (`atlantis-enroll` unless you change it on
both sides), and a subject pattern such as `repo:acme/api:*`.

The enrolment listener verifies each job's OIDC token against the issuer's
published keys under that rule and issues a one-hour certificate. The
runner generates its own key inside the job; only a certificate signing
request travels, and the credential dies with the runner. The certificate
renews within the budget the rule sets — once, by default — and then
refuses, so a credential stolen off a runner is useful for at most two
hours.

## 2. The pull-request workflow

```yaml
name: schema
on: [pull_request]

permissions:
  id-token: write     # the workload identity tide exchanges
  contents: read

jobs:
  plan:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: curl -fsSL https://releases.tryatlantis.dev/install.sh | sh
      - run: tide login --oidc --url=https://enroll.example.com
      - run: tide plan
      - run: tide generate --check --against-server
```

`--url` is the organisation's enrolment listener. `tide generate --check`
alone needs no credentials, so it can gate a pull request before any
identity exists; `--against-server` also fails when the server's schema
has moved since the client was generated, and rides the same one-hour
certificate.

## 3. Rehearse before unattended applies

On the `auto_verified` tier, a backfill-required change applies unattended
only behind a fresh passing rehearsal; `auto_all` extends that to
destructive changes. Additive changes apply unattended on both tiers with
no rehearsal, and a cross-caller breaking change always waits for a
person. The merge workflow rehearses before it applies:

```yaml
name: schema-apply
on:
  push:
    branches: [main]

permissions:
  id-token: write
  contents: read

jobs:
  apply:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: curl -fsSL https://releases.tryatlantis.dev/install.sh | sh
      - run: tide login --oidc --url=https://enroll.example.com
      - run: tide rehearse
      - run: tide apply --wait-for-approval=30m
```

`tide rehearse` executes the migration against a disposable clone of the
real database. Exit 0 is a pass; exit 2 means the real apply would fail the
same way — fix the schema or the data; exit 4 means the rehearsal could not
answer, which the tiers treat as no. The verdict is consumed within an
hour, so rehearse in the same job as the apply. Rehearsing requires the
caller's rehearsal grant ([Manage members and callers](manage-members-and-callers.md)).

## 4. Branch on the exit codes

| Exit | From | Meaning | What CI does |
|---|---|---|---|
| 0 | any | Applied, passed, or nothing to do | Pass |
| 1 | `plan`, `apply` | Backfill required | Re-run apply with `--backfill`, or fail for a human |
| 1 | `generate --check` | Committed client is stale | Fail; run `tide generate` and commit |
| 2 | `plan` | Cross-caller breaking | Block the merge |
| 2 | `apply` | Waiting for approval — or the request was rejected; the message says which | Treat as waiting; `--wait-for-approval` polls for you and stops on a rejection |
| 2 | `rehearse` | The migration fails on real data | Block the merge |
| 3 | any | Operational error (parse, network, config) | Fail |
| 4 | `plan` | Destructive | Block; a person decides whether losing the rows is intended |
| 4 | `rehearse` | Unverified — the rehearsal could not answer | Block, or fall back to the approval queue |

## Verify

Open a pull request with an additive `.atl` change: the plan step passes.
Merge it: the apply step prints `✔ applied at <timestamp>` with the new
content hash, and the console's History page shows the change attributed
to the CI caller.

## Related

- [`tide` CLI](../reference/cli-tide.md) — every flag and exit code.
- [Change approval](../concepts/change-approval.md) — the tiers and the
  rehearsal verdicts.
- [The generated client](../concepts/the-generated-client.md) — what
  `generate --check` holds in place.
