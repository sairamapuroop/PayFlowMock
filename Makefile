# PayFlowMock — local development
# Do not export DATABASE_URL here: `cmd/server` calls godotenv.Load(), which does not override
# existing environment variables. Exporting DATABASE_URL in Make would ignore `.env` for `make run`.

MIGRATE        ?= migrate
MIGRATIONS_PATH = migrations
BINARY         = bin/server

WEBHOOK_SINK_PORT ?= 9999

.PHONY: help up down postgres-up postgres-down redis-up redis-down migrate-up migrate-down run test test-unit test-integration test-week2-unit test-week2-integration test-week3-unit test-week3-integration build clean install-migrate webhook-sink stack-up stack-down logs docker-build

help: ## List targets
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-24s\033[0m %s\n", $$1, $$2}'

up: ## Start PostgreSQL and Redis (docker compose)
	docker compose up -d postgres redis

down: ## Stop and remove PostgreSQL and Redis containers (docker compose)
	docker compose down

stack-up: ## Full stack: Postgres, Redis, app, Jaeger, Prometheus, Grafana (requires `.env`; cp .env.example .env first)
	@test -f .env || (echo >&2 "Missing .env — run: cp .env.example .env"; exit 1)
	docker compose up -d --build

stack-down: ## Stop full stack (all compose services)
	docker compose down

logs: ## Follow logs — optional SERVICES="app grafana" (default: all services)
	docker compose logs -f $(SERVICES)

docker-build: ## Build the app image only
	docker compose build app

postgres-up: ## Start PostgreSQL only
	docker compose up -d postgres

postgres-down: ## Stop PostgreSQL only (container kept)
	docker compose stop postgres

redis-up: ## Start Redis only
	docker compose up -d redis

redis-down: ## Stop Redis only (container kept)
	docker compose stop redis

migrate-up: ## Apply all migrations (requires golang-migrate CLI)
	@export DATABASE_URL="$(DATABASE_URL)"; \
	set -a && [ -f .env ] && . ./.env; set +a && \
	$(MIGRATE) -path $(MIGRATIONS_PATH)/up -database "$$DATABASE_URL" up

migrate-down: ## Roll back the last migration
	@export DATABASE_URL="$(DATABASE_URL)"; \
	set -a && [ -f .env ] && . ./.env; set +a && \
	$(MIGRATE) -path $(MIGRATIONS_PATH)/down -database "$$DATABASE_URL" down 1

run: ## Run the API server
	go run ./cmd/server

test: ## Run all tests
	go test -race -count=1 ./...

test-unit: ## All packages with -short (skips tests that call testutil.MustPool / MustRedis)
	go test -race -count=1 -short ./...

test-week2-unit: ## Week 2 only: retry, PSP (router/mocks/circuit breaker), payment service — no Postgres/Redis (-short)
	go test -race -count=1 -short ./internal/retry/... ./internal/psp/... ./internal/service/...

test-integration: ## Integration: repository + handler + middleware (needs TEST_DATABASE_URL; Redis for idempotency Redis paths; loads .env if present)
	@set -a && [ -f .env ] && . ./.env; set +a && \
	go test -race -count=1 ./internal/repository/... ./internal/handler/... ./internal/middleware/...

test-week2-integration: ## Week 2 only: idempotency middleware against Postgres + Redis (same env as test-integration)
	@set -a && [ -f .env ] && . ./.env; set +a && \
	go test -race -count=1 ./internal/middleware/...

test-week3-unit: ## Week 3 only: merchant webhook registry + domain state machine — no Postgres/Redis (-short)
	go test -race -count=1 -short ./internal/merchant/... ./internal/domain/...

test-week3-integration: ## Week 3 only: webhook outbox worker against Postgres (same env as test-integration; no Redis required)
	@set -a && [ -f .env ] && . ./.env; set +a && \
	go test -race -count=1 ./internal/worker/...

build: ## Build server binary to bin/server
	@mkdir -p $(dir $(BINARY))
	go build -o $(BINARY) ./cmd/server

clean: ## Remove build artifacts
	rm -rf bin/

install-migrate: ## Install golang-migrate CLI with postgres support
	go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest

webhook-sink: ## Local echo server for webhook manual testing (port via WEBHOOK_SINK_PORT, default 9999)
	WEBHOOK_SINK_PORT=$(WEBHOOK_SINK_PORT) python3 scripts/webhook_sink.py
