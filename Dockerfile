# syntax=docker/dockerfile:1.7

# ---------- build ----------
# Let BuildKit fill TARGETOS/TARGETARCH; hardcoded defaults break cross-arch builds.
FROM --platform=$BUILDPLATFORM golang:1.25.0-alpine3.21 AS build
ARG TARGETOS
ARG TARGETARCH

# CGO toolchain for pg_query_go (vendored C parser, statically linked).
RUN apk add --no-cache build-base

WORKDIR /src

# Cache go mod download. The SDK go.mod is needed because of the local replace.
COPY go.mod go.sum* ./
COPY clients/go/go.mod clients/go/go.sum* ./clients/go/
RUN go mod download

COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-s -w -extldflags=-static" -o /out/atlantis ./cmd/server

# ---------- runtime ----------
FROM alpine:3.21

RUN adduser -D -u 10001 atlantis && \
    apk --no-cache add ca-certificates tzdata

WORKDIR /app
COPY --from=build /out/atlantis /app/atlantis
COPY migrations /app/migrations

# Runtime user needs write access to /app for tide apply's mirror step.
RUN mkdir -p /app/schema && chown -R atlantis:atlantis /app

USER atlantis

EXPOSE 9090
ENTRYPOINT ["/app/atlantis"]
