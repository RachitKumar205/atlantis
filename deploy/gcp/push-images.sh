#!/usr/bin/env bash
# push-images.sh — build every platform image for linux/amd64 and push it to
# Artifact Registry, tagged with the commit.
#
# GKE nodes run amd64. The Dockerfiles pin their build stages to
# $BUILDPLATFORM, so a build from an arm64 machine runs the compilers natively
# and only the final stages are cross-built.
#
#   PROJECT=my-project ./deploy/gcp/push-images.sh
#
# Prints the six references at the end; the provisioner and the platform
# Deployments take them as their image settings.
set -euo pipefail

PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
REGION="${REGION:-us-central1}"
REPO="${REPO:-atlantis}"
# The Postgres tag must parse as a Postgres version: CloudNativePG reads the
# major version out of it and refuses anything else at admission.
PG_IMAGE_TAG="${PG_IMAGE_TAG:-17.11}"
SHA="${SHA:-$(git rev-parse --short HEAD)}"

if [ -z "$PROJECT" ] || [ "$PROJECT" = "(unset)" ]; then
    echo "PROJECT is not set and gcloud has no project configured" >&2
    exit 1
fi
cd "$(dirname "$0")/../.."

R="${REGION}-docker.pkg.dev/${PROJECT}/${REPO}"
if ! gcloud artifacts repositories describe "$REPO" --location "$REGION" --project "$PROJECT" >/dev/null 2>&1; then
    gcloud artifacts repositories create "$REPO" --repository-format=docker --location "$REGION" --project "$PROJECT"
fi
gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet

for target in server provisioner console cloud; do
    docker buildx build --platform linux/amd64 --file Dockerfile --target "$target" \
        -t "$R/atlantis-$target:$SHA" --push .
done
docker buildx build --platform linux/amd64 --file Dockerfile.signer -t "$R/atlantis-signer:$SHA" --push .
docker buildx build --platform linux/amd64 --file Dockerfile.pg -t "$R/atlantis-pg:$PG_IMAGE_TAG" --push .

cat <<REFS

images pushed:
  PROVISIONER_SERVER_IMAGE=$R/atlantis-server:$SHA
  PROVISIONER_SIGNER_IMAGE=$R/atlantis-signer:$SHA
  PROVISIONER_POSTGRES_IMAGE=$R/atlantis-pg:$PG_IMAGE_TAG
  provisioner: $R/atlantis-provisioner:$SHA
  console:     $R/atlantis-console:$SHA
  cloud:       $R/atlantis-cloud:$SHA
REFS
