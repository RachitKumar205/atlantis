# atlantis Makefile

SHELL := /bin/bash

# Load .env if present; export so recipes inherit.
-include .env
export

PG_URL ?= postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable

# The console connects as its own role, because it refuses to start on one that
# reads through row-level security — which the `atlantis` dev role does, being a
# superuser. `make dev-console-role` creates it.
CONSOLE_PG_ROLE     ?= atlantis_console
CONSOLE_PG_PASSWORD ?= console
CONSOLE_PG_URL      ?= postgres://$(CONSOLE_PG_ROLE):$(CONSOLE_PG_PASSWORD)@localhost:5432/atlantis?sslmode=disable

# Identity. The console verifies every sign-in against Atlantis Cloud, with no
# local accounts and no development bypass, so these are required to start it —
# `make dev-auth` runs the issuer these values point at.
CLOUD_LISTEN       ?= :9500
CLOUD_ISSUER       ?= http://localhost:9500
CLOUD_AUDIENCE     ?= http://localhost:3000
CLOUD_JWKS_URL     ?= $(CLOUD_ISSUER)/.well-known/jwks.json
CLOUD_SIGNING_KEY  ?= $(DEV_CERT_DIR)/cloud-signing-key.pem

# Cloud's own database: accounts, organisations and memberships.
#
# Its own DSN, and locally its own schema in the same instance the console uses.
# That is a development convenience rather than a constraint — nothing joins
# across the two, so separating them later is this line and nothing else.
#
# The dev role here is the administrative one, which is a superuser. That is
# fine while every table in schema cloud is exempt from the user boundary, and
# stops being fine the moment one is not: store.VerifyPolicies refuses to start
# on a bypassing role as soon as a policed table exists, so the first policed
# table is also the point at which this needs its own NOSUPERUSER role.
CLOUD_PG_URL       ?= postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable

# The base every emailed link is built from. Required, with no default in the
# product — a wrong value does not fail, it sends every user a working link to
# the wrong host. Locally it is wherever `make dev-auth` is listening.
CLOUD_PUBLIC_URL   ?= http://localhost:9500

# Two migration histories: infra (hand-written) and tidectl (codegen).
MIGRATIONS_INFRA_DIR := ./migrations/infra
MIGRATIONS_TIDECTL_DIR := ./.dev/migrations/tidectl
MIGRATE_URL_INFRA := $(PG_URL)&search_path=public&x-migrations-table=atlantis_schema_migrations_infra
MIGRATE_URL_TIDECTL := $(PG_URL)&search_path=public&x-migrations-table=atlantis_schema_migrations_tidectl

GO ?= go
GOFLAGS ?=

BIN_DIR := ./bin

# Docker image tag and VERSION string for `make image`.
# VERSION is passed as --build-arg to the Dockerfile, baked into the binary
# via -ldflags '-X main.version=...', and emitted on startup. Default is
# `git describe`, which is a non-semver SHA — `make release-clis-native` rejects
# it and requires an explicit v-prefixed tag.
IMAGE   ?= atlantis:local
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)

.PHONY: help
help:
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-22s %s\n", $$1, $$2}'

# ---------- build ----------

.PHONY: build
build: build-server build-tidectl build-tide ## Build the three Go binaries (server, tidectl, tide); does not build the console SPA, console image, or signer image

.PHONY: build-server
build-server: ## Build the gRPC server
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/atlantis ./cmd/server

.PHONY: build-tidectl
build-tidectl: ## Build the server-side admin CLI
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/tidectl ./cmd/tidectl

.PHONY: build-tide
build-tide: ## Build the caller-side CLI
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/tide ./cmd/tide

# Output directory for release artifacts. Gitignored via /dist/.
RELEASE_DIR := dist

# CLIs published per platform. Each entry produces a tarball named
# `<cli>-<version>-<os>-<arch>.tar.gz` under dist/.
#
# The two CLIs ship differently because only one of them needs cgo.
# tide is a thin client: plan and apply send raw .atl bytes and the
# server does all parsing, so nothing in tide reaches pg_query_go and it
# cross-compiles to every platform from one runner. tidectl validates
# SQL locally (sqlvalidate -> pg_query_go -> libpg_query), so it needs a
# real toolchain per target; it runs on the server host, which is Linux.
RELEASE_TIDE_PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64
RELEASE_CGO_CLIS       := tidectl

# Resolved native host platform (used by release-clis-native to label
# the output tarballs). Override with GOOS / GOARCH when invoking.
NATIVE_OS   := $(shell $(GO) env GOOS)
NATIVE_ARCH := $(shell $(GO) env GOARCH)

# `make release-tide VERSION=v0.4.0` cross-compiles tide for every
# platform in RELEASE_TIDE_PLATFORMS from whatever host runs it, with
# CGO off. No cross-toolchain is involved: tide has no cgo dependency,
# so this is a pure-Go build matrix a single runner completes in
# seconds. This is the artifact end users install.
.PHONY: release-tide
release-tide: ## Cross-compile tide tarballs for every platform: make release-tide VERSION=v0.4.0
	@case "$(VERSION)" in v[0-9]*) : ;; *) \
	  echo "Usage: make release-tide VERSION=v0.4.0"; \
	  echo "       (got '$(VERSION)' — must look like v0.4.0)"; \
	  exit 1 ;; esac
	@mkdir -p $(RELEASE_DIR)
	@for plat in $(RELEASE_TIDE_PLATFORMS); do \
	  os=$${plat%%/*}; arch=$${plat##*/}; \
	  ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	  out=$(RELEASE_DIR)/tide-$(VERSION)-$$os-$$arch; \
	  echo "==> building $$out"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    $(GO) build -trimpath \
	      -ldflags "-s -w -X main.version=$(VERSION)" \
	      -o $$out/tide$$ext ./cmd/tide || exit 1; \
	  cp LICENSE $$out/LICENSE 2>/dev/null || true; \
	  tar -czf $$out.tar.gz -C $(RELEASE_DIR) $$(basename $$out); \
	  rm -rf $$out; \
	done
	@echo ""
	@echo "==> $(RELEASE_DIR)/ (tide, all platforms)"
	@ls -la $(RELEASE_DIR)/

# `make release-clis-native VERSION=v0.4.0` builds the cgo-requiring
# CLIs for the HOST OS+arch and lays out tarballs under dist/. tidectl
# links libpg_query, so each target needs its own toolchain; the
# workflow (.github/workflows/release-clis.yml) runs this once per
# native runner and aggregates in a final job. Operators install with
# `curl -L … | tar xz` — no go toolchain needed.
.PHONY: release-clis-native
release-clis-native: ## Build cgo CLI tarballs for the native host platform: make release-clis-native VERSION=v0.4.0
	@case "$(VERSION)" in v[0-9]*) : ;; *) \
	  echo "Usage: make release-clis-native VERSION=v0.4.0"; \
	  echo "       (got '$(VERSION)' — must look like v0.4.0)"; \
	  exit 1 ;; esac
	@mkdir -p $(RELEASE_DIR)
	@for cli in $(RELEASE_CGO_CLIS); do \
	  out=$(RELEASE_DIR)/$$cli-$(VERSION)-$(NATIVE_OS)-$(NATIVE_ARCH); \
	  echo "==> building $$out"; \
	  CGO_ENABLED=1 \
	    $(GO) build -trimpath \
	      -ldflags "-s -w -X main.version=$(VERSION)" \
	      -o $$out/$$cli ./cmd/$$cli || exit 1; \
	  cp LICENSE $$out/LICENSE 2>/dev/null || true; \
	  tar -czf $$out.tar.gz -C $(RELEASE_DIR) $$(basename $$out); \
	  rm -rf $$out; \
	done
	@echo ""
	@echo "==> $(RELEASE_DIR)/ (native: $(NATIVE_OS)/$(NATIVE_ARCH))"
	@ls -la $(RELEASE_DIR)/

.PHONY: build-console
build-console: ## Build the management console binary (requires SPA built first)
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/atlantis-console ./cmd/console

.PHONY: build-console-spa
build-console-spa: ## Build the console React SPA and write output to cmd/console/dist/
	@which npm >/dev/null || (echo "install Node.js: https://nodejs.org" && exit 1)
	cd web/console && npm ci && npm run build

.PHONY: build-console-image
build-console-image: ## Build the atlantis-console Docker image
	docker build --file Dockerfile.console -t atlantis-console:local .

.PHONY: build-signer-image
build-signer-image: ## Build the atlantis-signer Docker image (cert signing service)
	docker build --file Dockerfile.signer -t atlantis-signer:local .

# ---------- codegen ----------

.PHONY: codegen
codegen: ## Regenerate proto, server, client, sql from .atl; then buf generate; then build
	$(GO) run ./cmd/tidectl codegen
	@$(MAKE) proto
	# Build SDK in isolation so any leak from clients/go/ into internal/ fails here.
	cd clients/go && $(GO) build ./...
	# gen/ is optional on fresh clones (no .atl fixtures → no generated code).
	$(GO) build ./cmd/tide ./cmd/tidectl ./internal/...
	# Check for actual .go files, not just dir existence — an empty
	# leftover gen/ from a previous run would otherwise trip the build.
	@if [ -n "$$(find gen -type f -name '*.go' 2>/dev/null)" ]; then \
		$(GO) build ./gen/... ./cmd/server; \
	fi

.PHONY: proto
proto: ## Run buf generate against the regenerated .proto tree
	@which buf >/dev/null || (echo "install buf: brew install bufbuild/buf/buf" && exit 1)
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6
	$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
	buf lint
	buf generate

.PHONY: plan
plan: ## Stage a migration from current .atl state
	$(GO) run ./cmd/tidectl plan

.PHONY: approve
approve: ## Promote staged migration into migrations/
	$(GO) run ./cmd/tidectl promote

# ---------- migrate ----------
#
# Two histories: infra first (the outbox + tidectl bookkeeping the rest of
# the system depends on), then tidectl (the codegen-emitted entity tables).

.PHONY: migrate-up
migrate-up: migrate-up-infra migrate-up-tidectl ## Apply all pending migrations (infra then tidectl)

.PHONY: migrate-up-infra
migrate-up-infra: ## Apply pending infra migrations
	@which migrate >/dev/null || (echo "install golang-migrate: brew install golang-migrate" && exit 1)
	migrate -path $(MIGRATIONS_INFRA_DIR) -database "$(MIGRATE_URL_INFRA)" up

.PHONY: migrate-up-tidectl
migrate-up-tidectl: ## Apply pending tidectl-emitted migrations
	migrate -path $(MIGRATIONS_TIDECTL_DIR) -database "$(MIGRATE_URL_TIDECTL)" up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent tidectl migration
	migrate -path $(MIGRATIONS_TIDECTL_DIR) -database "$(MIGRATE_URL_TIDECTL)" down 1

.PHONY: migrate-down-infra
migrate-down-infra: ## Roll back the most recent infra migration
	migrate -path $(MIGRATIONS_INFRA_DIR) -database "$(MIGRATE_URL_INFRA)" down 1

.PHONY: migrate-status
migrate-status: ## Show migration versions (both histories)
	@echo "infra:"
	@migrate -path $(MIGRATIONS_INFRA_DIR) -database "$(MIGRATE_URL_INFRA)" version
	@echo "tidectl:"
	@migrate -path $(MIGRATIONS_TIDECTL_DIR) -database "$(MIGRATE_URL_TIDECTL)" version

.PHONY: migrate-create-infra
migrate-create-infra: ## Create a blank infra migration: make migrate-create-infra NAME=description
	@test -n "$(NAME)" || (echo "NAME=description required" && exit 1)
	migrate create -ext sql -dir $(MIGRATIONS_INFRA_DIR) -seq $(NAME)

# ---------- test ----------

.PHONY: test
test: ## Run unit tests
	$(GO) test ./...

.PHONY: test-integration
test-integration: ## Run integration tests (testcontainers PG + memcached fake)
	$(GO) test -tags=integration ./tests/integration/...

.PHONY: test-codegen-golden
test-codegen-golden: ## Run codegen golden-file tests
	$(GO) test ./internal/codegen/...

# ---------- dev ----------

# Local mTLS material, gitignored. atlantis accepts no plaintext connection, so
# this is the first step of any local run — the server, the console and every
# CLI need certificates.
#
# The same script a deployed stack runs, so a developer's trust setup has the
# shape the real one does. It is incremental: unchanged files are left alone, a
# missing leaf is reissued without disturbing the CA, and the server cert is
# reissued when ATLANTIS_DOMAIN stops being covered by it.
DEV_CERT_DIR ?= ./certs

# The keyset the console seals organisation credentials with, kept beside the
# certificates because it is the same kind of thing and the directory is
# already gitignored. See dev-data-key for why it is a file and not minted per
# run.
DEV_DATA_KEY_FILE ?= $(DEV_CERT_DIR)/console-data-key

.PHONY: dev-certs
dev-certs: ## Generate the local CA + server and console certs into ./certs
	@which openssl >/dev/null 2>&1 || (echo "openssl not found"; exit 1)
	CERT_DIR="$(DEV_CERT_DIR)" \
		CA_PRIVATE_DIR="$(DEV_CERT_DIR)/ca-private" \
		sh deploy/init-certs.sh

# dev-server, not dev, when Postgres and memcached are already running.
#
# `dev` starts the compose services first, which fails outright if something
# else already holds 5432 — a Postgres container started by hand, or one from
# another project. That is a working setup, not a broken one, so it gets a
# target rather than an error: PG_URL points wherever you like and this runs
# the server against it.
.PHONY: dev-server
dev-server: dev-certs ## Run the server against Postgres/memcached you started yourself
	AUTO_MIGRATE=true \
		ATL_MIRROR_SCHEMA=true \
		ATL_ALLOW_APPLY_MUTATION=true \
		TLS_CERT_FILE="$(DEV_CERT_DIR)/server.crt" \
		TLS_KEY_FILE="$(DEV_CERT_DIR)/server.key" \
		TLS_CA_FILE="$(DEV_CERT_DIR)/ca.crt" \
		$(GO) run ./cmd/server

.PHONY: dev
dev: dev-certs ## Start compose Postgres + memcached, then run the server
	docker compose up -d postgres memcached
	AUTO_MIGRATE=true \
		ATL_MIRROR_SCHEMA=true \
		ATL_ALLOW_APPLY_MUTATION=true \
		TLS_CERT_FILE="$(DEV_CERT_DIR)/server.crt" \
		TLS_KEY_FILE="$(DEV_CERT_DIR)/server.key" \
		TLS_CA_FILE="$(DEV_CERT_DIR)/ca.crt" \
		$(GO) run ./cmd/server

.PHONY: dev-console
dev-console: dev-certs dev-console-role dev-data-key build-console ## Run the management console BFF against the local dev server
	CONSOLE_PG_URL="$(CONSOLE_PG_URL)" \
		CONSOLE_SESSION_SECRET="$${CONSOLE_SESSION_SECRET:-dev-secret-change-in-prod-32chars!!}" \
		CONSOLE_LISTEN=":3000" \
		CONSOLE_COOKIE_SECURE=false \
		CONSOLE_DATA_KEY="$$(cat $(DEV_DATA_KEY_FILE))" \
		CLOUD_ISSUER="$(CLOUD_ISSUER)" \
		CLOUD_AUDIENCE="$(CLOUD_AUDIENCE)" \
		CLOUD_JWKS_URL="$(CLOUD_JWKS_URL)" \
		$(BIN_DIR)/atlantis-console

# ── Per-organisation atlantis registration ─────────────────────────────────
#
# The console no longer reads ATL_ENDPOINT or a certificate from its
# environment. One console serves many organisations, each with its own
# atlantis behind its own CA, so an address and a certificate are columns in
# console.orgs rather than process configuration. Nothing is reachable until it
# is registered — deliberately, because a fallback endpoint is exactly the
# silent cross-organisation read the design exists to prevent.

.PHONY: dev-data-key
dev-data-key: ## Create (once) the local keyset that seals organisation credentials
	@if [ ! -f "$(DEV_DATA_KEY_FILE)" ]; then \
	  mkdir -p "$$(dirname $(DEV_DATA_KEY_FILE))"; \
	  $(GO) run ./cmd/cloud data-key 2>/dev/null > "$(DEV_DATA_KEY_FILE)"; \
	  chmod 600 "$(DEV_DATA_KEY_FILE)"; \
	  echo "==> wrote a new keyset to $(DEV_DATA_KEY_FILE)"; \
	fi
	@# Written to a file rather than minted per run, and the difference is not
	@# cosmetic. Every organisation's private key is sealed under this value, so
	@# a fresh one each time would leave every previously registered
	@# organisation unopenable — a row that is complete, valid, and refuses to
	@# decrypt. Regenerating it locally means re-running dev-org-register.
	@echo "$$(cat $(DEV_DATA_KEY_FILE))"

.PHONY: dev-cloud-seed
dev-cloud-seed: build-cloud ## Create an account, an org, and a grant: make dev-cloud-seed EMAIL=you@example.com ORG=acme
	@test -n "$(EMAIL)" -a -n "$(ORG)" || { \
	  echo "Usage:   make dev-cloud-seed EMAIL=<address> ORG=<name>"; \
	  echo "Example: make dev-cloud-seed EMAIL=you@example.com ORG=acme"; \
	  exit 1; \
	}
	@# Three commands rather than one, because they are three different things
	@# and an operator will need them separately. This target exists because
	@# running all three by hand is the common case during development.
	@#
	@# Membership is not yet the gate: `make dev-token` mints an assertion for
	@# whatever ORG it is handed, with no check against these rows. Cloud's
	@# /authorize is what makes them load-bearing, and it does not exist yet.
	@# No `-` prefix on any of these. All three are idempotent, so re-running
	@# this target is not an error — and a `-` would swallow the failures that
	@# are, which is how a seed that stopped working looks exactly like one that
	@# worked.
	CLOUD_PG_URL="$(CLOUD_PG_URL)" $(BIN_DIR)/atlantis-cloud org create -org "$(ORG)"
	CLOUD_PG_URL="$(CLOUD_PG_URL)" $(BIN_DIR)/atlantis-cloud user create -email "$(EMAIL)"
	CLOUD_PG_URL="$(CLOUD_PG_URL)" $(BIN_DIR)/atlantis-cloud member add \
		-email "$(EMAIL)" -org "$(ORG)" -role admin

.PHONY: dev-org-register
dev-org-register: dev-certs dev-data-key build-cloud ## Point an org at the local atlantis: make dev-org-register ORG=<name>
	@test -n "$(ORG)" || { \
	  echo "Usage:   make dev-org-register ORG=<name>"; \
	  echo "Example: make dev-org-register ORG=acme"; \
	  echo; \
	  echo "The name must match the org claim you mint tokens with:"; \
	  echo "  make dev-token ORG=acme"; \
	  exit 1; \
	}
	@# One CA locally, and that is a development compromise worth naming.
	@#
	@# In a deployment each organisation's atlantis has its own trust root, so
	@# credentials issued for one do not chain at another and a mixed-up lookup
	@# is refused inside the TLS handshake. Locally there is one server, so
	@# every organisation registers against the same CA and the same console
	@# certificate — which means the local stack does NOT exercise that
	@# boundary. The two-CA test does (internal/console/org_client_pg_test.go);
	@# do not read a working `make dev` as evidence the separation holds.
	CONSOLE_PG_URL="$(CONSOLE_PG_URL)" \
		CLOUD_PG_URL="$(CLOUD_PG_URL)" \
		CONSOLE_DATA_KEY="$$(cat $(DEV_DATA_KEY_FILE))" \
		$(BIN_DIR)/atlantis-cloud org register \
			-org "$(ORG)" \
			-endpoint "localhost:9090" \
			-health "localhost:8081" \
			-ca "$(DEV_CERT_DIR)/ca.crt" \
			-cert "$(DEV_CERT_DIR)/console.crt" \
			-key "$(DEV_CERT_DIR)/console.key"

# ── Atlantis Cloud (identity) ──────────────────────────────────────────────
#
# The console has no local accounts and no development bypass: it verifies
# every sign-in against a JWKS URL in every environment. So running it locally
# means running the issuer locally, which is the same code Cloud runs.

.PHONY: dev-auth
dev-auth: build-cloud ## Serve Cloud: the key set the console verifies against, plus the account routes
	@# No CLOUD_SMTP_ADDR here, so verification and reset links are written to
	@# this terminal instead of emailed. `cloud serve` warns about it at startup
	@# and at every send — that is the intended development flow, and the
	@# warning is what stops it being the accidental production one.
	CLOUD_ISSUER="$(CLOUD_ISSUER)" \
		CLOUD_PG_URL="$(CLOUD_PG_URL)" \
		CLOUD_PUBLIC_URL="$(CLOUD_PUBLIC_URL)" \
		$(BIN_DIR)/atlantis-cloud serve \
			-key "$(CLOUD_SIGNING_KEY)" \
			-listen "$(CLOUD_LISTEN)"

.PHONY: dev-token
dev-token: build-cloud ## Mint a sign-in assertion. EMAIL=you@example.com ROLE=admin ORG=acme
	@# Prints the URL to open, because the console reads the assertion from the
	@# URL fragment — a fragment is never sent to a server, so it stays out of
	@# access logs and out of the Referer header.
	@#
	@# Same key as `dev-auth`, or the assertion would be signed by a key the
	@# console's key set does not list.
	@token=$$(CLOUD_ISSUER="$(CLOUD_ISSUER)" $(BIN_DIR)/atlantis-cloud mint \
		-key "$(CLOUD_SIGNING_KEY)" \
		-audience "$(CLOUD_AUDIENCE)" \
		-subject "usr_$${EMAIL:-dev@example.com}" \
		-org "$${ORG:-acme}" \
		-role "$${ROLE:-admin}" \
		-email "$${EMAIL:-dev@example.com}" \
		-name "$${NAME:-Local Developer}") && \
	echo "" && \
	echo "Open this to sign in (the assertion is single-use):" && \
	echo "  http://localhost:3000/login#assertion=$$token" && \
	echo ""

.PHONY: build-cloud
build-cloud: ## Build the Cloud identity service
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/atlantis-cloud ./cmd/cloud

.PHONY: dev-console-role
dev-console-role: ## Create the local console database role (NOSUPERUSER NOBYPASSRLS)
	@# Idempotent: creates the role only when absent, and re-grants either way.
	@#
	@# The console refuses to start on a role that can bypass row-level
	@# security, because its per-organisation boundary is an RLS policy and a
	@# superuser reads straight through one. The dev `atlantis` role is a
	@# superuser, so the console needs its own.
	@#
	@# CREATE on the database because the console runs its own migrations: it
	@# creates the console schema and therefore owns it, which is what FORCE ROW
	@# LEVEL SECURITY binds against.
	@psql "$(PG_URL)" -v ON_ERROR_STOP=1 -q -c "\
	  DO \$$\$$ BEGIN \
	    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '$(CONSOLE_PG_ROLE)') THEN \
	      CREATE ROLE $(CONSOLE_PG_ROLE) LOGIN PASSWORD '$(CONSOLE_PG_PASSWORD)' \
	        NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE; \
	    END IF; \
	  END \$$\$$;" \
	  -c "GRANT CONNECT, CREATE ON DATABASE atlantis TO $(CONSOLE_PG_ROLE)" \
	  -c "GRANT USAGE, CREATE ON SCHEMA public TO $(CONSOLE_PG_ROLE)"
	@# Hand over anything a previous superuser-run console created.
	@#
	@# Ownership, not just grants: FORCE ROW LEVEL SECURITY binds the table's
	@# OWNER, so a console that merely has INSERT/SELECT on tables owned by
	@# someone else would still not be subject to its own policies once step 3
	@# adds them. On a fresh database this loop finds nothing — the console
	@# creates and therefore owns everything itself.
	@#
	@# Tables only, no sequences: Postgres refuses ALTER SEQUENCE OWNER on a
	@# sequence owned by a serial column, and does not need it — reowning the
	@# table carries its dependent sequences along.
	@psql "$(PG_URL)" -v ON_ERROR_STOP=1 -q -c "\
	  DO \$$\$$ DECLARE r record; BEGIN \
	    IF EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'console') THEN \
	      EXECUTE 'ALTER SCHEMA console OWNER TO $(CONSOLE_PG_ROLE)'; \
	      FOR r IN SELECT c.relname FROM pg_class c \
	                 JOIN pg_namespace n ON n.oid = c.relnamespace \
	                WHERE n.nspname = 'console' AND c.relkind IN ('r','p') LOOP \
	        EXECUTE format('ALTER TABLE console.%I OWNER TO $(CONSOLE_PG_ROLE)', r.relname); \
	      END LOOP; \
	    END IF; \
	  END \$$\$$;"
	@# The migration history lives in public and is written by whoever migrates.
	@psql "$(PG_URL)" -v ON_ERROR_STOP=1 -q -c "\
	  DO \$$\$$ BEGIN \
	    IF to_regclass('public.console_schema_migrations') IS NOT NULL THEN \
	      EXECUTE 'ALTER TABLE public.console_schema_migrations OWNER TO $(CONSOLE_PG_ROLE)'; \
	    END IF; \
	  END \$$\$$;"
	@echo "==> role $(CONSOLE_PG_ROLE) ready (NOSUPERUSER NOBYPASSRLS), owns schema console"

.PHONY: dev-caller-cert
dev-caller-cert: dev-certs ## Issue a local caller cert signed by the dev CA: make dev-caller-cert CALLER=<name>
	@test -n "$(CALLER)" || { \
	  echo "Usage:   make dev-caller-cert CALLER=<name>"; \
	  echo "Example: make dev-caller-cert CALLER=backend"; \
	  exit 1; \
	}
	@# Signed directly by the dev CA. The signer service issues these in a
	@# deployed stack; locally there is no stack to ask.
	@mkdir -p "$(DEV_CERT_DIR)/callers/$(CALLER)"
	openssl ecparam -genkey -name prime256v1 -noout \
	  -out "$(DEV_CERT_DIR)/callers/$(CALLER)/client.key"
	openssl req -new -subj '/CN=$(CALLER)' \
	  -key "$(DEV_CERT_DIR)/callers/$(CALLER)/client.key" \
	  -out "$(DEV_CERT_DIR)/callers/$(CALLER)/client.csr"
	openssl x509 -req -days 3650 \
	  -in "$(DEV_CERT_DIR)/callers/$(CALLER)/client.csr" \
	  -CA "$(DEV_CERT_DIR)/ca.crt" \
	  -CAkey "$(DEV_CERT_DIR)/ca-private/ca.key" \
	  -CAcreateserial \
	  -out "$(DEV_CERT_DIR)/callers/$(CALLER)/client.crt"
	@rm -f "$(DEV_CERT_DIR)/callers/$(CALLER)/client.csr"
	@chmod 600 "$(DEV_CERT_DIR)/callers/$(CALLER)/client.key"
	@echo
	@echo "Issued $(DEV_CERT_DIR)/callers/$(CALLER)/client.crt (CN=$(CALLER))"
	@echo "  export TIDE_TLS_CERT=$(DEV_CERT_DIR)/callers/$(CALLER)/client.crt"
	@echo "  export TIDE_TLS_KEY=$(DEV_CERT_DIR)/callers/$(CALLER)/client.key"
	@echo "  export TIDE_TLS_CA=$(DEV_CERT_DIR)/ca.crt"

.PHONY: dev-isolated
dev-isolated: ## Full local stack via docker-compose (server + pg + memcached)
	# --build: rebuild atlantis image so a stale one isn't reused.
	# --profile isolated: opt into the atlantis service (otherwise infra-only).
	docker compose --profile isolated up --build

.PHONY: dev-down
dev-down: ## Tear down the docker-compose stack
	docker compose --profile isolated down -v

# dev-tree: symlink real infra migrations + create empty tidectl dir for dev-build's staged plans.
.PHONY: dev-tree
dev-tree:
	@mkdir -p .dev/migrations/tidectl
	@test -L .dev/migrations/infra || ln -sfn ../../migrations/infra .dev/migrations/infra

.PHONY: dev-watch
dev-watch: dev-tree ## Hot-reload server on .atl / .go edits (installs air if missing)
	@which air >/dev/null 2>&1 || (echo "==> installing air (one-time)..." && $(GO) install github.com/air-verse/air@latest)
	@echo "==> watching testdata/schema/, cmd/, internal/ — Ctrl-C to stop"
	@AUTO_MIGRATE=true \
		ATL_MIRROR_SCHEMA=true \
		ATL_ALLOW_APPLY_MUTATION=true \
		PG_URL="$(PG_URL)" \
		MEMCACHED_ADDR="$${MEMCACHED_ADDR:-localhost:11211}" \
		LOG_LEVEL=debug \
		MIGRATIONS_DIR=./.dev/migrations \
		air

# dev-build cycle: plan (stage diff if any) → approve (only if non-empty) → codegen → build.
# plan runs before codegen so the .atl-vs-checkpoint diff is visible.
.PHONY: dev-build
dev-build: ## One dev cycle (called by air): plan → approve (if meaningful) → codegen → build
	@printf "\033[36m==> plan\033[0m\n"
	-@$(GO) run ./cmd/tidectl plan -schema-dir=testdata/schema -migrations-dir=.dev/migrations/tidectl -ir-checkpoint=gen/.last-ir.json -stage-dir=.dev/migrations/tidectl/_staged
	@if ls .dev/migrations/tidectl/_staged/*.up.sql >/dev/null 2>&1; then \
		if grep -q "(no schema changes)" .dev/migrations/tidectl/_staged/*.up.sql; then \
			printf "\033[90m==> approve skipped (no schema diff)\033[0m\n"; \
			rm -f .dev/migrations/tidectl/_staged/*.sql; \
		else \
			printf "\033[36m==> approve\033[0m\n"; \
			$(GO) run ./cmd/tidectl promote -stage-dir=.dev/migrations/tidectl/_staged -migrations-dir=.dev/migrations/tidectl; \
		fi; \
	fi
	@printf "\033[36m==> codegen\033[0m\n"
	@$(GO) run ./cmd/tidectl codegen
	@printf "\033[36m==> buf generate\033[0m\n"
	@buf generate >/dev/null
	@printf "\033[36m==> build\033[0m\n"
	@$(GO) build -o $(BIN_DIR)/atlantis ./cmd/server
	@printf "\033[32m==> ready\033[0m\n"

# dev-reset-db drops the local schema and re-applies all committed
# dev-reset-db: psql DROP SCHEMA atlantis CASCADE + DROP TABLE on both
# golang-migrate version tables in public, then re-runs every committed
# migration from 0000. Wipes every caller's data. Targeted at the local
# dev DB; running against a shared DB destroys everyone's iteration state.
.PHONY: dev-reset-db
dev-reset-db: ## Drop local schema + both migration history tables + reapply all migrations (DESTRUCTIVE)
	@echo "==> dropping schema 'atlantis' on $(PG_URL)..."
	@psql "$(PG_URL)" -c "DROP SCHEMA IF EXISTS atlantis CASCADE;" >/dev/null
	@psql "$(PG_URL)" -c "DROP TABLE IF EXISTS atlantis_schema_migrations_infra, atlantis_schema_migrations_tidectl CASCADE;" >/dev/null
	@echo "==> re-applying migrations..."
	@$(MAKE) migrate-up

# ---------- images ----------
#
# atlantis is a managed cloud product. It is not shipped as a bundle anyone
# else runs, so there are no self-host targets here — the compose file, the
# systemd unit, the reverse-proxy configs and their env template are gone.
#
# What remains is image building, which the platform needs to deploy a stack.
# For local certificates use `make dev-certs` and `make dev-caller-cert`; the
# signer service issues them in a deployed stack.

.PHONY: image
image: ## Build the production image, version-stamped from git
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

# `deploy`, `systemd-install` and `logs` are gone with the systemd unit they
# managed. A stack is deployed by the platform, not by `sudo systemctl restart`
# on the machine that happens to hold this checkout.

# ---------- lint / quality ----------

.PHONY: lint
lint: ## Run static analysis (go vet + golangci-lint if installed)
	$(GO) vet ./...
	@if command -v golangci-lint >/dev/null; then \
		golangci-lint run; \
	else \
		echo "(golangci-lint not installed; skipping) install: brew install golangci-lint"; \
	fi

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

# ---------- CI gates ----------

# codegen-check: re-run codegen and fail if gen/, clients/go/client/,
# atlantis/consumer/, or atlantis/vendorpkg/ diverges from the checked-in
# tree. Run `make codegen` and commit the diff to recover.
#
# WHAT THIS DOES AND DOES NOT COVER
#
# Only useful in a tree that HAS .atl files — a caller's repo, or this one
# with --workspace pointed at one. This repo ships none by design, so here
# the command emits nothing and there is nothing to compare.
#
# It used to report "codegen-check ok" in exactly that case: gen/ is
# gitignored and absent on a fresh checkout, the mkdir -p below made both
# sides of every diff empty-but-present, and comparing empty to empty
# succeeded. So as a CI gate on this repo it passed unconditionally, for
# any change to any emitter, and the green tick meant nothing. Meanwhile it
# FAILED for developers whose working tree still held output generated
# against a schema that has since moved to a caller repo — noisy where it
# was wrong, silent where it mattered.
#
# It now says which of those two situations it is in, and emitter drift is
# covered where it can actually be checked: TestEmittersMatchGolden in
# internal/codegen runs every emitter against a committed fixture schema
# and diffs the result against committed golden files. That runs under
# plain `go test`, so it needs no .atl files anywhere and cannot go
# vacuous.
.PHONY: codegen-check
codegen-check: ## Verify gen/ + clients/go/ + atlantis/*.proto match the current .atl files
	@if [ -z "$$(find testdata/schema -name '*.atl' 2>/dev/null)" ]; then \
	  echo "codegen-check: no .atl files in testdata/schema — nothing to compare."; \
	  echo "  This repo ships no schema; emitter drift is covered by"; \
	  echo "  'go test ./internal/codegen -run Golden' against testdata/schema.atl."; \
	else \
	  tmp=$$(mktemp -d) && \
	  $(GO) run ./cmd/tidectl codegen --out "$$tmp" --ir-checkpoint gen/.last-ir.json && \
	  mkdir -p gen "$$tmp/gen" clients/go/client "$$tmp/clients/go/client" \
	           atlantis/consumer "$$tmp/atlantis/consumer" \
	           atlantis/vendorpkg "$$tmp/atlantis/vendorpkg" && \
	  diff -ruN gen "$$tmp/gen" >/dev/null && \
	  diff -ruN clients/go/client "$$tmp/clients/go/client" >/dev/null && \
	  diff -ruN atlantis/consumer "$$tmp/atlantis/consumer" >/dev/null && \
	  diff -ruN atlantis/vendorpkg "$$tmp/atlantis/vendorpkg" >/dev/null && \
	  rm -rf "$$tmp" && echo "codegen-check ok" || \
	  (echo "codegen-check FAILED. Run 'make codegen' and commit the diff."; rm -rf "$$tmp"; exit 1); \
	fi

# CI gate: up/down/up against fresh DB to catch broken .down.sql.
#
# Scoped to the infra history only. The tidectl history lives under
# .dev/migrations/tidectl/ which is gitignored — it's populated per-
# operator by `tidectl plan/approve` against their own .atl files and
# never lands in this repo, so CI can't roundtrip it.
.PHONY: migrate-roundtrip
# The assertion after `down -all` is the point of this target.
#
# Without it the gate passed for months while rolling nothing back: golang-migrate
# read an empty version table, reported "no change", and left every table
# standing. A reversibility check that cannot tell "rolled back cleanly" from
# "did nothing" is a check that reports on its own invocation, not on the
# migrations. See internal/migrate for the search_path cause.
migrate-roundtrip: ## Verify every infra migration is reversible against a fresh DB
	@which migrate >/dev/null || (echo "install golang-migrate: brew install golang-migrate" && exit 1)
	migrate -path $(MIGRATIONS_INFRA_DIR) -database "$(MIGRATE_URL_INFRA)" up
	migrate -path $(MIGRATIONS_INFRA_DIR) -database "$(MIGRATE_URL_INFRA)" down -all
	@left=$$(psql "$(PG_URL)" -tAc "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'atlantis'"); \
	  if [ "$$left" != "0" ]; then \
	    echo "migrate-roundtrip FAILED: $$left table(s) survived 'down -all'."; \
	    echo "  Either a .down.sql does not undo its .up.sql, or golang-migrate rolled"; \
	    echo "  nothing back — check that the version table is where it thinks it is."; \
	    exit 1; \
	  fi
	migrate -path $(MIGRATIONS_INFRA_DIR) -database "$(MIGRATE_URL_INFRA)" up
	@left=$$(psql "$(PG_URL)" -tAc "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'atlantis'"); \
	  if [ "$$left" = "0" ]; then \
	    echo "migrate-roundtrip FAILED: the second 'up' restored no tables."; exit 1; \
	  fi
	@echo "migrate-roundtrip ok"

# ---------- clean ----------

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)
