# atlantis Makefile

SHELL := /bin/bash

# Load .env if present; export so recipes inherit.
-include .env
export

PG_URL ?= postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable

# Directory for operator-issued caller certs (gitignored via .dev/).
# Callers get their cert here; they point tide.yaml at it to reach the
# self-host stack. Operator runs: make self-host-caller-cert CALLER=<name>
CALLER_CERT_DIR ?= .dev/caller-certs

# Host port the signer service exposes (127.0.0.1 only).
# Matches SIGNER_PORT in docker-compose.self-host.yml and .env.
SIGNER_PORT ?= 7071

# Two migration histories: infra (hand-written) and tidectl (codegen).
MIGRATIONS_INFRA_DIR := ./migrations/infra
MIGRATIONS_TIDECTL_DIR := ./.dev/migrations/tidectl
MIGRATE_URL_INFRA := $(PG_URL)&x-migrations-table=atlantis_schema_migrations_infra
MIGRATE_URL_TIDECTL := $(PG_URL)&x-migrations-table=atlantis_schema_migrations_tidectl

GO ?= go
GOFLAGS ?=

BIN_DIR := ./bin

# Docker image tag and VERSION string for `make image` / `make deploy`.
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

# Output directory for release artifacts. Gitignored via .dev/.
RELEASE_DIR := dist

# CLIs cross-compiled per platform. Each entry produces a tarball named
# `<cli>-<version>-<os>-<arch>.tar.gz` under dist/. Add a row to publish
# a new CLI.
RELEASE_CLIS := tide tidectl

# Resolved native host platform (used by release-clis-native to label
# the output tarballs). Override with GOOS / GOARCH when invoking.
NATIVE_OS   := $(shell $(GO) env GOOS)
NATIVE_ARCH := $(shell $(GO) env GOARCH)

# `make release-clis-native VERSION=v0.4.0` builds every CLI in
# RELEASE_CLIS for the HOST OS+arch with CGO enabled, lays out the
# tarballs under dist/, and emits a SHA256SUMS scoped to that
# platform. tide and tidectl both transitively depend on pg_query_go
# (libpg_query via cgo), so cross-compiling all platforms from one
# runner needs a CGO cross-toolchain pile that isn't worth maintaining
# in a Makefile — the workflow (.github/workflows/release-clis.yml)
# fans out across native runners (ubuntu amd64/arm64 + macos
# amd64/arm64) and aggregates the artifacts in a final job. Operators
# install with `curl -L … | tar xz` — no go toolchain needed.
.PHONY: release-clis-native
release-clis-native: ## Cross-compile tide + tidectl tarballs for the native host platform: make release-clis-native VERSION=v0.4.0
	@case "$(VERSION)" in v[0-9]*) : ;; *) \
	  echo "Usage: make release-clis-native VERSION=v0.4.0"; \
	  echo "       (got '$(VERSION)' — must look like v0.4.0)"; \
	  exit 1 ;; esac
	@mkdir -p $(RELEASE_DIR)
	@for cli in $(RELEASE_CLIS); do \
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
	$(GO) run ./cmd/tidectl approve

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

.PHONY: dev
dev: ## Run the server locally against docker-compose stack
	docker compose up -d postgres memcached
	AUTO_MIGRATE=true \
		ATL_MIRROR_SCHEMA=true \
		ATL_ALLOW_APPLY_MUTATION=true \
		$(GO) run ./cmd/server

.PHONY: dev-console
dev-console: build-console ## Run the management console BFF against the local dev server (no TLS)
	CONSOLE_PG_URL="$(PG_URL)" \
		ATL_ENDPOINT="localhost:9090" \
		CONSOLE_SESSION_SECRET="$${CONSOLE_SESSION_SECRET:-dev-secret-change-in-prod-32chars!!}" \
		CONSOLE_LISTEN=":3000" \
		ATL_HEALTH_LISTEN="localhost:8081" \
		CONSOLE_COOKIE_SECURE=false \
		$(BIN_DIR)/atlantis-console

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
			$(GO) run ./cmd/tidectl approve -stage-dir=.dev/migrations/tidectl/_staged -migrations-dir=.dev/migrations/tidectl; \
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

# ---------- deploy ----------
#
# Single-VM production deploy: the atlantis server runs as a systemd-managed
# Docker container (deploy/atlantis.service) against host-native Postgres +
# memcached. The docker run lives in the unit, not here, so the service boots
# independently of this checkout. See docs/guides/deploy-to-production.md.

.PHONY: self-host-up
self-host-up: image build-console-image build-signer-image ## Start the self-host bundle (builds 3 images; on first run copies deploy/.env.example to .env — edit PG_PASSWORD and CONSOLE_SESSION_SECRET before exposing 5432/3000 to any untrusted network)
	@test -f .env || (echo "==> copying deploy/.env.example to .env — fill PG_PASSWORD and CONSOLE_SESSION_SECRET" && cp deploy/.env.example .env)
	docker compose -f docker-compose.self-host.yml up -d

.PHONY: self-host-down
self-host-down: ## Tear down the self-hosting bundle (preserves volumes)
	docker compose -f docker-compose.self-host.yml down

.PHONY: self-host-caller-cert
self-host-caller-cert: ## Issue a caller mTLS cert via the signer service (stack must be running): make self-host-caller-cert CALLER=<name>
	@test -n "$(CALLER)" || { \
	  echo "Usage:   make self-host-caller-cert CALLER=<name>"; \
	  echo "Example: make self-host-caller-cert CALLER=backend"; \
	  exit 1; \
	}
	@which openssl >/dev/null 2>&1 || (echo "openssl not found"; exit 1)
	@which curl    >/dev/null 2>&1 || (echo "curl not found"; exit 1)
	@which python3 >/dev/null 2>&1 || (echo "python3 not found"; exit 1)
	@mkdir -p "$(abspath $(CALLER_CERT_DIR)/$(CALLER))"
	@# Generate the private key and a CSR on the host machine.
	@# The private key never leaves the host; only the CSR is sent to the signer.
	openssl ecparam -genkey -name prime256v1 -noout \
	  -out "$(abspath $(CALLER_CERT_DIR)/$(CALLER))/client.key"
	openssl req -new \
	  -key "$(abspath $(CALLER_CERT_DIR)/$(CALLER))/client.key" \
	  -subj '/CN=$(CALLER)' \
	  -out "$(abspath $(CALLER_CERT_DIR)/$(CALLER))/client.csr"
	@# POST the CSR to the signer service (exposed at 127.0.0.1:SIGNER_PORT).
	@# python3 json.dumps encodes the PEM newlines correctly for the JSON body.
	@CSR_JSON=$$(python3 -c "import json,sys; print(json.dumps(open('$(abspath $(CALLER_CERT_DIR)/$(CALLER))/client.csr').read()))"); \
	 RESP=$$(curl -sf --max-time 15 -X POST "http://localhost:$(SIGNER_PORT)/issue" \
	   -H "Content-Type: application/json" \
	   -d "{\"caller\":\"$(CALLER)\",\"csr_pem\":$$CSR_JSON}" 2>&1) \
	   || { echo "==> signer request failed. Is the stack running?  make self-host-up"; \
	        echo "    Response: $$RESP"; exit 1; }; \
	 echo "$$RESP" | python3 -c "import json,sys; d=json.load(sys.stdin); \
	   open('$(abspath $(CALLER_CERT_DIR)/$(CALLER))/client.crt','w').write(d['cert_pem']); \
	   open('$(abspath $(CALLER_CERT_DIR)/$(CALLER))/ca.crt','w').write(d['ca_pem']); \
	   print('[certs] issued CN=$(CALLER) · expires',d['expires_at'])"
	@rm -f "$(abspath $(CALLER_CERT_DIR)/$(CALLER))/client.csr"
	@chmod 644 "$(abspath $(CALLER_CERT_DIR)/$(CALLER))/client.crt" \
	            "$(abspath $(CALLER_CERT_DIR)/$(CALLER))/ca.crt"
	@chmod 600 "$(abspath $(CALLER_CERT_DIR)/$(CALLER))/client.key"
	@echo ""
	@echo "==> $(CALLER_CERT_DIR)/$(CALLER)/"
	@echo "    client.crt  signed leaf   (CN=$(CALLER))"
	@echo "    client.key  private key   (keep secret)"
	@echo "    ca.crt      trust anchor"
	@echo ""
	@echo "tide.yaml:"
	@echo "  caller:   $(CALLER)"
	@echo "  endpoint: localhost:9090"
	@echo "  tls:"
	@printf "    cert: %s/client.crt\n" "$(abspath $(CALLER_CERT_DIR)/$(CALLER))"
	@printf "    key:  %s/client.key\n" "$(abspath $(CALLER_CERT_DIR)/$(CALLER))"
	@printf "    ca:   %s/ca.crt\n" "$(abspath $(CALLER_CERT_DIR)/$(CALLER))"

.PHONY: self-host-logs
self-host-logs: ## Tail logs from the self-hosting bundle
	docker compose -f docker-compose.self-host.yml logs -f

.PHONY: image
image: ## Build the production image, version-stamped from git
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

.PHONY: deploy
deploy: image ## Rebuild the image and restart the atlantis service (run git pull first)
	sudo systemctl restart atlantis
	@echo "==> deployed $(IMAGE) ($(VERSION))"

.PHONY: systemd-install
systemd-install: ## Install/refresh the systemd unit, then enable + start
	sudo install -m 0644 deploy/atlantis.service /etc/systemd/system/atlantis.service
	sudo systemctl daemon-reload
	sudo systemctl enable --now atlantis

.PHONY: logs
logs: ## Tail the atlantis service logs (journald)
	journalctl -u atlantis -f

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
# tree. CI gate; run `make codegen` and commit the diff to recover.
#
# `mkdir -p` on both sides of each diff handles the gen-less-repo case:
# on a fresh clone with no .atl fixtures the output dirs don't exist at
# all, and `diff -ruN` (which treats absent FILES as empty) still errors
# on a missing root directory. The mkdirs make both sides empty-but-
# present so an absent .atl fixture diffs cleanly.
.PHONY: codegen-check
codegen-check: ## Verify gen/ + clients/go/ + atlantis/*.proto are up to date with current .atl files
	@tmp=$$(mktemp -d) && \
	  $(GO) run ./cmd/tidectl codegen --out "$$tmp" --ir-checkpoint gen/.last-ir.json && \
	  mkdir -p gen "$$tmp/gen" clients/go/client "$$tmp/clients/go/client" \
	           atlantis/consumer "$$tmp/atlantis/consumer" \
	           atlantis/vendorpkg "$$tmp/atlantis/vendorpkg" && \
	  diff -ruN gen "$$tmp/gen" >/dev/null && \
	  diff -ruN clients/go/client "$$tmp/clients/go/client" >/dev/null && \
	  diff -ruN atlantis/consumer "$$tmp/atlantis/consumer" >/dev/null && \
	  diff -ruN atlantis/vendorpkg "$$tmp/atlantis/vendorpkg" >/dev/null && \
	  rm -rf "$$tmp" && echo "codegen-check ok" || \
	  (echo "codegen-check FAILED. Run 'make codegen' and commit the diff."; rm -rf "$$tmp"; exit 1)

# CI gate: up/down/up against fresh DB to catch broken .down.sql.
#
# Scoped to the infra history only. The tidectl history lives under
# .dev/migrations/tidectl/ which is gitignored — it's populated per-
# operator by `tidectl plan/approve` against their own .atl files and
# never lands in this repo, so CI can't roundtrip it.
.PHONY: migrate-roundtrip
migrate-roundtrip: ## Verify every infra migration is reversible against a fresh DB
	@which migrate >/dev/null || (echo "install golang-migrate: brew install golang-migrate" && exit 1)
	migrate -path $(MIGRATIONS_INFRA_DIR) -database "$(MIGRATE_URL_INFRA)" up
	migrate -path $(MIGRATIONS_INFRA_DIR) -database "$(MIGRATE_URL_INFRA)" down -all
	migrate -path $(MIGRATIONS_INFRA_DIR) -database "$(MIGRATE_URL_INFRA)" up
	@echo "migrate-roundtrip ok"

# ---------- clean ----------

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)
