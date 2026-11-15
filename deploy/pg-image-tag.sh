#!/bin/sh
# pg-image-tag.sh — prints the atlantis-pg image tag, <postgres>-ts<timescaledb>,
# from the versions Dockerfile.pg pins.
#
# CNPG reads the Postgres version from the leading digits and dots of the tag,
# and refuses a tag without one at admission:
#   spec.imageName: Invalid value: "...:local": invalid version tag
set -eu
f="$(dirname "$0")/../Dockerfile.pg"
pg="$(sed -n 's/^ARG PG_VERSION=//p' "$f")"
ts="$(sed -n 's/^ARG TIMESCALE_VERSION=//p' "$f")"
if [ -z "$pg" ] || [ -z "$ts" ]; then
    echo "$f: no ARG PG_VERSION= or ARG TIMESCALE_VERSION= line" >&2
    exit 1
fi
echo "${pg}-ts${ts%%~*}"
