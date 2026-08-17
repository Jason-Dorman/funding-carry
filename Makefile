# Basis Carry System — developer entry points.
#
# Everything runs from the repository root. --project-directory keeps build
# contexts and .env paths resolving here rather than in deploy/.

GO      ?= go
COMPOSE ?= docker compose --project-directory . -f deploy/docker-compose.yml

# Integration tests connect over TimescaleDB's published port rather than the
# Compose network, so they can run from the host. 15432, not 5432, because a
# developer machine usually already has a Postgres on the default port. The
# credentials match the synthetic ones in .env.example; override to point
# somewhere else. The suite never writes to this database — it creates a
# throwaway one beside it.
TEST_DATABASE_URL ?= postgres://carry:carry@localhost:15432/carry?sslmode=disable

.DEFAULT_GOAL := help
.PHONY: help env up down ps logs build test test-integration lint fmt tidy migrate replay clean

help: ## List the available targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

env: .env ## Create .env from the synthetic template if it does not exist

.env:
	@cp .env.example .env
	@echo "created .env from .env.example (synthetic values; real ones go in .env.private)"

up: env ## Build and start the full stack, waiting until every service is healthy
	$(COMPOSE) up -d --build --wait

down: ## Stop the stack, keeping data volumes
	$(COMPOSE) down

ps: ## Show service status and health
	$(COMPOSE) ps

logs: ## Follow logs from all services
	$(COMPOSE) logs -f

build: ## Compile all binaries
	$(GO) build ./...

# -count=1 disables the test cache. internal/guard type-checks the whole module,
# and the cache cannot see that its result depends on every other package:
# without it the guard keeps reporting a stale pass after a violation is
# introduced elsewhere.
test: ## Run the Go test suite with the race detector
	$(GO) test -race -count=1 ./...

# The schema is a contract, so it is tested against a real TimescaleDB rather
# than a fake: real hypertables, real migrations, real numeric round trips.
test-integration: ## Run the integration suite against the Compose TimescaleDB
	DATABASE_URL="$(TEST_DATABASE_URL)" $(GO) test -race -count=1 -tags integration ./...

lint: ## Run golangci-lint
	golangci-lint run

fmt: ## Format the Go sources
	$(GO) fmt ./...

tidy: ## Reconcile go.mod and go.sum
	$(GO) mod tidy

# Run inside Compose rather than on the host: DATABASE_URL in .env names the
# Compose service, and the migrations are embedded in the binary, so this needs
# no Go toolchain and no migration CLI installed.
migrate: env ## Apply database migrations and seed the perp product row
	$(COMPOSE) run --rm --build migrate

replay: ## Run the 30-day backtest and refresh Grafana
	@echo "replay: not implemented until Part 20 (backtester / replay)" >&2
	@exit 1

clean: ## Stop the stack and delete its data volumes
	$(COMPOSE) down -v
