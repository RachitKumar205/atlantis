# syntax=docker/dockerfile:1.7

# ---------- proto generation ----------
# clients/go/pb is gitignored — caller SDKs regenerate against the version of
# atlantis they target — so a fresh clone has none of the *.pb.go files that
# internal/runtime/pagination.go imports. A dedicated proto stage runs
# `buf generate` inside the image build, so a clean clone can `docker build`
# without any host-side codegen step. Cached separately from the Go build —
# proto sources change rarely.
#
# This used to be `FROM bufbuild/buf:1.41.0` and it could never have worked:
# buf.gen.yaml declares LOCAL plugins, so buf shells out to protoc-gen-go and
# protoc-gen-go-grpc, and that image ships neither. The stage failed with
# `plugin protoc-gen-go: executable file not found in $PATH` and nothing caught
# it because no CI job builds this image.
#
# The plugin versions are the ones `make proto` pins on the host. They are
# duplicated rather than shared because a Dockerfile cannot read the Makefile —
# if you change one, change the other.
FROM --platform=$BUILDPLATFORM golang:1.26.4-alpine AS proto
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go install github.com/bufbuild/buf/cmd/buf@v1.41.0 && \
    go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6 && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
WORKDIR /src
COPY buf.yaml buf.gen.yaml ./
COPY atlantis ./atlantis
RUN buf generate

# ---------- SPA ----------
# The two React applications: the console's pages and Cloud's sign-in pages.
#
# Built in one stage because they are one npm workspace with one lockfile at the
# root. `npm ci` installs the whole tree and fails if a member named in the
# lockfile is missing, so every member's package.json has to be present — which
# is why all three are copied before any source, and why a change to a component
# does not invalidate the install layer.
#
# Nothing that does not embed a SPA depends on this stage. See the note on
# build-spa below for why that is arranged the way it is.
FROM --platform=$BUILDPLATFORM node:22-alpine AS spa
WORKDIR /app
COPY package.json package-lock.json ./
COPY web/console/package.json ./web/console/
COPY web/cloud/package.json ./web/cloud/
COPY web/shared/package.json ./web/shared/
RUN npm ci --prefer-offline

COPY web/shared/ ./web/shared/
COPY web/console/ ./web/console/
COPY web/cloud/ ./web/cloud/
# Each vite config writes to ../../cmd/<name>/dist, so these land at
# /app/cmd/console/dist and /app/cmd/cloud/dist inside this stage.
RUN npm run build --workspace web/console && \
    npm run build --workspace web/cloud

# ---------- build ----------
# Use BUILDPLATFORM so the compiler runs natively on the host (ARM64 on Apple
# Silicon, amd64 on CI). pg_query_go's vendored C parser compiles fine on both
# architectures with musl + build-base. For a forced amd64 production image,
# pass --platform linux/amd64 to docker build or use a CI runner.
FROM --platform=$BUILDPLATFORM golang:1.26.4-alpine AS build

# CGO toolchain for pg_query_go (vendored C parser, statically linked).
RUN apk add --no-cache build-base

WORKDIR /src

# Cache go mod download. The SDK go.mod is needed because of the local replace.
COPY go.mod go.sum* ./
COPY clients/go/go.mod clients/go/go.sum* ./clients/go/
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
# Pull in the freshly-generated pb stubs from the proto stage. Has to land
# AFTER `COPY . .` so a stale local clients/go/pb/ in the build context
# doesn't shadow the fresh generate.
COPY --from=proto /src/clients/go/pb ./clients/go/pb

# VERSION is stamped into the binary (-X main.version) and surfaced in the
# startup log. Pass --build-arg VERSION=<tag-or-sha>; defaults to "dev".
ARG VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=1 \
    go build -ldflags="-s -w -extldflags=-static -X main.version=${VERSION}" \
    -o /out/atlantis ./cmd/server

# The provisioner is built here rather than in a file of its own, and that is
# not to save a file.
#
# cmd/provisioner reaches internal/cloud/store, and through it both the
# generated protobuf package and pg_query_go — so it needs the proto stage above
# AND CGO, which makes its build identical to the server's in every respect that
# matters. A Dockerfile.provisioner would have to repeat the proto stage
# verbatim, including the plugin versions whose comment already says they are
# duplicated from the Makefile by hand. A third copy of a pin that nothing
# enforces is worse than a second binary in one stage.
#
# No -X: cmd/provisioner declares no version symbol, and the linker discards a
# -X for a symbol that does not exist without saying so.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=1 \
    go build -ldflags="-s -w -extldflags=-static" \
    -o /out/provisioner ./cmd/provisioner

# ---------- build: the binaries that carry a SPA ----------
# A second build stage, deriving from the first, and the split is the reason
# consolidating these Dockerfiles costs nothing.
#
# BuildKit builds only the stages a --target depends on. The server and the
# provisioner take their binaries from `build`, which knows nothing about
# `spa` — so `--target server` never starts Node, never runs npm ci, and builds
# exactly what it did before these two arrived.
#
# Putting the dist COPYs into `build` instead would have made every image in
# this file wait on a React build to produce a gRPC server.
FROM build AS build-spa
COPY --from=spa /app/cmd/console/dist ./cmd/console/dist/
COPY --from=spa /app/cmd/cloud/dist ./cmd/cloud/dist/

# -tags embedspa bakes the pages in, and it is deliberately not the default: an
# untagged build has no //go:embed directive at all, which is what lets a clean
# checkout compile with no Node installed. These are shipped binaries, so they
# opt in. Built without it they answer 404 on the SPA routes, naming the make
# target — see cmd/cloud/spa_none.go.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=1 \
    go build -tags embedspa -ldflags="-s -w -extldflags=-static" \
    -o /out/atlantis-console ./cmd/console && \
    go build -tags embedspa -ldflags="-s -w -extldflags=-static" \
    -o /out/atlantis-cloud ./cmd/cloud

# ---------- runtime: server ----------
# Named, because this file now produces two images. `make build-server-image`
# passes --target server. Without a target a build takes the LAST stage, which
# would silently produce the provisioner under the server's tag.
FROM alpine:3.21 AS server

RUN adduser -D -u 10001 atlantis && \
    apk --no-cache add ca-certificates tzdata

WORKDIR /app
COPY --from=build /out/atlantis /app/atlantis

# No `COPY migrations` for the server's own schema: migrations/infra is embedded
# in the binary, so the image cannot carry a version of it that disagrees with
# the code. MIGRATIONS_DIR now names only the tidectl-emitted tree, which a
# deployment mounts because it writes it after this image is built.

# /app/schema is writable so `tide apply` can mirror submitted .atl files when
# ATL_MIRROR_SCHEMA=true, which is a local-development aid — see the
# configuration reference. Owned by the non-root atlantis user so the server
# never runs as root.
RUN mkdir -p /app/schema && chown -R atlantis:atlantis /app

# Numeric, not `USER atlantis`. The kubelet enforces `runAsNonRoot: true` from
# image metadata alone, before the container runs, so it cannot resolve a name to
# a uid and refuses the image: "container has runAsNonRoot and image has
# non-numeric user". The pod stays in CreateContainerConfigError. The name still
# exists — adduser above created it, and the chown uses it — this line just says
# the same thing in the form the kubelet can check.
USER 10001

# 9090 gRPC; 8081 health/metrics (/healthz, /readyz, /metrics).
EXPOSE 9090 8081

# Readiness reflects true serving state (pg + memcached + outbox liveness), so
# a container only reports healthy once it can actually serve. start-period
# covers boot + AUTO_MIGRATE. Orchestrators should still wire the dedicated
# /healthz (liveness) and /readyz (readiness) probes directly rather than rely
# on this single signal. busybox wget exits non-zero on a 503, so no jq needed.
# https, and --no-check-certificate deliberately.
#
# The health listener terminates TLS so that /status and /metrics can demand a
# client certificate. /readyz does not, but it is on the same listener, so the
# scheme changed with it. The container has no copy of the organisation's CA and
# should not need one to check on itself — this probe asks "is this process
# serving", not "is this the right process", and it dials 127.0.0.1 where there
# is nothing to impersonate.
HEALTHCHECK --start-period=20s --interval=15s --timeout=5s --retries=3 \
    CMD wget -q -O - --no-check-certificate https://127.0.0.1:8081/readyz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/atlantis"]

# ---------- runtime: provisioner ----------
# Build with --target provisioner. See the note on the server stage above about
# what happens without a target.
FROM alpine:3.21 AS provisioner

# The same uid as the server, and numeric for the same reason: the kubelet
# enforces `runAsNonRoot` from image metadata before the container starts, so it
# cannot resolve a name to a uid and refuses the image outright. See
# nonRootPodSecurity in internal/cloud/provision/workloads.go.
RUN adduser -D -u 10001 provisioner && \
    apk --no-cache add ca-certificates tzdata

WORKDIR /app
COPY --from=build /out/provisioner /app/provisioner
USER 10001

# No EXPOSE and no HEALTHCHECK.
#
# The provisioner serves one HTTP listener for /healthz and /readyz, on an
# address that is configuration rather than a constant, so a port baked in here
# would be a second answer that can disagree with the first. Its Deployment
# names the port it actually binds, and the probes go there.
ENTRYPOINT ["/app/provisioner"]

# ---------- runtime: console ----------
# Build with --target console.
#
# This stage used to live in Dockerfile.console, which carried its own copy of
# the proto stage above — a second set of the plugin version pins that the
# comment at the top of this file already says are duplicated from the Makefile
# by hand. Adding Cloud would have made three. They are one stage now.
FROM alpine:3.21 AS console

# The uid was `USER app` when this stage lived in its own file: a NAME, which
# the kubelet cannot resolve from image metadata and therefore refuses under
# `runAsNonRoot` with CreateContainerConfigError. It never fired because the
# console is not deployed anywhere yet. Corrected here rather than carried
# across, because moving a latent fault is not the same as moving a stage.
RUN adduser -D -u 10001 console && \
    apk --no-cache add ca-certificates tzdata

COPY --from=build-spa /out/atlantis-console /usr/local/bin/atlantis-console
USER 10001

EXPOSE 3000
HEALTHCHECK --interval=10s --timeout=5s --retries=3 \
    CMD wget -q -O - http://127.0.0.1:3000/api/setup/status >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/atlantis-console"]

# ---------- runtime: cloud ----------
# Build with --target cloud.
FROM alpine:3.21 AS cloud

RUN adduser -D -u 10001 cloud && \
    apk --no-cache add ca-certificates tzdata

COPY --from=build-spa /out/atlantis-cloud /usr/local/bin/atlantis-cloud
USER 10001

# No HEALTHCHECK. Cloud's /healthz is a bare 200 and its readiness is better
# expressed by the JWKS route, but both are probes its Deployment declares
# against the port it actually binds — CLOUD_LISTEN is configuration, and a port
# written here would be a second answer free to disagree with it.
EXPOSE 9500

ENTRYPOINT ["/usr/local/bin/atlantis-cloud"]
