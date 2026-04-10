# PayFlowMock

A mock payment middleware service: a small HTTP API backed by PostgreSQL for creating payments, fetching them, and issuing refunds. Use it to exercise integrations without calling a real payment service provider.

## Requirements

- **Go** 1.25+ (see `go.mod`)
- **Docker** (optional, for local PostgreSQL via Compose)

## Quick start

1. **Configure secrets**: copy `.env.example` to `.env` and set `POSTGRES_*`, `DATABASE_URL`, and optionally `API_KEY`.

2. **Start PostgreSQL** (Compose reads `POSTGRES_*` from `.env`):

   ```bash
   make up
   ```

3. **Run the server** (loads `.env` via [godotenv](https://github.com/joho/godotenv); migrations run on startup):

   ```bash
   make run
   ```

   The API listens on **port 8080** by default.

## Configuration

| Variable           | Description                                      | Default                                      |
| ------------------ | ------------------------------------------------ | -------------------------------------------- |
| `DATABASE_URL`     | PostgreSQL connection string (**required**)      | _(none — use `.env` or export)_             |
| `API_KEY`          | If set, `/v1/*` requires `X-API-Key` or `Authorization: Bearer` | _(empty — no API key check)_     |
| `PORT`             | HTTP listen port                                 | `8080`                                       |
| `MIGRATIONS_PATH`  | Directory containing SQL migration files         | `./migrations` relative to process cwd       |

`POSTGRES_USER`, `POSTGRES_PASSWORD`, and `POSTGRES_DB` in `.env` are used by Docker Compose for the `postgres` service; `DATABASE_URL` must match those credentials when connecting from the host.

Example (or use a `.env` file next to the binary):

```bash
export DATABASE_URL='postgres://payflow:payflow_dev_password@127.0.0.1:5432/DBName?sslmode=disable'
export PORT=8080
go run ./cmd/server
```

## Makefile targets

Run `make help` for a full list. Common commands:

- `make up` / `make down` — start or stop Postgres via Docker Compose
- `make run` — run the API with the default `DATABASE_URL`
- `make test` — run tests
- `make build` — build `bin/server`
- `make migrate-up` / `make migrate-down` — apply or roll back migrations (requires the [golang-migrate](https://github.com/golang-migrate/migrate) CLI)

## HTTP API

Base path: `/v1`.

| Method | Path                         | Description        |
| ------ | ---------------------------- | ------------------ |
| `POST` | `/v1/payments`               | Create a payment   |
| `GET`  | `/v1/payments/{id}`          | Get payment by ID  |
| `POST` | `/v1/payments/{id}/refund`   | Refund a payment   |
| `GET`  | `/healthz`                   | Liveness; checks DB connectivity |

Errors are JSON: `{ "error": { "code": "...", "message": "..." } }`.

## Project layout

- `cmd/server` — HTTP server entrypoint
- `internal/` — handlers, services, repositories, domain types
- `migrations/` — SQL migrations (applied on startup and via `make migrate-*`)
- `pkg/` — shared packages (e.g. logging)
