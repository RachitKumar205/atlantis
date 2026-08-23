# Getting started

By the end of this page you will have an organisation with its own atlantis, a
machine enrolled against it, and a `Note` entity applied to that organisation's
Postgres.

This describes the **local stack**, which is the only way to run atlantis today.
Every command here was run end to end; where a step needs a browser, it says so.

## What you need

- Go 1.26.4 or later
- Apple `container` (this repo's local flow uses it rather than Docker)
- `kubectl`, `psql`
- Node 22, but only for the two pages you sign in through

An organisation runs in Kubernetes: its own certificate authorities, its own
Postgres, its own atlantis and its own certificate signer. That is what the
cluster below is for.

## 1. Bring up the platform

Four processes and two containers. Each `make` target runs in the foreground, so
give each one a terminal.

```
make dev-infra          # Postgres and memcached, in containers
make dev-k8s            # the Kubernetes cluster organisations are built into
make build-provision-images
make dev-k8s-load       # push those images into the cluster
```

`make dev-k8s` refuses to start if the disk is nearly full. Take it seriously: a
full disk stops the cluster in a way that reports anything except disk.

Then, one per terminal:

```
make dev-auth-app       # Atlantis Cloud, with its sign-in pages
make dev-console-app    # the management console, with its pages
make dev-provisioner    # builds organisations that have been queued
```

Use `dev-auth` and `dev-console` instead if you do not want to build the pages —
the APIs are the same, but you cannot sign in through a browser without them.

See [Local development](../guides/local-development.md) for what each of these
is and how to configure it.

## 2. Create your account

Open <http://localhost:9500/signin> and sign up. Two things differ from
production:

- **The verification link is printed, not emailed.** No mail server is
  configured locally, so `make dev-auth` writes the message to its terminal.
  Open the link from there.
- **A second factor is required**, not optional. You will be asked to enrol an
  authenticator app before your first sign-in completes, and shown ten backup
  codes once.

`cloud user create` exists but deliberately makes an account with no credential,
which cannot sign in. Sign up through the browser.

## 3. Create an organisation

```
make dev-cloud-seed EMAIL=you@example.com ORG=acme
```

That creates the organisation, makes you its admin, and queues it. The
provisioner picks it up within a few seconds and takes about a minute to build
a namespace, two certificate authorities, a Postgres cluster, an atlantis and a
signer.

Watch it:

```
make dev-org-status ORG=acme
```

`state` goes `pending` → `provisioning` → `ready`. To create and wait in one
command instead:

```
./bin/atlantis-cloud org create -org acme -owner you@example.com -wait
```

It exits non-zero if the organisation does not come up, and prints why.

## 4. Register a caller and enrol this machine

A **caller** is a service that talks to atlantis. It needs to exist before a
machine can enrol as it.

In the console at <http://localhost:3000>, open **Callers**:

1. Add a caller named `backend`. Leave the permission to change schema **on** —
   it is on by default. A caller without it can read and nothing else, and
   `tide plan` will refuse.
2. Use its enrol action to mint a token. This asks you to re-enter your second
   factor; the token is single-use and expires in fifteen minutes.

If you register a caller through the API rather than the page, note that
`can_mutate` defaults to **false** there — the opposite of the checkbox.

Then, on the machine that will run `tide`:

```
go install ./cmd/tide

tide login \
  --url https://127.0.0.1:3443 \
  --org acme \
  --token <the token> \
  --ca ./certs/ca.crt
```

`tide` generates a private key locally, sends only a certificate signing
request, and stores the result under `~/.atlantis/acme/backend/`. The key never
leaves the machine.

`--ca` is **local development only**. It exists because the enrolment listener
uses a development certificate that your system does not trust. In a deployment
that listener has a publicly trusted certificate and the flag is not used.

## 5. Declare an entity

Create a directory for your service and add `tide.yaml`:

```yaml
caller: backend       # the caller you registered above
org: acme             # which organisation this repository belongs to
schema_paths:         # directories scanned recursively for *.atl
  - .
```

There is no `endpoint:` and no TLS configuration. The organisation owns its
address and issues the credential; `tide login` collected both.

Add `schema.atl`:

```
entity Note in app {
  id         bigint primary
  title      varchar(200) not null
  body       text
  created_at timestamptz not null default now()
}
```

`app` is the schema namespace.

## 6. Plan, then apply

```
tide plan
```

This prints what would change and a plan id. Nothing has been written yet. Then:

```
tide apply
```

```
✔ applied at 2026-08-23T10:45:31Z
  content  d809a3ee7eda
```

The table now exists in that organisation's own Postgres:

```
kubectl exec -n org-acme pg-1 -c postgres -- \
  psql -U postgres -d atlantis -c '\d app_note'
```

## What's next

- `tide list` — every entity in the merged schema.
- `tide show Note` — the canonical `.atl` text for one entity.
- [Declare a custom query](your-first-custom-query.md) — a read that does not
  fit primary-key lookup.
- [Use the sandbox](../guides/use-the-sandbox.md) — a disposable copy of the
  schema with seed data.
- [Concepts](../concepts/) — the model behind `.atl`, the cache, the CLI split.

`tide generate` writes a typed Go client for the namespaces `tide.yaml` lists
under `generate:`.

## If something goes wrong

| What you see | What it means |
|---|---|
| `requires CAPABILITY_SCHEMA_PLAN` | The caller was registered without permission to change schema — most likely through the API, where it defaults to off. Turn it on in the console; re-enrolling will not help |
| `duplicate entity` naming `.tide-cache/` | You are on a `tide` older than this page. Rebuild it |
| `org status` stuck on `pending` | No provisioner is running, or it cannot reach the cluster. Check `make dev-provisioner`'s terminal |
| `org status` showing `failed` | Read `last error` on the same output. A missing image is the usual cause — run `make dev-k8s-load` |
| The cluster stops answering | Nearly always a full disk. `make dev-k8s-destroy` then `make dev-k8s` is faster than diagnosing it |
