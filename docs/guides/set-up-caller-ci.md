# Set up caller CI

After this recipe, every PR in your service repo that touches `.atl` files runs `tide plan` against a staging or production Atlantis server. Schema-breaking changes fail the CI job and block the merge.

Prereqs:

- Atlantis server reachable from the CI runner.
- `tide.yaml` in the service repo (`caller`, `endpoint`, `schema_paths`).
- TLS credentials for the runner if the server requires mTLS.

## GitHub Actions

```yaml
# .github/workflows/tide-plan.yml
name: tide plan

on:
  pull_request:
    paths:
      - "**/*.atl"
      - "tide.yaml"

jobs:
  plan:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: "1.24"

      - name: Install tide
        run: go install github.com/rachitkumar205/atlantis/cmd/tide@v0.1.0
        # Pin to a tagged version; bump when you upgrade the server.

      - name: Materialize TLS credentials
        env:
          TIDE_TLS_CERT_PEM: ${{ secrets.TIDE_TLS_CERT_PEM }}
          TIDE_TLS_KEY_PEM:  ${{ secrets.TIDE_TLS_KEY_PEM }}
          TIDE_TLS_CA_PEM:   ${{ secrets.TIDE_TLS_CA_PEM }}
        run: |
          umask 077
          mkdir -p .tls
          printf '%s' "$TIDE_TLS_CERT_PEM" > .tls/cert.pem
          printf '%s' "$TIDE_TLS_KEY_PEM"  > .tls/key.pem
          printf '%s' "$TIDE_TLS_CA_PEM"   > .tls/ca.pem

      - name: Plan against staging
        env:
          TIDE_TLS_CERT: .tls/cert.pem
          TIDE_TLS_KEY:  .tls/key.pem
          TIDE_TLS_CA:   .tls/ca.pem
        run: tide plan --against=atlantis-staging.internal:9090 --format=json
```

Replace `atlantis-staging.internal:9090` with your endpoint. Drop `--format=json` for human-readable output in the workflow log.

The job fails on any non-zero exit. `tide plan` returns 1 for backfill-required and 2 for cross-caller breaking; both should block the PR.

## Post the plan as a PR comment

Optional. Captures the plan output and posts it as a sticky comment on the PR:

```yaml
- name: Plan
  id: plan
  env:
    TIDE_TLS_CERT: .tls/cert.pem
    TIDE_TLS_KEY:  .tls/key.pem
    TIDE_TLS_CA:   .tls/ca.pem
  run: |
    tide plan --against=atlantis-staging.internal:9090 --format=json > plan.json
    echo "exit=$?" >> "$GITHUB_OUTPUT"

- name: Comment on PR
  uses: marocchino/sticky-pull-request-comment@v2
  with:
    header: tide-plan
    path: plan.json

- name: Fail on planning error
  if: steps.plan.outputs.exit != '0'
  run: exit ${{ steps.plan.outputs.exit }}
```

The sticky comment overwrites itself on each push, so the PR shows the latest plan only.

## Keep the generated client in sync

The typed Go client lives in the caller repo (under `output_dir`) and is regenerated with `tide generate`. Commit it alongside the `.atl` change that motivates it. A CI check guards against forgetting: regenerate against the server and fail if the working tree drifts.

```yaml
- name: Install buf
  uses: bufbuild/buf-setup-action@v1

- name: Check generated client is current
  env:
    TIDE_TLS_CERT: .tls/cert.pem
    TIDE_TLS_KEY:  .tls/key.pem
    TIDE_TLS_CA:   .tls/ca.pem
    ATL_ENDPOINT:  atlantis-staging.internal:9090
  run: |
    tide generate
    git diff --exit-code -- "$(yq '.output_dir' tide.yaml)" \
      || { echo "::error::generated client is stale — run 'tide generate' and commit"; exit 1; }
```

`tide generate` needs `buf` on the runner and reads the caller's `go.mod` for the module path, so run it from the repo root. The `generate:` namespaces and `output_dir` come from `tide.yaml`. Plan against the same environment you generate against so the proto field numbers match the server you'll deploy to.

## TLS credentials

Store the client cert, client key, and CA as GitHub Secrets and write them to files on the runner; the workflow above shows the pattern. The `TIDE_TLS_*` environment variables override `tls.*` in `tide.yaml`, so the on-disk paths stay out of committed config.

Alternatives: mount a secret from your secrets manager (Vault, AWS / GCP Secrets Manager) or use a workload-identity-bound service account if the runner supports it.

## Which environment to plan against

Staging is the safe default; its schema lags production but `tide plan` against it catches most cross-caller breakage without loading prod. Planning against production is also fine: `PlanSchema` is read-only and runs even when `ATL_ALLOW_APPLY_MUTATION=false`.

## Verify it works

Open a PR that adds a column to an `.atl` file. The `tide plan` check should appear on the PR and pass with an additive diff in the log (or in the sticky comment if you wired that step). If the check doesn't appear, GitHub Actions is filtering it out — confirm the changed paths match the workflow's `paths:` block.

## Related

- [`tide generate`](../reference/cli-tide.md#tide-generate) — regenerate the typed client; commit it alongside the schema change.
- [`tide` CLI reference](../reference/cli-tide.md) — full `tide plan` flag list and exit codes.
- [Schema flow](../architecture/schema-flow.md) — how dev and prod schema state diverge.
