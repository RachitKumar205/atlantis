# syntax=docker/dockerfile:1.7

# The release host, as a container for Cloud Run.
#
# It carries no artifacts. Everything it serves lives in the Cloud Storage
# bucket named by RELEASES_BUCKET, so cutting a release is an upload and this
# image is deployed once.
#
# No build stage: the whole service is a Caddyfile. Build with
# `docker build -f deploy/releases/Dockerfile deploy/releases`, or
# `make releases-image`.
FROM caddy:2-alpine

COPY Caddyfile /etc/caddy/Caddyfile

# Caddy writes nothing at run time — admin, persist_config and automatic HTTPS
# are all off in the Caddyfile — but it still resolves these at startup, and
# the container filesystem is writable only in memory.
ENV XDG_CONFIG_HOME=/tmp XDG_DATA_HOME=/tmp

# The bucket the proxy fronts. Overridden at deploy time by --set-env-vars so
# a different bucket needs no rebuild.
ENV RELEASES_BUCKET=atlantis-releases

# Numeric, matching the other images in this repository: a kubelet enforcing
# runAsNonRoot reads image metadata and cannot resolve a name to a uid.
USER 10001

# Cloud Run injects PORT and the Caddyfile defaults to 8080 when it is unset.
EXPOSE 8080
CMD ["caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"]
