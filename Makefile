# ozymandias — developer entry points. `make help` lists them.
#
# CI calls these same targets (.github/workflows/ci.yml), so "green locally"
# and "green in CI" mean the same thing.

SHELL := /bin/bash
.DEFAULT_GOAL := help

MODULE   := $(shell go list -m 2>/dev/null)
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X $(MODULE)/internal/buildinfo.Version=$(VERSION)
COMPOSE  := OZY_VERSION=$(VERSION) OZY_HOSTNAME=$(shell hostname -s) docker compose -f deploy/docker-compose.yml
FUZZTIME ?= 30s
CI_FUZZTIME ?= 10s

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{ printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2 }'

# --- build -------------------------------------------------------------------
.PHONY: build
build: ## Build ozyd + agent into ./bin (embeds whatever UI is in internal/api/ui/dist)
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/ ./cmd/...

.PHONY: web
web: ## Build the web UI into the Go embed dir
	cd web && npm ci && npm run build

# --- quality gates -----------------------------------------------------------
.PHONY: test
test: ## go test -race + coverage gates
	scripts/check-coverage.sh

.PHONY: test-short
test-short: ## go test without race/coverage (fast inner loop)
	go test ./...

.PHONY: lint
lint: ## golangci-lint + no-app-coupling check
	@want=$$(cat .golangci-lint-version); got=v$$(golangci-lint version --short 2>/dev/null); \
		if [ "$$got" != "$$want" ]; then \
			echo "warning: linting with golangci-lint $$got; CI uses $$want (.golangci-lint-version)"; \
		fi
	golangci-lint run ./...
	scripts/check-no-app-coupling.sh

.PHONY: docs-check
docs-check: ## Documentation drift checks
	scripts/check-docs.sh

.PHONY: fuzz
fuzz: ## Every fuzz target for FUZZTIME (default 30s)
	FUZZTIME=$(FUZZTIME) scripts/run-fuzz.sh

.PHONY: fuzz-long
fuzz-long: ## Every fuzz target for 10 minutes
	FUZZTIME=10m scripts/run-fuzz.sh

.PHONY: soak
soak: ## The storage tests at full scale (OZY_SOAK=1) — nightly, not the PR gate
	OZY_SOAK=1 go test -race -timeout 40m -run 'TestDB_' ./internal/tsdb/db/

.PHONY: web-check
web-check: ## Web typecheck + lint + tests with coverage
	cd web && npm run typecheck && npm run lint && npm run test:coverage

.PHONY: sdk-check
sdk-check: ## Both SDKs: install, typecheck, lint, tests with their 90% gates
	@want=$$(awk '$$1=="uv"{print $$2}' sdk/python/.tool-versions); got=$$(uv --version 2>/dev/null | awk '{print $$2}'); \
		if [ "$$got" != "$$want" ]; then \
			echo "warning: using uv $$got; CI uses $$want (sdk/python/.tool-versions)"; \
		fi
	cd sdk/node && npm ci --silent && npm run typecheck && npm run lint && npm run test:coverage
	cd sdk/python && uv sync --locked -q && uv run ruff check && uv run ruff format --check && uv run mypy && uv run pytest -q

.PHONY: ci
ci: lint test docs-check web-check sdk-check ## The full local gate (runs on git push via lefthook)
	$(MAKE) fuzz FUZZTIME=$(CI_FUZZTIME)

# --- run ---------------------------------------------------------------------
.PHONY: up
up: ## Build the image and start the compose stack (waits until healthy)
	docker network inspect ozymandias >/dev/null 2>&1 || docker network create ozymandias
	$(COMPOSE) up -d --build --wait

.PHONY: down
down: ## Stop the compose stack (data volume kept; `make down-v` wipes it)
	$(COMPOSE) down

.PHONY: down-v
down-v: ## Stop the compose stack and delete its data volume
	$(COMPOSE) down -v

.PHONY: dev
dev: build ## Run ozyd + agent natively and the Vite dev server
	scripts/dev.sh

.PHONY: smoke
smoke: ## End-to-end checks against the running compose stack
	scripts/smoke.sh

.PHONY: sdk-release
sdk-release: ## Build SDK artifacts and copy them into app vendor dirs
	scripts/release-sdk.sh

.PHONY: clean
clean: ## Remove build output
	rm -rf bin coverage.out
