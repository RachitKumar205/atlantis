# syntax=docker/dockerfile:1.7

# tryatlantis.dev, as a container for Cloud Run.
#
# The build context is the repository root, which holds the one
# package-lock.json. Build with `docker build -f web/site/Dockerfile .`

# `npm ci` fails when a lockfile member's package.json is absent.
#
# Node 24 for its npm: node:22's npm 10.9.8 cannot resolve this lockfile
# (see web/docs/Dockerfile).
FROM --platform=$BUILDPLATFORM node:24-bookworm-slim AS build
WORKDIR /app

COPY package.json package-lock.json ./
COPY web/console/package.json ./web/console/
COPY web/cloud/package.json ./web/cloud/
COPY web/shared/package.json ./web/shared/
COPY web/docs/package.json ./web/docs/
COPY web/site/package.json ./web/site/
RUN npm ci --prefer-offline \
      --workspace web/site \
      --include-workspace-root

# The site imports @atlantis/shared from source.
COPY web/shared/ ./web/shared/
COPY web/site/ ./web/site/
RUN npm run build --workspace web/site

FROM caddy:2-alpine

COPY web/site/Caddyfile /etc/caddy/Caddyfile
COPY --from=build /app/web/site/dist /srv

# Caddy writes nothing at run time, but resolves these at startup, and the
# container filesystem is writable only in memory.
ENV XDG_CONFIG_HOME=/tmp XDG_DATA_HOME=/tmp

# Numeric: a kubelet enforcing runAsNonRoot cannot resolve a name to a uid.
USER 10001

EXPOSE 8080
CMD ["caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"]
