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
CALICO_VERSION="${CALICO_VERSION:-v3.32.1}"
# The cluster's pod network, as kubeadm configured it. Calico's own default is
# 192.168.0.0/16, which overlaps the host's container network on this machine.
#
# This must match provision.Config.PodCIDR, which writes it into every tenant's
# NetworkPolicy as the range to exclude. A drift between the two fails open —
# tenant pods land outside the exception and are admitted — so the guard is
# TestK8sTenantsCannotReachEachOthersDatabase, which probes the property rather
# than comparing the two settings.
POD_CIDR="${POD_CIDR:-10.244.0.0/16}"
LPP_VERSION="${LPP_VERSION:-v0.0.37}"
CONTAINER="${CONTAINER:-container}"

# ---------- 0. disk ----------
#
# Checked first, because a full disk is the failure this workflow actually hits
# and the one that never says so. Observed twice: the apiserver stops serving
# with nothing in its log, `container` reports the node as running, and an
# unrelated command answers `internalError: "mount"`. Every symptom points
# somewhere other than disk.
#
# The numbers come from measurement, not taste. One provisioned organisation
# costs roughly 400 MiB once its Postgres volume and three pods exist, and the
# cluster itself is about 6.5 GiB with images loaded. Three organisations took
# this machine from 5.5 GiB free to zero and killed the cluster mid-walkthrough.
#
# A warning rather than a refusal above the floor: it is legitimate to bring up
# a cluster on a tight disk and provision nothing.
DISK_WARN_GIB="${DISK_WARN_GIB:-15}"
DISK_FLOOR_GIB="${DISK_FLOOR_GIB:-5}"

free_gib() {
    # -P for POSIX output (one line per filesystem, no wrapping), -k for
    # kibibytes, which every df agrees on. $4 is available.
    df -Pk . 2>/dev/null | awk 'NR==2 {printf "%d", $4/1024/1024}'
}

avail="$(free_gib)"
if [ -n "$avail" ] && [ "$avail" -lt "$DISK_FLOOR_GIB" ]; then
    echo "refusing to start: ${avail} GiB free, below the ${DISK_FLOOR_GIB} GiB floor." >&2
    echo >&2
    echo "  A full disk kills this cluster in a way that looks like something" >&2
    echo "  else — the apiserver stops with no error and the node still reports" >&2
    echo "  as running. Free space first." >&2
    echo >&2
    echo "  Biggest reclaimable things, usually in this order:" >&2
    echo "    go clean -modcache                 # often several GiB" >&2
    echo "    make dev-k8s-destroy               # the cluster itself, ~6.5 GiB" >&2
    echo "    $CONTAINER image prune             # unreferenced layers" >&2
    echo >&2
    echo "  Override with DISK_FLOOR_GIB=0 if you know what you are doing." >&2
    exit 1
fi
if [ -n "$avail" ] && [ "$avail" -lt "$DISK_WARN_GIB" ]; then
    echo "warning: ${avail} GiB free. Each provisioned organisation costs about" >&2
    echo "         400 MiB, and a full disk stops this cluster without saying so." >&2
fi

# Images the cluster needs that must come from the host. Local builds are
# handled separately, below, because they must not be pulled.
REMOTE_IMAGES=(
    "ghcr.io/cloudnative-pg/cloudnative-pg:${CNPG_VERSION}"
    "docker.io/rancher/local-path-provisioner:${LPP_VERSION}"
    "docker.io/library/busybox:1.36"
    "docker.io/library/memcached:${MEMCACHED_VERSION:-1.6.29-alpine}"
    "quay.io/calico/node:${CALICO_VERSION:-v3.32.1}"
    "quay.io/calico/cni:${CALICO_VERSION:-v3.32.1}"
    "quay.io/calico/kube-controllers:${CALICO_VERSION:-v3.32.1}"
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
    "atlantis-provisioner:local"
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
    # Only the extra tag is dropped, never the image.
    #
    # The pulled images above are deleted from the host store because they can
    # be pulled again. These cannot: they are built here, and atlantis-pg takes
    # several minutes and a network round trip to the TimescaleDB apt
    # repository to reproduce. Deleting the host copy of something whose only
    # other copy is inside a cluster means destroying the cluster destroys the
    # image — which is exactly what happened, and it turned a five-minute
    # cluster rebuild into a twenty-minute one.
    #
    # `image rm "$ref"` is deliberately NOT here. If the two names resolve to
    # one image, dropping the qualified tag is a no-op and the build survives;
    # if they are separate, the unqualified build survives. Either way there is
    # still a copy on the host.
    "$CONTAINER" image rm "$qualified" >/dev/null 2>&1 || true
    echo "loaded $qualified"
done

# ---------- 3b. the CNI ----------
#
# kindnet is replaced by Calico because kindnet enforces no NetworkPolicy at
# all. Its entire flag set is logging flags — the API server accepts a policy
# and nothing implements it, so tenant namespaces would appear isolated while
# every pod could reach every other pod's database. A control that cannot fire
# is worse than a missing one, because it reads as present.
#
# CALICO_IPV4POOL_CIDR is the setting to get right, and it has to be right the
# first time: the manifest's own comment says changing it after installation has
# no effect. Calico's default pool is 192.168.0.0/16, which overlaps Apple
# `container`'s host network (192.168.64.0/24) — so the default here does not
# merely misconfigure the cluster, it breaks networking for every container on
# the machine.
say "CNI (Calico ${CALICO_VERSION:-v3.32.1})"
if kubectl --context "$CLUSTER" get daemonset -n kube-system calico-node >/dev/null 2>&1; then
    echo "calico present"
else
    manifest="$(mktemp -t calico)"
    # .bak too: sed -i on BSD writes one and would otherwise leave it behind.
    trap 'rm -f "$manifest" "$manifest.bak"' EXIT
    curl -fsSL -o "$manifest" \
        "https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VERSION:-v3.32.1}/manifests/calico.yaml"

    # The pool CIDR ships commented out. Fail loudly if the shape ever changes
    # rather than applying a manifest that silently keeps the default — the
    # whole point of this block is the one value it sets.
    # POSIX character classes, not \s. This is BSD sed on macOS, where \s
    # matches nothing at all and the substitution silently does nothing — which
    # is precisely why the check below exists rather than trusting the edit.
    if ! grep -q '^[[:space:]]*# - name: CALICO_IPV4POOL_CIDR' "$manifest"; then
        echo "the Calico manifest no longer has a commented CALICO_IPV4POOL_CIDR;" >&2
        echo "check what it looks like now before trusting this step" >&2
        exit 1
    fi
    sed -i.bak \
        -e 's|^\([[:space:]]*\)# - name: CALICO_IPV4POOL_CIDR|\1- name: CALICO_IPV4POOL_CIDR|' \
        -e 's|^\([[:space:]]*\)#   value: "192.168.0.0/16"|\1  value: "'"$POD_CIDR"'"|' \
        "$manifest"
    if ! grep -q "value: \"$POD_CIDR\"" "$manifest"; then
        echo "failed to set CALICO_IPV4POOL_CIDR to $POD_CIDR" >&2
        exit 1
    fi

    # kindnet first, so there is never a moment with two CNIs writing config.
    kubectl --context "$CLUSTER" delete daemonset -n kube-system kindnet --ignore-not-found >/dev/null
    "$CONTAINER" exec "$CLUSTER" rm -f /etc/cni/net.d/10-kindnet.conflist 2>/dev/null || true

    kubectl --context "$CLUSTER" apply -f "$manifest" >/dev/null
fi

kubectl --context "$CLUSTER" -n kube-system rollout status daemonset/calico-node --timeout=300s

# Verify rather than assume. A pool that came up on the default would collide
# with the host network, and the symptom is every container on the machine
# losing connectivity — a long way from anything that mentions Kubernetes.
pool="$(kubectl --context "$CLUSTER" get ippool default-ipv4-ippool \
    -o jsonpath='{.spec.cidr}' 2>/dev/null || true)"
if [ -n "$pool" ] && [ "$pool" != "$POD_CIDR" ]; then
    echo "Calico's IP pool is $pool, expected $POD_CIDR." >&2
    echo "It cannot be changed after installation — delete the cluster and start again:" >&2
    echo "  container k8s delete --name ${CLUSTER} && $0" >&2
    exit 1
fi
echo "pod network ${pool:-$POD_CIDR}, NetworkPolicy enforced"

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

# ---------------------------------------------------------------------------
# The provisioner, running under its own service account.
#
# Its role is a file — deploy/provisioner-rbac.yaml — because it is static, it
# is identical on every cluster, and it is the security artifact this is all
# about. The Deployment is inline here instead, because every value in it is
# local: image tags that exist only on this node, a hostname that resolves only
# on this machine, and a database address discovered at run time. A file with
# those baked in would read like a deployment manifest and be usable on exactly
# one laptop.
# ---------------------------------------------------------------------------
say "provisioner"

# The role goes on whether or not the workload does. It is static, it grants
# nothing to nobody until something is bound to it, and having it present means
# the impersonation test in kube_k8s_test.go can run against a cluster built by
# plain `make dev-k8s`.
kubectl --context "$CLUSTER" apply -f "$(dirname "$0")/provisioner-rbac.yaml" >/dev/null

# The Deployment needs three credentials and an image, and `make dev-k8s` has
# neither — it builds a cluster, not the product. Skipping is the right answer
# rather than failing: a pod referring to an image the node does not have never
# starts, and these nodes cannot reach a registry to find out otherwise.
#
# `crictl inspecti` on the qualified name, for the reason spelled out at the
# side-loading loop above: `crictl images` shows the normalised reference even
# when the lookup would fail, so the listing looks right while the pull does not.
if ! "$CONTAINER" exec "$CLUSTER" crictl inspecti \
    docker.io/library/atlantis-provisioner:local >/dev/null 2>&1 ||
    [ -z "${CLOUD_PG_URL:-}" ] || [ -z "${CONSOLE_PG_URL:-}" ] || [ -z "${CONSOLE_DATA_KEY:-}" ]; then
    echo "  skipping the provisioner Deployment: run 'make dev-k8s-load', which"
    echo "  builds the image and passes the database credentials."
    say "ready"
    kubectl --context "$CLUSTER" get nodes
    exit 0
fi

# The Cloud database runs outside the cluster, in a sibling container. CoreDNS
# does not resolve .test, so the pod is given the address directly through
# hostAliases and CLOUD_PG_URL stays byte-identical to the one in .env — one
# connection string, not two that can drift apart.
#
# The node and the database container share a /24 and pod egress is SNATed
# through the node, so this is reachable; deploy-time discovery is only because
# the address is assigned, not fixed.
PG_HOST_NAME="${PG_HOST:-atlantis-pg.test}"
PG_HOST_IP="$(ping -c1 -W1 "$PG_HOST_NAME" 2>/dev/null | head -1 | sed -E 's/.*\(([0-9.]+)\).*/\1/')"
if [ -z "$PG_HOST_IP" ]; then
    echo "cannot resolve $PG_HOST_NAME; is the atlantis-pg container running?" >&2
    exit 1
fi
echo "  $PG_HOST_NAME -> $PG_HOST_IP"

# Credentials go in a Secret rather than the pod spec: `kubectl get deployment`
# is something you run in front of other people.
kubectl --context "$CLUSTER" -n atlantis-system \
    create secret generic atlantis-provisioner \
    --from-literal=CLOUD_PG_URL="${CLOUD_PG_URL:?set CLOUD_PG_URL}" \
    --from-literal=CONSOLE_PG_URL="${CONSOLE_PG_URL:?set CONSOLE_PG_URL}" \
    --from-literal=CONSOLE_DATA_KEY="${CONSOLE_DATA_KEY:?set CONSOLE_DATA_KEY}" \
    --dry-run=client -o yaml | kubectl --context "$CLUSTER" apply -f - >/dev/null

kubectl --context "$CLUSTER" apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: atlantis-provisioner
  namespace: atlantis-system
  labels:
    app.kubernetes.io/name: atlantis-provisioner
spec:
  replicas: 1
  selector:
    matchLabels: { app.kubernetes.io/name: atlantis-provisioner }
  template:
    metadata:
      labels: { app.kubernetes.io/name: atlantis-provisioner }
    spec:
      serviceAccountName: atlantis-provisioner
      # true, and stated rather than left to the default — which is the opposite
      # of every other pod this system creates. The tenant workloads refuse the
      # token because they have no business calling the API. This process IS the
      # API caller: the token is the whole reason it has an identity. Beside
      # three manifests that say false, an omitted field would read like one
      # somebody forgot.
      automountServiceAccountToken: true
      hostAliases:
        - ip: "${PG_HOST_IP}"
          hostnames: ["${PG_HOST_NAME}"]
      securityContext:
        runAsNonRoot: true
        seccompProfile: { type: RuntimeDefault }
      terminationGracePeriodSeconds: 40
      containers:
        - name: provisioner
          image: atlantis-provisioner:local
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: { drop: [ALL] }
            readOnlyRootFilesystem: true
          envFrom:
            - secretRef: { name: atlantis-provisioner }
          env:
            - { name: CLOUD_AUDIENCE, value: "${CLOUD_AUDIENCE:-http://localhost:3000}" }
            - { name: PROVISIONER_EXTERNAL_HOST, value: "${EXTERNAL_HOST:-atl-dev.test}" }
            - { name: PROVISIONER_SERVER_IMAGE, value: "atlantis-server:local" }
            - { name: PROVISIONER_SIGNER_IMAGE, value: "atlantis-signer:local" }
            - { name: PROVISIONER_POSTGRES_IMAGE, value: "atlantis-pg:${PG_IMAGE_TAG}" }
            - { name: PROVISIONER_MEMCACHED_ADDR, value: "memcached.atlantis-system.svc.cluster.local:11211" }
            - { name: PROVISIONER_PULL_POLICY, value: "IfNotPresent" }
          ports:
            - name: health
              containerPort: 8082
          # Plain HTTP: this listener terminates no TLS. See the note in
          # internal/provisioner/health.go about what that means for /metrics,
          # which shares the port and is unauthenticated.
          readinessProbe:
            httpGet: { path: /readyz, port: 8082 }
            initialDelaySeconds: 3
            periodSeconds: 5
          livenessProbe:
            httpGet: { path: /healthz, port: 8082 }
            initialDelaySeconds: 10
            periodSeconds: 10
            failureThreshold: 6
          resources:
            requests:
              memory: 128Mi
              cpu: 50m
          volumeMounts:
            - { name: tmp, mountPath: /tmp }
      volumes:
        - name: tmp
          emptyDir: {}
EOF
kubectl --context "$CLUSTER" -n atlantis-system rollout status deployment/atlantis-provisioner --timeout=180s

say "ready"
kubectl --context "$CLUSTER" get nodes
