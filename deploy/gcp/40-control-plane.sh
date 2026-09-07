#!/usr/bin/env bash
# 40-control-plane.sh — deploy Cloud, the console and the provisioner.
#
#   PROJECT=my-project DOMAIN=example.dev SHA=<commit> ./deploy/gcp/40-control-plane.sh
#
# SHA is the tag step 1 used; it defaults to the current commit.
# The console pod stays in ContainerCreating until 50-traffic.sh has issued
# its enrolment certificate, which is expected on the first pass.
set -euo pipefail

PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
ZONE="${ZONE:-us-central1-a}"
REGION="${REGION:-${ZONE%-*}}"
CLUSTER="${CLUSTER:-atlantis}"
DOMAIN="${DOMAIN:-tryatlantis.dev}"
SHA="${SHA:-$(git rev-parse --short HEAD)}"
PG_IMAGE_TAG="${PG_IMAGE_TAG:-17.11}"
CTX="gke_${PROJECT}_${ZONE}_${CLUSTER}"
R="${REGION}-docker.pkg.dev/${PROJECT}/atlantis"
cd "$(dirname "$0")"

k() { kubectl --context "$CTX" "$@"; }

# The enrolment address is reserved here or by 50-traffic.sh, whichever runs
# first; both check before creating.
if ! gcloud compute addresses describe atlantis-enroll --region "$REGION" --project "$PROJECT" >/dev/null 2>&1; then
    gcloud compute addresses create atlantis-enroll --region "$REGION" --project "$PROJECT" >/dev/null
fi
ENROLL_IP="$(gcloud compute addresses describe atlantis-enroll --region "$REGION" --project "$PROJECT" --format='value(address)')"
POD_CIDR="$(gcloud container clusters describe "$CLUSTER" --zone "$ZONE" --project "$PROJECT" --format='value(clusterIpv4Cidr)')"

resend="$(k -n atlantis-system get secret atlantis-cloud -o jsonpath='{.data.CLOUD_RESEND_API_KEY}' | base64 -d)"
if [ -n "$resend" ]; then MAIL_DEV=false; else MAIL_DEV=true; fi
MAIL_FROM="${CLOUD_MAIL_FROM:-no-reply@${DOMAIN}}"

sed -e "s|__CLOUD_IMAGE__|$R/atlantis-cloud:$SHA|g" \
    -e "s|__CONSOLE_IMAGE__|$R/atlantis-console:$SHA|g" \
    -e "s|__PROVISIONER_IMAGE__|$R/atlantis-provisioner:$SHA|g" \
    -e "s|__SERVER_IMAGE__|$R/atlantis-server:$SHA|g" \
    -e "s|__SIGNER_IMAGE__|$R/atlantis-signer:$SHA|g" \
    -e "s|__PG_IMAGE__|$R/atlantis-pg:$PG_IMAGE_TAG|g" \
    -e "s|__DOMAIN__|$DOMAIN|g" \
    -e "s|__POD_CIDR__|$POD_CIDR|g" \
    -e "s|__ENROLL_IP__|$ENROLL_IP|g" \
    -e "s|__MAIL_DEV__|$MAIL_DEV|g" \
    -e "s|__MAIL_FROM__|$MAIL_FROM|g" \
    40-control-plane.yaml | k apply -f -

k -n atlantis-system rollout status deployment/atlantis-cloud --timeout=300s
k -n atlantis-system rollout status deployment/atlantis-provisioner --timeout=300s
echo
echo "cloud and provisioner are up. the console waits for its enrolment certificate: run 50-traffic.sh"
echo "  enroll address: $ENROLL_IP   pod range: $POD_CIDR   mail: $([ "$MAIL_DEV" = false ] && echo Resend || echo 'logging mailer')"
