# PayFlowMock — local development
# Do not export DATABASE_URL here: `cmd/server` calls godotenv.Load(), which does not override
# existing environment variables. Exporting DATABASE_URL in Make would ignore `.env` for `make run`.

MIGRATE        ?= migrate
MIGRATIONS_PATH = migrations
BINARY         = bin/server

.PHONY: help up down migrate-up migrate-down run test build clean install-migrate

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-18s\033[0m %s\n", $$1, $$2}'

up: ## Start PostgreSQL (docker compose)
	docker compose up -d

down: ## Stop PostgreSQL
	docker compose down

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
