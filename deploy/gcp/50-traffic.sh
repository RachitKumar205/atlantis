#!/usr/bin/env bash
# 50-traffic.sh — addresses, certificates, the Ingress, and the passthrough
# load balancer every organisation's gRPC port is reached through.
#
#   PROJECT=my-project DOMAIN=example.dev ACME_EMAIL=ops@example.dev \
#   CLOUDFLARE_API_TOKEN=... ./deploy/gcp/50-traffic.sh
#
# The Cloudflare token needs Zone:DNS:Edit on the domain's zone and is stored
# in Secret Manager the first time; later runs read it from there. It ends by
# printing the four DNS records to create at Cloudflare, all "DNS only".
set -euo pipefail

PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
ZONE="${ZONE:-us-central1-a}"
REGION="${REGION:-${ZONE%-*}}"
CLUSTER="${CLUSTER:-atlantis}"
DOMAIN="${DOMAIN:-tryatlantis.dev}"
ACME_EMAIL="${ACME_EMAIL:?set ACME_EMAIL}"
CTX="gke_${PROJECT}_${ZONE}_${CLUSTER}"
cd "$(dirname "$0")"

k() { kubectl --context "$CTX" "$@"; }
say() { echo; echo "==> $*"; }

# ---------- 1. addresses ----------
say "static addresses"
if ! gcloud compute addresses describe atlantis-web --global --project "$PROJECT" >/dev/null 2>&1; then
    gcloud compute addresses create atlantis-web --global --project "$PROJECT" >/dev/null
fi
for name in atlantis-enroll atlantis-tenants; do
    if ! gcloud compute addresses describe "$name" --region "$REGION" --project "$PROJECT" >/dev/null 2>&1; then
        gcloud compute addresses create "$name" --region "$REGION" --project "$PROJECT" >/dev/null
    fi
done
WEB_IP="$(gcloud compute addresses describe atlantis-web --global --project "$PROJECT" --format='value(address)')"
ENROLL_IP="$(gcloud compute addresses describe atlantis-enroll --region "$REGION" --project "$PROJECT" --format='value(address)')"
TENANTS_IP="$(gcloud compute addresses describe atlantis-tenants --region "$REGION" --project "$PROJECT" --format='value(address)')"

# ---------- 2. the Cloudflare token for DNS-01 ----------
say "cert-manager issuer"
if [ -n "${CLOUDFLARE_API_TOKEN:-}" ] && ! gcloud secrets describe atlantis-cloudflare-api-token --project "$PROJECT" >/dev/null 2>&1; then
    gcloud secrets create atlantis-cloudflare-api-token --project "$PROJECT" --replication-policy=automatic >/dev/null
    printf '%s' "$CLOUDFLARE_API_TOKEN" | gcloud secrets versions add atlantis-cloudflare-api-token --project "$PROJECT" --data-file=- >/dev/null
fi
token="$(gcloud secrets versions access latest --secret atlantis-cloudflare-api-token --project "$PROJECT" 2>/dev/null || true)"
if [ -z "$token" ]; then
    echo "no Cloudflare API token: pass CLOUDFLARE_API_TOKEN=... once" >&2
    exit 1
fi
# A ClusterIssuer reads its Secrets from cert-manager's own namespace.
k -n cert-manager create secret generic cloudflare-api-token \
    --from-literal=api-token="$token" --dry-run=client -o yaml | k apply -f - >/dev/null

# ---------- 3. the manifests ----------
say "Ingress, managed certificate, enrolment certificate"
sed -e "s|__DOMAIN__|$DOMAIN|g" -e "s|__ACME_EMAIL__|$ACME_EMAIL|g" 50-traffic.yaml | k apply -f -

# ---------- 4. the tenant plane ----------
#
# The provisioner exposes each organisation on a NodePort and tells callers
# to dial <org>.<domain>:<port>. A wildcard record sends every organisation
# to one passthrough TCP load balancer, which forwards the whole NodePort
# range to the default node pool; TLS stays end to end.
say "tenant load balancer on $TENANTS_IP"
NODE_TAG="$(gcloud compute instances list --project "$PROJECT" --filter="name~^gke-${CLUSTER}-default-pool" --format='value(tags.items[0])' | head -1)"
MIG="$(gcloud container node-pools describe default-pool --cluster "$CLUSTER" --zone "$ZONE" --project "$PROJECT" --format='value(instanceGroupUrls)' | tr ';' '\n' | head -1)"
if [ -z "$NODE_TAG" ] || [ -z "$MIG" ]; then
    echo "cannot find the default node pool's instances or instance group" >&2
    exit 1
fi
if ! gcloud compute health-checks describe atlantis-node --region "$REGION" --project "$PROJECT" >/dev/null 2>&1; then
    # kube-proxy's health port: a node answers here when it can forward.
    gcloud compute health-checks create tcp atlantis-node --region "$REGION" --project "$PROJECT" --port 10256 >/dev/null
fi
if ! gcloud compute backend-services describe atlantis-tenants --region "$REGION" --project "$PROJECT" >/dev/null 2>&1; then
    gcloud compute backend-services create atlantis-tenants --region "$REGION" --project "$PROJECT" \
        --load-balancing-scheme=EXTERNAL --protocol=TCP --health-checks=atlantis-node --health-checks-region "$REGION" >/dev/null
    gcloud compute backend-services add-backend atlantis-tenants --region "$REGION" --project "$PROJECT" \
        --instance-group "$MIG" >/dev/null
fi
if ! gcloud compute forwarding-rules describe atlantis-tenants --region "$REGION" --project "$PROJECT" >/dev/null 2>&1; then
    gcloud compute forwarding-rules create atlantis-tenants --region "$REGION" --project "$PROJECT" \
        --load-balancing-scheme=EXTERNAL --ip-protocol=TCP --ports=30000-32767 \
        --address=atlantis-tenants --backend-service=atlantis-tenants >/dev/null
fi
if ! gcloud compute firewall-rules describe atlantis-tenants --project "$PROJECT" >/dev/null 2>&1; then
    gcloud compute firewall-rules create atlantis-tenants --project "$PROJECT" \
        --allow=tcp:30000-32767 --target-tags="$NODE_TAG" --source-ranges=0.0.0.0/0 >/dev/null
fi
if ! gcloud compute firewall-rules describe atlantis-node-health --project "$PROJECT" >/dev/null 2>&1; then
    gcloud compute firewall-rules create atlantis-node-health --project "$PROJECT" \
        --allow=tcp:10256 --target-tags="$NODE_TAG" --source-ranges=35.191.0.0/16,130.211.0.0/22,209.85.152.0/22,209.85.204.0/22 >/dev/null
fi

cat <<DONE

create these records at Cloudflare, proxy OFF ("DNS only") on every one:
  A  platform.$DOMAIN  -> $WEB_IP
  A  console.$DOMAIN   -> $WEB_IP
  A  enroll.$DOMAIN    -> $ENROLL_IP
  A  *.$DOMAIN         -> $TENANTS_IP      (every organisation; explicit records like docs. still win)

then watch:
  kubectl -n atlantis-system get managedcertificate atlantis-web     # Active once both names resolve here
  kubectl -n atlantis-system get certificate atlantis-console-enroll  # READY True after DNS-01
  kubectl -n atlantis-system get ingress atlantis-web
  kubectl -n atlantis-system rollout status deployment/atlantis-console
DONE
