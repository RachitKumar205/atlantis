# syntax=docker/dockerfile:1.7

# The documentation site, as a container for Cloud Run.
#
# The build context is the repository root, not web/docs: there is one
# package-lock.json for every workspace member, and `npm ci` reads it from
# there. Build with `docker build -f web/docs/Dockerfile .`
#
# ---------- build ----------
# Every member's package.json has to be present because `npm ci` fails on a
# member the lockfile names but the context lacks. The install is then scoped
# to web/docs, so React and the two SPAs stay out of this image.
#
# Node 24 for the npm it bundles: 11.19.0. node:22 ships npm 10.9.8, whose
# arborist cannot resolve this lockfile — the install runs for a minute and
# dies on "Exit handler never called", and the same version fails the same way
# on the host.
#
# Debian slim. Only dist/ leaves this stage, so the base's size never reaches
# the runtime image.
FROM --platform=$BUILDPLATFORM node:24-bookworm-slim AS build
WORKDIR /app

COPY package.json package-lock.json ./
COPY web/console/package.json ./web/console/
COPY web/cloud/package.json ./web/cloud/
COPY web/shared/package.json ./web/shared/
COPY web/docs/package.json ./web/docs/
RUN npm ci --prefer-offline \
      --workspace web/docs \
      --include-workspace-root

# @atlantis/shared carries the design tokens the site imports; it is a
# workspace member rather than a published package, so its source is needed.
COPY web/shared/ ./web/shared/
COPY web/docs/ ./web/docs/
RUN npm run build --workspace web/docs

# ---------- runtime ----------
# Caddy rather than nginx: Cloud Run runs containers with reduced privileges
# and no setuid, and stock nginx wants to write its pid and temp directories
# as root. Caddy serves happily as an unprivileged user on a high port.
FROM caddy:2-alpine

COPY web/docs/Caddyfile /etc/caddy/Caddyfile
COPY --from=build /app/web/docs/dist /srv

# Caddy writes nothing at run time — admin, persist_config and automatic
# HTTPS are all off in the Caddyfile — but it still resolves these at startup,
# and the container filesystem is writable only in memory.
ENV XDG_CONFIG_HOME=/tmp XDG_DATA_HOME=/tmp

# Numeric, matching the other images in this repository: a kubelet enforcing
# runAsNonRoot reads image metadata and cannot resolve a name to a uid.
USER 10001

# Cloud Run injects PORT and the Caddyfile defaults to 8080 when it is unset.
EXPOSE 8080
CMD ["caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"]
