# PayFlowMock — local development
# Do not export DATABASE_URL here: `cmd/server` calls godotenv.Load(), which does not override
# existing environment variables. Exporting DATABASE_URL in Make would ignore `.env` for `make run`.

MIGRATE        ?= migrate
MIGRATIONS_PATH = migrations
BINARY         = bin/server

WEBHOOK_SINK_PORT ?= 9999

.PHONY: help up down postgres-up postgres-down redis-up redis-down migrate-up migrate-down run test build clean install-migrate webhook-sink

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-18s\033[0m %s\n", $$1, $$2}'

up: ## Start PostgreSQL and Redis (docker compose)
	docker compose up -d postgres redis

down: ## Stop and remove PostgreSQL and Redis containers (docker compose)
	docker compose down

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
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$$DATABASE_URL" up

migrate-down: ## Roll back the last migration
	@export DATABASE_URL="$(DATABASE_URL)"; \
	set -a && [ -f .env ] && . ./.env; set +a && \
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$$DATABASE_URL" down 1

run: ## Run the API server
	go run ./cmd/server

test: ## Run all tests
	go test -race -count=1 ./...

build: ## Build server binary to bin/server
	@mkdir -p $(dir $(BINARY))
	go build -o $(BINARY) ./cmd/server

clean: ## Remove build artifacts
	rm -rf bin/

install-migrate: ## Install golang-migrate CLI with postgres support
	go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest

webhook-sink: ## Local echo server for webhook manual testing (port via WEBHOOK_SINK_PORT, default 9999)
	WEBHOOK_SINK_PORT=$(WEBHOOK_SINK_PORT) python3 scripts/webhook_sink.py
