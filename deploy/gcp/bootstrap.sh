#!/usr/bin/env bash
# bootstrap.sh — bring up the GKE cluster the platform runs on, and everything
# in it that is not an atlantis workload.
#
# Idempotent: every step checks before it acts, so it can be re-run after a
# partial failure. It is the GCP counterpart of deploy/k8s-dev.sh and follows
# the same order: cluster, storage, operator, platform namespace, RBAC.
#
#   PROJECT=my-project ./deploy/gcp/bootstrap.sh
#
# Run step 1 first (build-images.sh or push-images.sh): the control-plane
# database runs the atlantis-pg image. What this does not do is deploy Cloud,
# the console and the provisioner; those need the platform secrets and come
# after.
set -euo pipefail
# VERBOSE=1 echoes every command before it runs.
[ -n "${VERBOSE:-}" ] && set -x

PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
ZONE="${ZONE:-us-central1-a}"
CLUSTER="${CLUSTER:-atlantis}"
# E2 for the app pool, per the plan's Stage 1 sizing. E2 cannot attach
# Hyperdisk, so every disk here is pd-balanced.
MACHINE="${MACHINE:-e2-standard-4}"
NODES="${NODES:-2}"
SANDBOX_MACHINE="${SANDBOX_MACHINE:-n2d-standard-4}"
SANDBOX_NODES="${SANDBOX_NODES:-1}"
CNPG_VERSION="${CNPG_VERSION:-1.30.0}"
# The Barman Cloud plugin needs cert-manager and CloudNativePG 1.26 or newer.
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.21.1}"
BARMAN_PLUGIN_VERSION="${BARMAN_PLUGIN_VERSION:-v0.15.0}"
BACKUP_BUCKET="${BACKUP_BUCKET:-${PROJECT}-atlantis-backups}"
REGION="${REGION:-${ZONE%-*}}"

if [ -z "$PROJECT" ] || [ "$PROJECT" = "(unset)" ]; then
    echo "PROJECT is not set and gcloud has no project configured" >&2
    exit 1
fi
cd "$(dirname "$0")"

say() { echo; echo "==> $*"; }

# ---------- 0. APIs ----------
say "APIs"
gcloud services enable container.googleapis.com artifactregistry.googleapis.com \
    secretmanager.googleapis.com compute.googleapis.com iam.googleapis.com \
    --project "$PROJECT" >/dev/null

# ---------- 1. the cluster ----------
#
# Zonal, one cluster: the free tier covers exactly one cluster fee. Dataplane
# V2 enforces the NetworkPolicies the provisioner writes; Workload Identity is
# how the backup pods reach Cloud Storage without a key file.
say "cluster $CLUSTER in $ZONE"
if gcloud container clusters describe "$CLUSTER" --zone "$ZONE" --project "$PROJECT" >/dev/null 2>&1; then
    echo "cluster present"
else
    gcloud container clusters create "$CLUSTER" --zone "$ZONE" --project "$PROJECT" \
        --enable-dataplane-v2 --workload-pool="$PROJECT.svc.id.goog" \
        --num-nodes "$NODES" --machine-type "$MACHINE" --disk-type pd-balanced --disk-size 50
fi

say "sandbox node pool (spot)"
if gcloud container node-pools describe sandbox --cluster "$CLUSTER" --zone "$ZONE" --project "$PROJECT" >/dev/null 2>&1; then
    echo "pool present"
else
    gcloud container node-pools create sandbox --cluster "$CLUSTER" --zone "$ZONE" --project "$PROJECT" \
        --spot --machine-type "$SANDBOX_MACHINE" --disk-type pd-balanced --num-nodes "$SANDBOX_NODES" \
        --node-labels=atlantis.dev/pool=sandbox --node-taints=atlantis.dev/sandbox=true:NoSchedule
fi

gcloud container clusters get-credentials "$CLUSTER" --zone "$ZONE" --project "$PROJECT" >/dev/null
CTX="gke_${PROJECT}_${ZONE}_${CLUSTER}"
k() { kubectl --context "$CTX" "$@"; }

# The pod range is what the provisioner writes into every tenant NetworkPolicy
# as the range to exclude. A wrong value fails open, so it is printed here and
# must be set as PROVISIONER_POD_CIDR on the provisioner Deployment.
POD_CIDR="$(gcloud container clusters describe "$CLUSTER" --zone "$ZONE" --project "$PROJECT" --format='value(clusterIpv4Cidr)')"

# ---------- 2. storage ----------
say "default StorageClass on the CSI driver"
k apply -f 00-storageclass.yaml >/dev/null
# GKE marks its own class default too; two defaults make a PVC without a class
# an error.
for sc in $(k get storageclass -o jsonpath='{.items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")].metadata.name}'); do
    if [ "$sc" != "pd-csi" ]; then
        k annotate storageclass "$sc" storageclass.kubernetes.io/is-default-class- >/dev/null
    fi
done
k get storageclass

# ---------- 3. CloudNativePG ----------
say "CloudNativePG $CNPG_VERSION"
if k get deployment -n cnpg-system cnpg-controller-manager >/dev/null 2>&1; then
    echo "operator present"
else
    k apply --server-side -f \
        "https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-${CNPG_VERSION%.*}/releases/cnpg-${CNPG_VERSION}.yaml" >/dev/null
fi
k -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout=300s

# ---------- 3b. cert-manager and the Barman Cloud plugin ----------
#
# The plugin is what writes WAL and base backups to Cloud Storage. It installs
# into the operator's namespace and issues its own certificates through
# cert-manager, which it requires.
say "cert-manager $CERT_MANAGER_VERSION"
if k get deployment -n cert-manager cert-manager-webhook >/dev/null 2>&1; then
    echo "cert-manager present"
else
    k apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml" >/dev/null
fi
k -n cert-manager rollout status deployment/cert-manager-webhook --timeout=300s

say "Barman Cloud plugin $BARMAN_PLUGIN_VERSION"
if k get deployment -n cnpg-system barman-cloud >/dev/null 2>&1; then
    echo "plugin present"
else
    k apply -f "https://github.com/cloudnative-pg/plugin-barman-cloud/releases/download/${BARMAN_PLUGIN_VERSION}/manifest.yaml" >/dev/null
fi
k -n cnpg-system rollout status deployment/barman-cloud --timeout=300s

# ---------- 3c. the backup bucket and the identity that writes to it ----------
#
# One Google service account for every backup writer, bound through Workload
# Identity to the Kubernetes ServiceAccount of each Postgres cluster. The
# control-plane cluster is bound here; the provisioner binds each tenant's
# once it emits backup configuration.
say "backup bucket gs://$BACKUP_BUCKET"
if ! gcloud storage buckets describe "gs://$BACKUP_BUCKET" --project "$PROJECT" >/dev/null 2>&1; then
    gcloud storage buckets create "gs://$BACKUP_BUCKET" --project "$PROJECT" --location "$REGION"         --uniform-bucket-level-access --public-access-prevention
fi
GSA="atlantis-backups@${PROJECT}.iam.gserviceaccount.com"
if ! gcloud iam service-accounts describe "$GSA" --project "$PROJECT" >/dev/null 2>&1; then
    gcloud iam service-accounts create atlantis-backups --project "$PROJECT" --display-name "atlantis backups"
fi
gcloud storage buckets add-iam-policy-binding "gs://$BACKUP_BUCKET" --project "$PROJECT"     --member "serviceAccount:$GSA" --role roles/storage.objectAdmin >/dev/null
gcloud iam service-accounts add-iam-policy-binding "$GSA" --project "$PROJECT"     --role roles/iam.workloadIdentityUser     --member "serviceAccount:${PROJECT}.svc.id.goog[atlantis-system/control]" >/dev/null

# ---------- 4. the platform namespace ----------
say "atlantis-system: memcached"
k apply -f 10-platform.yaml >/dev/null
k -n atlantis-system rollout status deployment/memcached --timeout=120s

# ---------- 4b. the control-plane database ----------
#
# Passwords are generated once and kept only in the cluster; the URLs the
# services need are printed at the end and belong in Secret Manager.
say "control-plane database"
for role in cloud console; do
    if ! k -n atlantis-system get secret "control-db-$role" >/dev/null 2>&1; then
        k -n atlantis-system create secret generic "control-db-$role"             --type kubernetes.io/basic-auth             --from-literal=username="atlantis_$role"             --from-literal=password="$(openssl rand -base64 30 | tr -d '/+=' | cut -c1-32)" >/dev/null
    fi
done
PG_IMAGE="${PG_IMAGE:-${REGION}-docker.pkg.dev/${PROJECT}/atlantis/atlantis-pg:${PG_IMAGE_TAG:-17.11}}"
sed -e "s|__PROJECT__|$PROJECT|g" -e "s|__BUCKET__|$BACKUP_BUCKET|g" -e "s|__PG_IMAGE__|$PG_IMAGE|g"     20-control-db.yaml | k apply -f - >/dev/null
echo "waiting for the control cluster (first bootstrap takes a few minutes; watch with: kubectl -n atlantis-system get cluster,pods -w)"
for i in $(seq 1 60); do
    phase="$(k -n atlantis-system get cluster control -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    ready="$(k -n atlantis-system get cluster control -o jsonpath='{.status.readyInstances}' 2>/dev/null || true)"
    echo "  $((i * 10))s  phase=${phase:-<none yet>}  ready instances=${ready:-0}/3"
    [ "$phase" = "Cluster in healthy state" ] && break
    sleep 10
done
echo "control cluster: ${phase:-unknown}"
if [ "$phase" = "Cluster in healthy state" ]; then
    k -n atlantis-system exec -i control-1 -c postgres -- psql -U postgres -d atlantis -v ON_ERROR_STOP=1 -q < control-roles.sql
    echo "roles granted"
fi

# ---------- 5. the provisioner's identity ----------
#
# Applied by an administrator, before the Deployment that references it; the
# provisioner cannot create the role that limits it.
say "provisioner RBAC"
k apply -f ../provisioner-rbac.yaml >/dev/null

CLOUD_PW="$(k -n atlantis-system get secret control-db-cloud -o jsonpath='{.data.password}' | base64 -d)"
CONSOLE_PW="$(k -n atlantis-system get secret control-db-console -o jsonpath='{.data.password}' | base64 -d)"
cat <<DONE

cluster ready: context $CTX
  pod range:    $POD_CIDR      -> PROVISIONER_POD_CIDR
  storage:      pd-csi         -> PROVISIONER_STORAGE_CLASS
  memcached:    memcached.atlantis-system.svc.cluster.local:11211
  backups:      gs://$BACKUP_BUCKET (ObjectStore control-backups, daily at 02:00 UTC)

connection strings for Secret Manager (in-cluster, TLS required):
  CLOUD_PG_URL=postgres://atlantis_cloud:$CLOUD_PW@control-rw.atlantis-system.svc.cluster.local:5432/atlantis?sslmode=require
  CONSOLE_PG_URL=postgres://atlantis_console:$CONSOLE_PW@control-rw.atlantis-system.svc.cluster.local:5432/atlantis?sslmode=require

next: 30-secrets, then the Cloud, console and provisioner Deployments
DONE
