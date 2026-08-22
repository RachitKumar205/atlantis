#!/usr/bin/env bash
# k8s-dev.sh — bring up the local Kubernetes cluster provisioning runs against.
#
# Idempotent: every step checks before it acts, so running this twice is cheap
# and running it after a partial failure finishes the job. That matters more
# here than usual, because the cluster is not durable — see "Recreate, don't
# repair" below.
#
# This exists because the cluster needs three things the node image does not
# give you, and every one of them was originally applied by hand and then lost:
#
#   1. A working resolver. The node's only nameserver answers `.test` and
#      refuses everything else, so nothing in-cluster can resolve a registry.
#   2. A StorageClass. Unlike kind, the node image ships the local-path
#      provisioner's *images* but deploys nothing, so there is no default
#      StorageClass and every PVC stays Pending.
#   3. Images. Containers under Apple `container` have no outbound internet on
#      this machine, so the node cannot pull. Everything is pulled host-side and
#      pushed in with `container k8s load-image`.
#
# Recreate, don't repair. A full host disk kills this cluster in a way that
# looks like something else: the apiserver silently stops, its cert disappears
# from /etc/kubernetes/pki, and a restart brings the node back on a different
# IP. Recreating takes a few minutes and always works; diagnosing it does not.
#
# Disk is the recurring constraint. `container k8s load-image` stores an image
# twice — once in the host store, once in the node's containerd — so this script
# drops the host copy after loading. A full disk on this machine does not
# announce itself; it surfaces as `internalError: "mount"` from an unrelated
# command.

set -euo pipefail

CLUSTER="${CLUSTER:-atl-dev}"
CPUS="${CPUS:-4}"
MEMORY="${MEMORY:-6g}"
CNPG_VERSION="${CNPG_VERSION:-1.30.0}"
LPP_VERSION="${LPP_VERSION:-v0.0.37}"
CONTAINER="${CONTAINER:-container}"

# Images the cluster needs that must come from the host. Local builds are
# handled separately, below, because they must not be pulled.
REMOTE_IMAGES=(
    "ghcr.io/cloudnative-pg/cloudnative-pg:${CNPG_VERSION}"
    "docker.io/rancher/local-path-provisioner:${LPP_VERSION}"
    "docker.io/library/busybox:1.36"
    "docker.io/library/memcached:${MEMCACHED_VERSION:-1.6.29-alpine}"
)

# Built by `make build-server-image` / `build-signer-image` / `build-pg-image`.
# Absent ones are skipped with a warning rather than failing the whole run, so
# this script is usable before every image exists.
# atlantis-pg is tagged with a version rather than `local` because CloudNativePG
# parses the tag for the Postgres major version and refuses anything that does
# not look like one.
PG_IMAGE_TAG="${PG_IMAGE_TAG:-17.11}"
LOCAL_IMAGES=(
    "atlantis-pg:${PG_IMAGE_TAG}"
    "atlantis-server:local"
    "atlantis-signer:local"
)

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

require() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "missing: $1" >&2
        exit 1
    }
}

require "$CONTAINER"
require kubectl

# ---------- 1. the cluster ----------
say "cluster ${CLUSTER}"
if "$CONTAINER" k8s list 2>/dev/null | awk '{print $1}' | grep -qx "$CLUSTER"; then
    echo "exists"
else
    "$CONTAINER" k8s create --name "$CLUSTER" --cpus "$CPUS" --memory "$MEMORY"
fi

# The node can be present but stopped, and `k8s create` will not start it.
if ! "$CONTAINER" k8s list 2>/dev/null | grep -q "^${CLUSTER}.*running"; then
    "$CONTAINER" start "$CLUSTER" >/dev/null 2>&1 || true
fi

# ---------- 2. the resolver ----------
#
# Appended rather than replaced: 192.168.64.1 is what resolves `.test`, which is
# how the host reaches atlantis-pg and friends, and dropping it would break
# anything in-cluster that needs those names.
say "node resolver"
"$CONTAINER" exec "$CLUSTER" sh -c \
    'grep -q "nameserver 1.1.1.1" /etc/resolv.conf ||
     printf "nameserver 1.1.1.1\nnameserver 8.8.8.8\n" >> /etc/resolv.conf'
echo "public resolvers present"

# Wait for the API before anything that talks to it.
say "waiting for the API server"
for _ in $(seq 1 40); do
    if kubectl --context "$CLUSTER" get --raw /readyz >/dev/null 2>&1; then
        echo "ready"
        break
    fi
    sleep 5
done
kubectl --context "$CLUSTER" get --raw /readyz >/dev/null 2>&1 || {
    echo "the API server did not come up. Recreate rather than repair:" >&2
    echo "  container k8s delete --name ${CLUSTER} && $0" >&2
    exit 1
}

# ---------- 3. images ----------
say "images"
for ref in "${REMOTE_IMAGES[@]}"; do
    if "$CONTAINER" exec "$CLUSTER" crictl images 2>/dev/null |
        awk '{print $1":"$2}' | grep -qx "$ref"; then
        echo "in cluster: $ref"
        continue
    fi
    echo "pulling $ref"
    "$CONTAINER" image pull --platform linux/arm64 "$ref" >/dev/null
    "$CONTAINER" k8s load-image --name "$CLUSTER" "$ref" >/dev/null
    # Host copy is dead weight once containerd has it, and this is the single
    # largest source of disk growth in this workflow.
    "$CONTAINER" image rm "$ref" >/dev/null 2>&1 || true
    echo "loaded $ref"
done

# Local images are tagged into docker.io/library/ before they are loaded, and
# that is not cosmetic.
#
# containerd stores an image under the exact reference it was imported with, and
# `container build -t atlantis-server:local` produces the bare name. Kubernetes
# normalises an unqualified image in a pod spec to docker.io/library/..., so
# kubelet then asks for a name the store does not have, decides the image is
# absent, and tries to pull it from Docker Hub — which these nodes cannot reach.
#
# What makes it expensive to diagnose is that `crictl images` *displays* the
# normalised form, so the listing looks exactly right while the lookup fails.
# `crictl inspecti docker.io/library/atlantis-server:local` is the honest check.
#
# We never push anything to a registry; docker.io/library here is only the
# namespace Kubernetes resolves bare names into.
#
# FORCE_LOAD exists because a locally built image keeps its tag when it is
# rebuilt. Skipping on "the tag is already in the cluster" is right for the
# remote images above, whose tags are immutable, and silently wrong here: it
# would leave yesterday's binary running and report success. `make dev-k8s-load`
# sets it for exactly that reason.
for ref in "${LOCAL_IMAGES[@]}"; do
    qualified="docker.io/library/${ref}"

    # The cluster is asked before the host store, and the order matters: this
    # script deletes the host copy once an image is loaded, so checking the host
    # first would report "not built" for an image that is present and working in
    # the cluster.
    #
    # Ask the way kubelet asks. Matching on the `crictl images` listing instead
    # is what hid the qualification bug: that output is normalised for display
    # and happily lists an image kubelet cannot resolve.
    if [ -z "${FORCE_LOAD:-}" ] &&
        "$CONTAINER" exec "$CLUSTER" crictl inspecti "$qualified" >/dev/null 2>&1; then
        echo "in cluster: $qualified (set FORCE_LOAD=1 to replace a rebuilt image)"
        continue
    fi

    if ! "$CONTAINER" image ls 2>/dev/null | awk '{print $1":"$2}' | grep -qx "$ref"; then
        echo "not built, skipping: $ref (run: make build-provision-images)"
        continue
    fi

    "$CONTAINER" image tag "$ref" "$qualified" >/dev/null 2>&1 || true
    "$CONTAINER" k8s load-image --name "$CLUSTER" "$qualified" >/dev/null
    "$CONTAINER" image rm "$qualified" >/dev/null 2>&1 || true
    echo "loaded $qualified"
done

# ---------- 4. storage ----------
#
# The node image ships kindest/local-path-provisioner but deploys nothing, so
# without this every PVC — including every Postgres cluster — stays Pending.
say "storage"
if kubectl --context "$CLUSTER" get storageclass local-path >/dev/null 2>&1; then
    echo "local-path present"
else
    kubectl --context "$CLUSTER" apply -f \
        "https://raw.githubusercontent.com/rancher/local-path-provisioner/${LPP_VERSION}/deploy/local-path-storage.yaml"
fi

# The helper pod defaults to `busybox` with no tag, which Kubernetes treats as
# :latest and therefore imagePullPolicy: Always — a guaranteed failure on a node
# that cannot pull. Pin it to the tag loaded above.
kubectl --context "$CLUSTER" -n local-path-storage patch configmap local-path-config \
    --type merge -p "$(
        cat <<'JSON'
{"data":{"helperPod.yaml":"apiVersion: v1\nkind: Pod\nmetadata:\n  name: helper-pod\nspec:\n  priorityClassName: system-node-critical\n  tolerations:\n    - key: node.kubernetes.io/disk-pressure\n      operator: Exists\n      effect: NoSchedule\n  containers:\n  - name: helper-pod\n    image: docker.io/library/busybox:1.36\n    imagePullPolicy: IfNotPresent\n"}}
JSON
    )" >/dev/null

kubectl --context "$CLUSTER" patch storageclass local-path \
    -p '{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}' >/dev/null
echo "local-path is the default StorageClass"

# ---------- 5. CloudNativePG ----------
say "CloudNativePG ${CNPG_VERSION}"
if kubectl --context "$CLUSTER" get deployment -n cnpg-system cnpg-controller-manager >/dev/null 2>&1; then
    echo "operator present"
else
    kubectl --context "$CLUSTER" apply --server-side -f \
        "https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-${CNPG_VERSION%.*}/releases/cnpg-${CNPG_VERSION}.yaml" >/dev/null
fi

# The manifest asks for Always, which cannot work on a node that cannot pull.
kubectl --context "$CLUSTER" -n cnpg-system patch deployment cnpg-controller-manager \
    --type=json \
    -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' \
    >/dev/null 2>&1 || true

kubectl --context "$CLUSTER" -n cnpg-system rollout status \
    deployment/cnpg-controller-manager --timeout=300s

# ---------- 6. the shared cache ----------
#
# One memcached for every organisation, in a platform namespace rather than a
# tenant one. It is shared because there is nothing tenant-specific in it and a
# cache per organisation would be the largest per-tenant cost on the node.
#
# Not optional, despite being a cache. atlantis's readiness probe performs a
# real cache operation and reports 503 on anything that is not a hit or a miss,
# so an organisation whose MEMCACHED_ADDR points nowhere never becomes Ready —
# and because the client connects lazily, the process starts happily and simply
# never passes readiness.
say "shared cache"
kubectl --context "$CLUSTER" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: atlantis-system
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: memcached
  namespace: atlantis-system
spec:
  replicas: 1
  selector:
    matchLabels: { app.kubernetes.io/name: memcached }
  template:
    metadata:
      labels: { app.kubernetes.io/name: memcached }
    spec:
      containers:
        - name: memcached
          image: docker.io/library/memcached:${MEMCACHED_VERSION:-1.6.29-alpine}
          imagePullPolicy: IfNotPresent
          args: ["-m", "256", "-I", "5m"]
          ports:
            - name: memcached
              containerPort: 11211
          resources:
            requests:
              memory: 64Mi
              cpu: 25m
---
apiVersion: v1
kind: Service
metadata:
  name: memcached
  namespace: atlantis-system
spec:
  selector: { app.kubernetes.io/name: memcached }
  ports:
    - name: memcached
      port: 11211
      targetPort: 11211
EOF
kubectl --context "$CLUSTER" -n atlantis-system rollout status deployment/memcached --timeout=120s

say "ready"
kubectl --context "$CLUSTER" get nodes
