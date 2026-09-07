# Deploying the platform on GCP

The Stage 1 shape from the plan: one zonal GKE cluster, CloudNativePG, one
Postgres cluster per organisation on a shared node pool, and the control
plane (Cloud, console, provisioner) beside them in `atlantis-system`. It is
the same layout `deploy/k8s-dev.sh` builds locally, on Google's nodes.

Every step is a file here. Run them in order from the repository root, with
`PROJECT` set to the Google project id.

| Step | Command or file | What it leaves behind |
|---|---|---|
| 1 | `./deploy/gcp/build-images.sh` | six images in Artifact Registry, tagged with the commit, built on Cloud Build (amd64). `push-images.sh` builds the same images on this machine, under emulation for the Go stages, and is the fallback |
| 2 | `./deploy/gcp/bootstrap.sh` | the cluster, `pd-csi` StorageClass, CloudNativePG, cert-manager, the Barman Cloud plugin, the backup bucket and identity, memcached, the control-plane database with two roles, the provisioner's RBAC |
| 3 | `./deploy/gcp/30-secrets.sh` | the data keys, signing key, session secret, connection strings and Resend key in Secret Manager; the `atlantis-cloud`, `atlantis-console` and `atlantis-provisioner` Secrets in the cluster |
| 4 | `./deploy/gcp/40-control-plane.sh` | Cloud, the console and the provisioner Deployments from `40-control-plane.yaml`, with the images, domain and pod range filled in |
| 5 | `./deploy/gcp/50-traffic.sh` | three static addresses, the Ingress with a Google-managed certificate for `platform.` and `console.`, the Let's Encrypt certificate for `enroll.`, the passthrough load balancer every organisation's port is reached through, firewall rules; the four A records at Cloudflare |

Step 5 takes `ACME_EMAIL` on every run and a Cloudflare API token
(`CLOUDFLARE_API_TOKEN`) once, stored in Secret Manager on first use. The
token needs `Zone:Zone:Read` and `Zone:DNS:Edit` on the domain's zone, the
first for the zone lookup and cert-manager's DNS-01 challenge, the second to
write records. `WRITE_DNS=no` prints the records instead of writing them. The domain's DNS is at Cloudflare;
every record must be "DNS only" (proxy off), because Google validates the
managed certificate at the address itself and the other two listeners carry
TLS end to end.

The console pod stays in ContainerCreating between steps 4 and 5: its
enrolment listener mounts the certificate step 5 issues.

## What the bootstrap prints

`bootstrap.sh` ends by printing the four values the later steps need: the
pod range (`PROVISIONER_POD_CIDR`), the StorageClass name, the memcached
address, and the two database connection strings. Put the connection strings
in Secret Manager; they are printed once and the passwords live only in the
cluster.

`PROVISIONER_POD_CIDR` is the one to get right. The provisioner writes it
into every tenant NetworkPolicy as the range to exclude, its default is the
local cluster's `10.244.0.0/16`, and a mismatch fails open.

## Settings

Values that differ from the local cluster, from `ops/configuration.md`:

| Variable | Value |
|---|---|
| `CLOUD_ISSUER`, `CLOUD_PUBLIC_URL` | `https://platform.<domain>` |
| `CLOUD_AUDIENCE` (Cloud, console, provisioner) | `https://console.<domain>`, byte-identical in all three |
| `CLOUD_JWKS_URL` (console) | `http://atlantis-cloud.atlantis-system.svc.cluster.local:9500/.well-known/jwks.json` |
| `CLOUD_COOKIE_SECURE`, `CONSOLE_COOKIE_SECURE` | `true` |
| `CLOUD_TRUST_PROXY` | `true`, behind the Gateway |
| `CLOUD_MAIL_DEV` | `false`; `CLOUD_RESEND_API_KEY` and `CLOUD_MAIL_FROM` set, sending domain verified at Resend |
| `CONSOLE_ENROLL_LISTEN` | `:3443` |
| `CONSOLE_ENROLL_PUBLIC_URL`, `PROVISIONER_ENROLL_URL` | `https://enroll.<domain>` |
| `PROVISIONER_ORG_DOMAIN` | `<domain>`: each organisation is `<org>.<domain>`, served through the wildcard record |
| `PROVISIONER_CONSOLE_IN_CLUSTER` | `true` |
| `PROVISIONER_STORAGE_CLASS` | `pd-csi` |
| `PROVISIONER_POD_CIDR` | the cluster's pod range, from the bootstrap output |
| `PROVISIONER_POSTGRES_STORAGE` | `10Gi` |
| `PROVISIONER_*_IMAGE` | the references step 1 prints |

Secrets, generated once and kept in Secret Manager: `CLOUD_DATA_KEY`
(`atlantis-cloud data-key`), `CONSOLE_DATA_KEY`, the Cloud signing key
(`atlantis-cloud signing-key`), `CONSOLE_SESSION_SECRET` (32 bytes or more),
the Resend key, and the two connection strings. Losing either data key makes
every TOTP factor or every organisation permanently unopenable.

## Traffic

Two kinds of listener, and they need different fronts:

- **HTTP behind a proxy:** Cloud on 9500 at `platform.` and the console on
  3000 at `console.`, through a GKE Ingress with a Google-managed
  certificate.
- **TLS terminated by the workload itself:** the console's enrolment
  listener on 3443 at `enroll.`, and every organisation's gRPC NodePort at
  `<org>.<domain>`, which is mTLS. These get passthrough TCP load balancers
  onto the node pool, static addresses, and firewall rules admitting 443 and
  the NodePort range 30000 to 32767. Nothing in front may terminate TLS. The
  enrolment listener's certificate comes from Let's Encrypt through
  cert-manager, proven by DNS-01 at Cloudflare.

Names: `platform.<domain>` is Cloud, `console.<domain>` the console,
`enroll.<domain>` enrolment, and `<org>.<domain>` each organisation through
one wildcard record. Cloud refuses those platform labels as organisation
names.

## Before a customer's production data

The plan's entry conditions, and two of them are code:

1. The provisioner emits no backup configuration for tenant clusters. Give
   it the same ObjectStore, plugin and Workload Identity binding the control
   cluster has, then a scheduled restore drill with measured RTO and RPO.
2. The provisioner has no setting for instance count; tenants run one
   instance. Three is the floor once an SLO is sold.
3. gVisor for sandbox and rehearsal pods, decided and recorded.
4. Runbooks, a published SLO, an escalation path, a customer export path.
