#!/usr/bin/env bash
# push-images.sh — build every platform image for linux/amd64 and push it to
# Artifact Registry, tagged with the commit.
#
# GKE nodes run amd64. The proto and SPA stages run natively on the build
# machine; the Go stages need cgo and so run as amd64, under emulation on an
# arm64 machine, which is slow the first time and cached after.
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
# Docker when its daemon answers; Apple's `container` otherwise. Both build
# BuildKit Dockerfiles for linux/amd64; `container` runs the amd64 stages
# under Rosetta.
if docker version >/dev/null 2>&1; then
    gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet
    build() { docker buildx build --platform linux/amd64 --file "$1" ${2:+--target "$2"} -t "$3" --push .; }
else
    echo "docker is not running; building with Apple container under Rosetta"
    # A builder that is already running is used as it is, whatever size it was
    # started at.
    container builder status >/dev/null 2>&1 || container builder start --cpus 4 --memory 6144MB --dns 1.1.1.1
    # --dns: the build network's own resolver answers only .test and refuses
    # public names, so go install and apk fail without it.
    #
    # The login is per image: the access token lasts an hour and six emulated
    # builds can take longer, so one taken up front expires before the last
    # push.
    #
    # The builder's disk is a sparse file on the host that keeps every block
    # the guest ever wrote; a trim after each image hands the freed ones back.
    build() {
        container build --platform linux/amd64 --dns 1.1.1.1 --file "$1" ${2:+--target "$2"} -t "$3" . || return
        gcloud auth print-access-token | container registry login --username oauth2accesstoken --password-stdin "${REGION}-docker.pkg.dev" || return
        container image push --platform linux/amd64 "$3" || return
        container exec buildkit fstrim / >/dev/null 2>&1 || true
    }
fi

for target in server provisioner console cloud; do
    build Dockerfile "$target" "$R/atlantis-$target:$SHA"
done
build Dockerfile.signer "" "$R/atlantis-signer:$SHA"
build Dockerfile.pg "" "$R/atlantis-pg:$PG_IMAGE_TAG"

cat <<REFS

images pushed:
  PROVISIONER_SERVER_IMAGE=$R/atlantis-server:$SHA
  PROVISIONER_SIGNER_IMAGE=$R/atlantis-signer:$SHA
  PROVISIONER_POSTGRES_IMAGE=$R/atlantis-pg:$PG_IMAGE_TAG
  provisioner: $R/atlantis-provisioner:$SHA
  console:     $R/atlantis-console:$SHA
  cloud:       $R/atlantis-cloud:$SHA
REFS
