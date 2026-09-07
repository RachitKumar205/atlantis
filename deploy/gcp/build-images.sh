#!/usr/bin/env bash
# build-images.sh — build and push the platform images on Cloud Build.
#
#   PROJECT=my-project ./deploy/gcp/build-images.sh        # tag = the commit
#   PROJECT=my-project SHA=v0.6.0 ./deploy/gcp/build-images.sh
#
# The working tree is what gets built, gitignored paths excluded, so an
# uncommitted change is in the image and the tag names a commit it may not
# match. Commit first when the tag matters.
set -euo pipefail

PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
REGION="${REGION:-us-central1}"
SHA="${SHA:-$(git rev-parse --short HEAD)}"
PG_IMAGE_TAG="${PG_IMAGE_TAG:-17.11}"
cd "$(dirname "$0")/../.."

gcloud services enable cloudbuild.googleapis.com artifactregistry.googleapis.com --project "$PROJECT" >/dev/null
if ! gcloud artifacts repositories describe atlantis --location "$REGION" --project "$PROJECT" >/dev/null 2>&1; then
    gcloud artifacts repositories create atlantis --repository-format=docker --location "$REGION" --project "$PROJECT"
fi

gcloud builds submit --project "$PROJECT" --region "$REGION" \
    --config deploy/gcp/cloudbuild.yaml \
    --substitutions "_R=${REGION}-docker.pkg.dev/${PROJECT}/atlantis,_TAG=${SHA},_PG_TAG=${PG_IMAGE_TAG}" .

R="${REGION}-docker.pkg.dev/${PROJECT}/atlantis"
cat <<REFS

images pushed:
  PROVISIONER_SERVER_IMAGE=$R/atlantis-server:$SHA
  PROVISIONER_SIGNER_IMAGE=$R/atlantis-signer:$SHA
  PROVISIONER_POSTGRES_IMAGE=$R/atlantis-pg:$PG_IMAGE_TAG
  provisioner: $R/atlantis-provisioner:$SHA
  console:     $R/atlantis-console:$SHA
  cloud:       $R/atlantis-cloud:$SHA
REFS
