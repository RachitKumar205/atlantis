#!/usr/bin/env bash
# 50-traffic.sh — addresses, certificates, the Ingress, and the passthrough
# load balancer every organisation's gRPC port is reached through.
#
#   PROJECT=my-project DOMAIN=example.dev ACME_EMAIL=ops@example.dev \
#   CLOUDFLARE_API_TOKEN=... ./deploy/gcp/50-traffic.sh
#
# The Cloudflare token needs Zone:Read and Zone:DNS:Edit on the domain's zone
# and is stored in Secret Manager the first time; later runs read it from
# there. cert-manager proves DNS-01 with it, and this script writes the four
# A records with it, all "DNS only".
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
say "Cloudflare token"
if [ -n "${CLOUDFLARE_API_TOKEN:-}" ] && ! gcloud secrets describe atlantis-cloudflare-api-token --project "$PROJECT" >/dev/null 2>&1; then
    gcloud secrets create atlantis-cloudflare-api-token --project "$PROJECT" --replication-policy=automatic >/dev/null
    printf '%s' "$CLOUDFLARE_API_TOKEN" | gcloud secrets versions add atlantis-cloudflare-api-token --project "$PROJECT" --data-file=- >/dev/null
fi
token="$(gcloud secrets versions access latest --secret atlantis-cloudflare-api-token --project "$PROJECT" 2>/dev/null || true)"

# ---------- 3. the Ingress ----------
say "Ingress and managed certificate"
sed -e "s|__DOMAIN__|$DOMAIN|g" 50-traffic.yaml | k apply -f -

# ---------- 3b. the enrolment certificate ----------
#
# Needs the Cloudflare token. Without it this part is skipped, the Ingress
# and the tenant load balancer still go up, and the console pod keeps waiting
# for Secret atlantis-console-enroll; re-run once the token is stored.
say "enrolment certificate"
if [ -z "$token" ]; then
    echo "  skipped: no Cloudflare token in Secret Manager (atlantis-cloudflare-api-token)"
else
    # A ClusterIssuer reads its Secrets from cert-manager's own namespace.
    k -n cert-manager create secret generic cloudflare-api-token \
        --from-literal=api-token="$token" --dry-run=client -o yaml | k apply -f - >/dev/null
    sed -e "s|__DOMAIN__|$DOMAIN|g" -e "s|__ACME_EMAIL__|$ACME_EMAIL|g" 51-enroll-cert.yaml | k apply -f -
fi

# ---------- 4. the tenant plane ----------
#
# The provisioner exposes each organisation on a NodePort and tells callers
# to dial <org>.<domain>:<port>. A wildcard record sends every organisation
# to one passthrough TCP load balancer, which forwards the whole NodePort
# range to the default node pool; TLS stays end to end.
say "tenant load balancer on $TENANTS_IP"
NODE_TAG="$(gcloud compute instances list --project "$PROJECT" --filter="name~^gke-${CLUSTER}-default-pool" --format='value(tags.items[0])' | head -1)"
# The node pool reports its instance group manager; the backend takes the
# instance group of the same name.
MIG="$(gcloud container node-pools describe default-pool --cluster "$CLUSTER" --zone "$ZONE" --project "$PROJECT" --format='value(instanceGroupUrls)' | tr ';' '\n' | head -1 | sed 's|/instanceGroupManagers/|/instanceGroups/|')"
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
fi
if ! gcloud compute backend-services describe atlantis-tenants --region "$REGION" --project "$PROJECT" --format='value(backends)' | grep -q instanceGroups; then
    gcloud compute backend-services add-backend atlantis-tenants --region "$REGION" --project "$PROJECT" \
        --instance-group "$MIG" --instance-group-zone "$ZONE" >/dev/null
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

# ---------- 5. DNS ----------
#
# The same token cert-manager uses proves DNS-01, so it can also write the
# records. Every one is "DNS only": Google validates the managed certificate
# at the address itself, and the other two listeners carry TLS end to end.
say "DNS records at Cloudflare"
# WRITE_DNS=no, or no token, leaves the records to be created by hand; they
# are printed below either way.
# -f: without it curl exits 0 on a 4xx, and a refused write prints as one
# made.
cf() { curl -sSf -H "Authorization: Bearer $token" -H 'Content-Type: application/json' "$@"; }
if [ "${WRITE_DNS:-yes}" = "no" ] || [ -z "$token" ]; then
    echo "  skipped (WRITE_DNS=no or no token)"
else
    zone="$(cf "https://api.cloudflare.com/client/v4/zones?name=$DOMAIN" | python3 -c 'import json,sys;r=json.load(sys.stdin)["result"];print(r[0]["id"] if r else "")')"
    if [ -z "$zone" ]; then
        echo "zone $DOMAIN is not visible to this token: it needs Zone:Zone:Read" >&2
        exit 1
    fi
    upsert() {
        local name="$1" ip="$2" id body
        id="$(cf "https://api.cloudflare.com/client/v4/zones/$zone/dns_records?type=A&name=$name" | python3 -c 'import json,sys;r=json.load(sys.stdin)["result"];print(r[0]["id"] if r else "")')"
        body="{\"type\":\"A\",\"name\":\"$name\",\"content\":\"$ip\",\"ttl\":300,\"proxied\":false}"
        if [ -n "$id" ]; then
            cf -X PUT "https://api.cloudflare.com/client/v4/zones/$zone/dns_records/$id" --data "$body" >/dev/null
        else
            cf -X POST "https://api.cloudflare.com/client/v4/zones/$zone/dns_records" --data "$body" >/dev/null
        fi
        echo "  A $name -> $ip"
    }
    upsert "platform.$DOMAIN" "$WEB_IP"
    upsert "console.$DOMAIN" "$WEB_IP"
    upsert "enroll.$DOMAIN" "$ENROLL_IP"
    upsert "*.$DOMAIN" "$TENANTS_IP"
fi

cat <<DONE

records, all proxy OFF ("DNS only"):
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
