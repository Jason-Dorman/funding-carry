# Basis Carry System — developer entry points.
#
# Everything runs from the repository root. --project-directory keeps build
# contexts and .env paths resolving here rather than in deploy/.

GO      ?= go
COMPOSE ?= docker compose --project-directory . -f deploy/docker-compose.yml

.DEFAULT_GOAL := help
.PHONY: help env up down ps logs build test lint fmt tidy migrate replay clean

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

lint: ## Run golangci-lint
	golangci-lint run

fmt: ## Format the Go sources
	$(GO) fmt ./...

tidy: ## Reconcile go.mod and go.sum
	$(GO) mod tidy

migrate: ## Apply database migrations
	@echo "migrate: not implemented until Part 2 (database schema, migrations, writer package)" >&2
	@exit 1

replay: ## Run the 30-day backtest and refresh Grafana
	@echo "replay: not implemented until Part 20 (backtester / replay)" >&2
	@exit 1

clean: ## Stop the stack and delete its data volumes
	$(COMPOSE) down -v
