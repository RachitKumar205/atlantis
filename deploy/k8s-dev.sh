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
#   1. A working resolver, in two places. The node's only nameserver answers
#      `.test` and refuses everything else. CoreDNS then forwards to that same
#      file with the refusing server first, so no pod resolves a public name.
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
# Each entry is a reference, or `pull-from|load-as` when the two differ.
#
# They differ for Calico, and only because of where the bytes come from.
#
# quay.io serves these at about 20 KB/s from here: a 53 MB image reached 28% in
# twenty minutes and then stopped moving, while github.com managed 1.7 MB/s in
# the same minute. Docker Hub has the identical tags and pulled the same image
# in forty seconds.
#
# The load-as name still has to say quay.io. The Calico manifest applied below
# references quay.io/calico/*, and containerd stores an image under the exact
# reference it was imported with — so one loaded as docker.io/calico/node is one
# the kubelet cannot find, and it would try to pull from quay.io on a node with
# no route to the internet. Pulling from the fast mirror and importing under the
# name the manifest expects is what makes both true at once.
#
# If Docker Hub is ever the slow one, this is a one-line edit per image.
REMOTE_IMAGES=(
    "ghcr.io/cloudnative-pg/cloudnative-pg:${CNPG_VERSION}"
    "docker.io/rancher/local-path-provisioner:${LPP_VERSION}"
    "docker.io/library/busybox:1.36"
    "docker.io/library/memcached:${MEMCACHED_VERSION:-1.6.29-alpine}"
    "docker.io/calico/node:${CALICO_VERSION:-v3.32.1}|quay.io/calico/node:${CALICO_VERSION:-v3.32.1}"
    "docker.io/calico/cni:${CALICO_VERSION:-v3.32.1}|quay.io/calico/cni:${CALICO_VERSION:-v3.32.1}"
    "docker.io/calico/kube-controllers:${CALICO_VERSION:-v3.32.1}|quay.io/calico/kube-controllers:${CALICO_VERSION:-v3.32.1}"
    "registry.k8s.io/metrics-server/metrics-server:${METRICS_SERVER_VERSION:-v0.9.0}"
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
    "atlantis-cloud:local"
    "atlantis-console:local"
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

# ---------- 2b. in-cluster DNS ----------
#
# CoreDNS ships with `forward . /etc/resolv.conf`, and section 2 leaves
# 192.168.64.1 first in that file. It answers `.test` and returns SERVFAIL for
# everything else. The forward plugin fails over on timeouts and connection
# errors, not on a SERVFAIL — that is a valid response, so it reaches the
# client and the public resolvers appended below it are never tried.
#
# The effect is one-sided and easy to misread: `.test` resolves, so the
# database is reachable and the cluster looks healthy, while every pod lookup
# of a public name fails. Cloud sending mail through api.resend.com is the
# case that surfaces it.
#
# One zone per resolver, so each name goes to a server that can answer it.
# The kubernetes plugin still claims cluster.local ahead of both.
say "in-cluster DNS"
dns_applied=$(kubectl --context "$CLUSTER" apply -f - <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: coredns
  namespace: kube-system
data:
  Corefile: |
    test:53 {
        errors
        cache 30
        forward . 192.168.64.1
    }
    .:53 {
        errors
        health {
           lameduck 5s
        }
        ready
        kubernetes cluster.local in-addr.arpa ip6.arpa {
           pods insecure
           fallthrough in-addr.arpa ip6.arpa
           ttl 30
        }
        prometheus :9153
        forward . 1.1.1.1 8.8.8.8 {
           max_concurrent 1000
        }
        cache 30 {
           disable success cluster.local
           disable denial cluster.local
        }
        loop
        reload
        loadbalance
    }
YAML
)
case "$dns_applied" in
    *configured*)
        kubectl --context "$CLUSTER" -n kube-system rollout restart deployment/coredns >/dev/null
        kubectl --context "$CLUSTER" -n kube-system rollout status deployment/coredns --timeout=90s >/dev/null
        echo "public names resolve in-cluster"
        ;;
    *)
        echo "already split"
        ;;
esac

# ---------- 3. images ----------
say "images"
for entry in "${REMOTE_IMAGES[@]}"; do
    # `pull|load` splits into two names; a bare reference is both.
    src="${entry%%|*}"
    dst="${entry##*|}"

    if "$CONTAINER" exec "$CLUSTER" crictl images 2>/dev/null |
        awk '{print $1":"$2}' | grep -qx "$dst"; then
        echo "in cluster: $dst"
        continue
    fi
    if [ "$src" = "$dst" ]; then
        echo "pulling $src"
    else
        echo "pulling $src (loading as $dst)"
    fi
    "$CONTAINER" image pull --platform linux/arm64 "$src" >/dev/null
    if [ "$src" != "$dst" ]; then
        "$CONTAINER" image tag "$src" "$dst" >/dev/null
    fi
    "$CONTAINER" k8s load-image --name "$CLUSTER" "$dst" >/dev/null
    # Host copies are dead weight once containerd has them, and this is the
    # single largest source of disk growth in this workflow. Both names when the
    # image was mirrored, or the saving is halved.
    "$CONTAINER" image rm "$dst" >/dev/null 2>&1 || true
    if [ "$src" != "$dst" ]; then
        "$CONTAINER" image rm "$src" >/dev/null 2>&1 || true
    fi
    echo "loaded $dst"
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
# ---------------------------------------------------------------------------
# metrics-server, so `kubectl top` answers.
#
# Not required by anything the cluster runs — it exists so resource limits can
# be set from observation rather than from a number somebody liked. Without it
# `kubectl top` fails with `Metrics API not available` and every request in this
# repository is a guess.
#
# It is the corroborating instrument rather than the primary one. It samples at
# --metric-resolution (15s by default), so it cannot see a spike shorter than
# that — and a spike shorter than that is exactly what OOMKills a pod. The true
# high-water mark is the container's own cgroup:
#
#   kubectl exec -n <ns> <pod> -- cat /sys/fs/cgroup/memory.peak
#
# Use that to size a limit. Use this for steady state and for CPU rates.
# ---------------------------------------------------------------------------
say "metrics-server (${METRICS_SERVER_VERSION:-v0.9.0})"
if kubectl --context "$CLUSTER" get deployment -n kube-system metrics-server >/dev/null 2>&1; then
    echo "metrics-server present"
else
    kubectl --context "$CLUSTER" apply -f \
        "https://github.com/kubernetes-sigs/metrics-server/releases/download/${METRICS_SERVER_VERSION:-v0.9.0}/components.yaml" >/dev/null
fi

# --kubelet-insecure-tls, added rather than shipped in the manifest.
#
# metrics-server verifies the kubelet's serving certificate against the cluster
# CA. kind does not sign kubelet certificates with it, so every scrape fails
# with an x509 error and `kubectl top` reports no metrics — which reads like the
# install not having worked.
#
# THIS FLAG IS LOCAL-ONLY. The upstream documentation calls it useful for
# testing and not recommended for production, and it must not travel to a real
# cluster: it turns off the check that the thing reporting a node's memory is
# that node. It is applied here, to this kind cluster, and is deliberately not
# part of any manifest in this repository.
#
# Appended only when absent. A second copy of the flag is not obviously
# harmless, and this script is expected to be re-run.
if ! kubectl --context "$CLUSTER" -n kube-system get deployment metrics-server \
    -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null |
    grep -q -- '--kubelet-insecure-tls'; then
    kubectl --context "$CLUSTER" -n kube-system patch deployment metrics-server --type=json \
        -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]' >/dev/null
fi
kubectl --context "$CLUSTER" -n kube-system rollout status deployment/metrics-server --timeout=120s

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
            # The console runs in this cluster, so the address it is given for
            # each organisation is a Service name and not the node. Callers are
            # outside and still get the node — see Status.PublicEndpoint.
            #
            # Set this false only if you run the console on your machine with
            # make dev-console-app, which dials from outside.
            - { name: PROVISIONER_CONSOLE_IN_CLUSTER, value: "${PROVISIONER_CONSOLE_IN_CLUSTER:-true}" }
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
          # Memory limited, CPU not — see appResources in
          # internal/cloud/provision/workloads.go for why the two differ.
          # Measured anon while provisioning three organisations at once: 15.9Mi
          # against this 128Mi, with a cgroup peak of 27.1Mi.
          resources:
            requests:
              memory: 128Mi
              cpu: 50m
            limits:
              memory: 128Mi
          volumeMounts:
            - { name: tmp, mountPath: /tmp }
      volumes:
        - name: tmp
          emptyDir: {}
EOF
kubectl --context "$CLUSTER" -n atlantis-system rollout status deployment/atlantis-provisioner --timeout=180s

# ---------------------------------------------------------------------------
# Cloud: the identity service.
#
# It mints the assertions every console verifies, so two things about it are
# load-bearing in a way the provisioner's configuration is not.
#
# THE ISSUER URL. CLOUD_ISSUER is the `iss` claim baked into every assertion,
# and CLOUD_JWKS_URL is derived from it — the address the console fetches
# verification keys from. It has to be one stable string that resolves the same
# way from the host, from a browser, and inside the cluster. So the Service
# pins its NodePort at 30500 rather than taking whatever it is given.
#
# The per-organisation Services deliberately do the opposite; the comment on
# atlantisService says "a port we pick is a port that collides with somebody
# eventually". That is about a port allocated once per tenant. This is one
# service for the whole fleet, so there is nothing for it to collide with, and
# a stable issuer is worth more than a dynamic port.
#
# THE SIGNING KEY. internal/cloud/issuer/keyfile.go loads the key or creates
# one, and its comment says losing it "would invalidate every assertion already
# in flight, silently". An emptyDir would therefore be exactly wrong: the pod
# would mint a fresh key on every restart, publish a different JWKS, and every
# existing session would fail to verify with nothing pointing at the cause. It
# comes from the host as a Secret.
# ---------------------------------------------------------------------------
say "cloud"

if ! "$CONTAINER" exec "$CLUSTER" crictl inspecti \
    docker.io/library/atlantis-cloud:local >/dev/null 2>&1 ||
    [ -z "${CLOUD_SIGNING_KEY_DATA:-}" ] || [ -z "${CLOUD_DATA_KEY:-}" ]; then
    # Says which of the two is missing. The earlier version told you to run
    # `make dev-k8s-load` — which is what you had just run, if the signing key
    # was the thing absent.
    if ! "$CONTAINER" exec "$CLUSTER" crictl inspecti \
        docker.io/library/atlantis-cloud:local >/dev/null 2>&1; then
        echo "  skipping Cloud: atlantis-cloud:local is not in the cluster."
        echo "  Run 'make dev-k8s-load', which builds it and loads it."
    else
        echo "  skipping Cloud: no signing key or data key was passed."
        echo "  'make dev-k8s-load' creates both — see dev-cloud-signing-key."
    fi
    say "ready"
    kubectl --context "$CLUSTER" get nodes
    exit 0
fi

# Mail. Cloud refuses to start with no transport at all, so one of these is
# always set.
#
# With no API key the cluster gets the logging mailer, and verification links
# appear in `kubectl logs deploy/atlantis-cloud` rather than in an inbox. Pass
# CLOUD_RESEND_API_KEY (and CLOUD_MAIL_FROM) to send for real from here.
#
# The two are mutually exclusive — Cloud refuses both, because the logging
# mailer would win and the deployment would deliver nothing while holding a
# valid key.
if [ -n "${CLOUD_RESEND_API_KEY:-}" ]; then
    CLOUD_MAIL_DEV_VALUE="false"
else
    CLOUD_MAIL_DEV_VALUE="true"
fi

# The API key goes in the Secret rather than the pod spec: it is a credential,
# and env on a Deployment is readable by anything that can read Deployments.
kubectl --context "$CLUSTER" -n atlantis-system \
    create secret generic atlantis-cloud \
    --from-literal=CLOUD_PG_URL="${CLOUD_PG_URL:?set CLOUD_PG_URL}" \
    --from-literal=CLOUD_DATA_KEY="${CLOUD_DATA_KEY:?set CLOUD_DATA_KEY}" \
    --from-literal=CLOUD_RESEND_API_KEY="${CLOUD_RESEND_API_KEY:-}" \
    --from-literal=signing-key.pem="${CLOUD_SIGNING_KEY_DATA}" \
    --dry-run=client -o yaml | kubectl --context "$CLUSTER" apply -f - >/dev/null

kubectl --context "$CLUSTER" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata:
  name: atlantis-cloud
  namespace: atlantis-system
  labels:
    app.kubernetes.io/name: atlantis-cloud
spec:
  type: NodePort
  selector: { app.kubernetes.io/name: atlantis-cloud }
  ports:
    - name: http
      port: 9500
      targetPort: 9500
      # Pinned. See the note above — this number is half of CLOUD_ISSUER.
      nodePort: ${CLOUD_NODE_PORT:-30500}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: atlantis-cloud
  namespace: atlantis-system
  labels:
    app.kubernetes.io/name: atlantis-cloud
spec:
  replicas: 1
  selector:
    matchLabels: { app.kubernetes.io/name: atlantis-cloud }
  template:
    metadata:
      labels: { app.kubernetes.io/name: atlantis-cloud }
    spec:
      # false, unlike the provisioner: Cloud never calls the Kubernetes API.
      automountServiceAccountToken: false
      hostAliases:
        - ip: "${PG_HOST_IP}"
          hostnames: ["${PG_HOST_NAME}"]
      securityContext:
        runAsNonRoot: true
        seccompProfile: { type: RuntimeDefault }
        # The signing key is mounted 0400 and owned by root, so a non-root
        # process cannot read it without being placed in its group — the same
        # reason fsGroup exists on the tenant pods.
        fsGroup: 65532
      terminationGracePeriodSeconds: 40
      containers:
        - name: cloud
          image: atlantis-cloud:local
          imagePullPolicy: IfNotPresent
          args: ["serve", "-key", "/keys/signing-key.pem", "-listen", ":9500"]
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: { drop: [ALL] }
            readOnlyRootFilesystem: true
          envFrom:
            - secretRef: { name: atlantis-cloud }
          env:
            - { name: CLOUD_ISSUER, value: "${CLOUD_ISSUER}" }
            - { name: CLOUD_PUBLIC_URL, value: "${CLOUD_PUBLIC_URL:-${CLOUD_ISSUER}}" }
            # See the note above the Secret. Without a Resend key this is the
            # logging mailer, and verification links come out of the pod log.
            - { name: CLOUD_MAIL_DEV, value: "${CLOUD_MAIL_DEV_VALUE}" }
            - { name: CLOUD_MAIL_FROM, value: "${CLOUD_MAIL_FROM:-}" }
          ports:
            - name: http
              containerPort: 9500
          livenessProbe:
            httpGet: { path: /healthz, port: 9500 }
            initialDelaySeconds: 5
            periodSeconds: 10
            failureThreshold: 6
          # Readiness reaches the database. /healthz is a bare 200 by design —
          # liveness must not restart a healthy process over a database hiccup —
          # and the JWKS route proves only that the signing key loaded. Every
          # sign-in Cloud serves is a database call, so neither answers the
          # question a load balancer is asking.
          readinessProbe:
            httpGet: { path: /readyz, port: 9500 }
            initialDelaySeconds: 3
            periodSeconds: 5
          resources:
            requests:
              memory: 128Mi
              cpu: 50m
            limits:
              memory: 128Mi
          volumeMounts:
            - { name: keys, mountPath: /keys, readOnly: true }
            - { name: tmp, mountPath: /tmp }
      volumes:
        - name: keys
          secret:
            secretName: atlantis-cloud
            items:
              - { key: signing-key.pem, path: signing-key.pem }
            defaultMode: 0440
        - name: tmp
          emptyDir: {}
EOF
kubectl --context "$CLUSTER" -n atlantis-system rollout status deployment/atlantis-cloud --timeout=180s

# ---------------------------------------------------------------------------
# The console: the management UI, and the thing "Open organisation" opens.
#
# It ran on a developer's laptop for longer than it should have. Cloud stores
# one console_url per organisation and the provisioner writes it from
# CLOUD_AUDIENCE, so while that said localhost:3000 the button worked for
# exactly one person and gave everybody else ERR_CONNECTION_REFUSED — including
# the person running a from-scratch demo on the machine that had never run
# `make dev-console-app`.
#
# ONE CONSOLE, NOT ONE PER ORGANISATION. It holds its own state — sessions,
# enrolment tokens, caller certificates, the audit log — none of which has a
# home in a tenant namespace, and it reaches each organisation over mTLS with
# credentials it unseals from its own database. Supabase's dashboard is the same
# shape for the same reason. So this is one Deployment beside Cloud rather than
# a container in every tenant pod.
#
# THE PORT IS PINNED, like Cloud's. It is half of CLOUD_AUDIENCE, which is
# compared for exact equality against the `aud` claim in every assertion and is
# copied into cloud.orgs.console_url at provisioning time. A port Kubernetes
# picked would change on re-create and silently invalidate both.
# ---------------------------------------------------------------------------
say "console"

if ! "$CONTAINER" exec "$CLUSTER" crictl inspecti \
    docker.io/library/atlantis-console:local >/dev/null 2>&1 ||
    [ -z "${CONSOLE_SESSION_SECRET:-}" ]; then
    if ! "$CONTAINER" exec "$CLUSTER" crictl inspecti \
        docker.io/library/atlantis-console:local >/dev/null 2>&1; then
        echo "  skipping the console: atlantis-console:local is not in the cluster."
        echo "  Run 'make dev-k8s-load', which builds it and loads it."
    else
        echo "  skipping the console: no session secret was passed."
        echo "  'make dev-k8s-load' creates one — see dev-session-secret."
    fi
    say "ready"
    kubectl --context "$CLUSTER" get nodes
    exit 0
fi

# CONSOLE_DATA_KEY is the same value the provisioner is given, and it has to be:
# the provisioner seals each organisation's private key with it at registration
# and the console unseals it to dial. Two different keysets produce rows that
# look complete and refuse to decrypt.
# The enrolment certificate has to name the address machines dial.
#
# Checked here rather than left to fail at `tide login`, where it arrives as
# "could not verify the server's certificate" — a message about trust that says
# nothing about the name being missing.
#
# It is easy to reach this state. init-certs.sh reissues a leaf only when the
# file is absent, so adding ATLANTIS_DOMAIN after the certificate already exists
# leaves the old names in place and changes nothing. The fix is to delete
# certs/enroll-server.* and re-run, which the message says.
ENROLL_HOST="${EXTERNAL_HOST:-atl-dev.test}"
if [ -n "${CONSOLE_ENROLL_TLS_CERT_DATA:-}" ] &&
    ! printf '%s' "${CONSOLE_ENROLL_TLS_CERT_DATA}" |
        openssl x509 -noout -checkhost "${ENROLL_HOST}" >/dev/null 2>&1; then
    echo "  the enrolment certificate does not cover ${ENROLL_HOST}, so 'tide login'"
    echo "  against this cluster would fail to verify it. Regenerate it:"
    echo "    rm -f certs/enroll-server.crt certs/enroll-server.key"
    echo "    ATLANTIS_DOMAIN=${ENROLL_HOST} make dev-certs"
    exit 1
fi

kubectl --context "$CLUSTER" -n atlantis-system \
    create secret generic atlantis-console \
    --from-literal=CONSOLE_PG_URL="${CONSOLE_PG_URL:?set CONSOLE_PG_URL}" \
    --from-literal=CONSOLE_DATA_KEY="${CONSOLE_DATA_KEY:?set CONSOLE_DATA_KEY}" \
    --from-literal=CONSOLE_SESSION_SECRET="${CONSOLE_SESSION_SECRET}" \
    --from-literal=enroll-server.crt="${CONSOLE_ENROLL_TLS_CERT_DATA:-}" \
    --from-literal=enroll-server.key="${CONSOLE_ENROLL_TLS_KEY_DATA:-}" \
    --dry-run=client -o yaml | kubectl --context "$CLUSTER" apply -f - >/dev/null

kubectl --context "$CLUSTER" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata:
  name: atlantis-console
  namespace: atlantis-system
  labels:
    app.kubernetes.io/name: atlantis-console
spec:
  type: NodePort
  selector: { app.kubernetes.io/name: atlantis-console }
  ports:
    - name: http
      port: 3000
      targetPort: 3000
      # Pinned. See the note above — this number is half of CLOUD_AUDIENCE.
      nodePort: ${CONSOLE_NODE_PORT:-30300}
    # A second port, not a second Service: it is the same process. It carries
    # only /enroll and /renew, never the pages or the API, and it terminates its
    # own TLS because /renew reads the client certificate off the connection.
    - name: enroll
      port: 3443
      targetPort: 3443
      nodePort: ${CONSOLE_ENROLL_NODE_PORT:-30443}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: atlantis-console
  namespace: atlantis-system
  labels:
    app.kubernetes.io/name: atlantis-console
spec:
  replicas: 1
  selector:
    matchLabels: { app.kubernetes.io/name: atlantis-console }
  template:
    metadata:
      labels: { app.kubernetes.io/name: atlantis-console }
    spec:
      # The console never calls the Kubernetes API. It reaches organisations
      # over mTLS at the addresses in its own database, which is why it needs no
      # cluster credentials at all and should not be handed any.
      automountServiceAccountToken: false
      hostAliases:
        - ip: "${PG_HOST_IP}"
          hostnames: ["${PG_HOST_NAME}"]
      securityContext:
        runAsNonRoot: true
        seccompProfile: { type: RuntimeDefault }
        fsGroup: 65532
      terminationGracePeriodSeconds: 40
      containers:
        - name: console
          image: atlantis-console:local
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: { drop: [ALL] }
            readOnlyRootFilesystem: true
          envFrom:
            - secretRef: { name: atlantis-console }
          env:
            # All three are required and none has a default, deliberately: an
            # empty expected issuer or audience does not fail, it skips the
            # check — so a console missing one would accept assertions from any
            # issuer, for any console.
            - { name: CLOUD_ISSUER, value: "${CLOUD_ISSUER}" }
            - { name: CLOUD_AUDIENCE, value: "${CLOUD_AUDIENCE}" }
            # The keys are fetched in-cluster, and the issuer is not.
            #
            # CLOUD_ISSUER is an identity, compared byte for byte against the
            # iss claim, so it has to be the address a browser used.
            # CLOUD_JWKS_URL is only a place to fetch from, and the verifier
            # keeps them independent for exactly this reason.
            #
            # No backticks anywhere in this block. It sits inside an unquoted
            # heredoc, so the shell expands it before kubectl ever sees it:
            # quoting a claim name the way prose would turns it into a command
            # substitution and prints "iss: command not found" mid-deploy.
            #
            # Deriving one from the other put a host-only name inside a pod:
            # atl-dev.test resolves on the machine running the browser and
            # nowhere in cluster DNS, so every sign-in failed with "cannot reach
            # the identity provider" while Cloud was healthy two pods away.
            #
            # The Service name rather than a hostAlias to the node. A hostAlias
            # would work and would carry the node's IP, which changes whenever
            # the cluster is rebuilt — the same staleness that once left
            # kube-proxy pointing at an address that had moved.
            - { name: CLOUD_JWKS_URL, value: "${CONSOLE_CLOUD_JWKS_URL:-http://atlantis-cloud.atlantis-system.svc.cluster.local:9500/.well-known/jwks.json}" }
            # Enrolment. CONSOLE_ENROLL_PUBLIC_URL is not derivable from the
            # bind address and must not be read from the Host header: the
            # console prints it into a command that carries a live token, so a
            # wrong host hands that token to whoever owns it.
            - { name: CONSOLE_ENROLL_LISTEN, value: ":3443" }
            - { name: CONSOLE_ENROLL_TLS_CERT, value: "/enroll/enroll-server.crt" }
            - { name: CONSOLE_ENROLL_TLS_KEY, value: "/enroll/enroll-server.key" }
            - { name: CONSOLE_ENROLL_PUBLIC_URL, value: "${CONSOLE_ENROLL_URL:-https://${EXTERNAL_HOST:-atl-dev.test}:${CONSOLE_ENROLL_NODE_PORT:-30443}}" }
          ports:
            - name: http
              containerPort: 3000
            - name: enroll
              containerPort: 3443
          # /api/setup/status is what the image's own HEALTHCHECK uses: it
          # answers without a session, which every other route needs.
          livenessProbe:
            httpGet: { path: /api/setup/status, port: 3000 }
            initialDelaySeconds: 5
            periodSeconds: 10
            failureThreshold: 6
          readinessProbe:
            httpGet: { path: /api/setup/status, port: 3000 }
            initialDelaySeconds: 3
            periodSeconds: 5
          resources:
            requests:
              memory: 192Mi
              cpu: 50m
            # Equal to requests, like the tenant workloads: a console that is
            # merely slow degrades every organisation's management at once.
            limits:
              memory: 192Mi
          volumeMounts:
            - { name: enroll, mountPath: /enroll, readOnly: true }
            - { name: tmp, mountPath: /tmp }
      volumes:
        - name: enroll
          secret:
            secretName: atlantis-console
            items:
              - { key: enroll-server.crt, path: enroll-server.crt }
              - { key: enroll-server.key, path: enroll-server.key }
            # 0440 with the fsGroup above, like Cloud's signing key: the private
            # key is readable by the group and by nobody else.
            defaultMode: 0440
        - name: tmp
          emptyDir: {}
EOF
kubectl --context "$CLUSTER" -n atlantis-system rollout status deployment/atlantis-console --timeout=180s

say "ready"
kubectl --context "$CLUSTER" get nodes
echo
echo "  console:  ${CLOUD_AUDIENCE}"
echo "  cloud:    ${CLOUD_ISSUER}"
echo "  enrol:    ${CONSOLE_ENROLL_URL:-https://${EXTERNAL_HOST:-atl-dev.test}:${CONSOLE_ENROLL_NODE_PORT:-30443}}"
