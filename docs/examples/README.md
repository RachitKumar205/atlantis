# Examples

## Caller CI, through workload identity

A CI job authenticates with the identity its runner already has. On GitHub
Actions that is the job's OIDC token: the enrolment listener verifies it
against GitHub's published keys under a **federation rule** an admin
configured on the console's Callers page — issuer, audience, and a subject
pattern such as `repo:acme/api:*` — and issues a one-hour certificate. No
repository secrets, no key pasted anywhere, nothing stored between runs.

An earlier generation of these examples put a client certificate and **its
private key** into repository secrets. Enrolment replaced that: a machine
generates its own key and only a certificate signing request travels. The
workload path keeps that property — the runner's key is generated in the job
and dies with it.

```yaml
name: schema
on: [pull_request]

permissions:
  id-token: write     # the workload identity tide exchanges
  contents: read

jobs:
  plan:
    runs-on: ubuntu-latest
    env:
      ATL_ENROLL_URL: https://enroll.example.com   # the organisation's enrolment listener
    steps:
      - uses: actions/checkout@v4
      - run: curl -fsSL https://releases.tryatlantis.dev/install.sh | sh
      - run: tide login --oidc
      - run: tide plan
      - run: tide generate --check --against-server
```

The rule's audience defaults to `atlantis-enroll` on both sides; pass
`--audience` where the rule says otherwise. The certificate renews within the
budget the rule sets — once, by default — and then refuses, so a token stolen
off a runner is worth at most that many further hours.

`tide generate --check` with no flags needs no credentials at all, so it can
gate a pull request before any identity exists; `--against-server` adds "did
the schema move since this client was generated" and rides the same one-hour
certificate.

Exit codes, unchanged from the beginning:

| Exit code | Meaning | What CI should do |
|---|---|---|
| 0 | Applied, or nothing to do | Pass |
| 2 | Cross-caller breaking, or waiting for approval | Block the merge; for `apply`, treat as "waiting" rather than "broken" |
| 3 | The schema did not parse | Fail |
| 4 | Destructive | Block; somebody decides whether losing the rows is intended |
