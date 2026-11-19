# CLAUDE.md

Guidance for Claude Code when working in this repository.

## Project
Event-driven notification system (SMS / Email / Push) for the Insider One assessment.
Two binaries share one codebase:
- `cmd/api` – HTTP API (chi), WebSocket fan-out, `/metrics` (incl. queue depth), `/v1/stats`
- `cmd/worker` – per-channel delivery workers (claim → rate-limit → send → retry); `/metrics`, `/healthz`, `/readyz` on `:9091`
- `cmd/mockprovider` – local stand-in for webhook.site with configurable failure rate/latency

PostgreSQL is both the source of truth **and** the queue (`FOR UPDATE SKIP LOCKED`).
Redis is used for the distributed rate limiter and status-event pub/sub. See `docs/adr/`.

## Architecture rules
- Dependency direction: `httpapi`/`delivery` → `domain` ← `store/*`. `domain` imports nothing from the project.
- Business rules (validation, status transitions, retry policy) live in `internal/domain` or `internal/delivery`, never in handlers or SQL.
- Every status transition is a **conditional UPDATE** (`WHERE status = <expected>`). Never read-then-write.
- Interfaces are defined by the consumer package, kept small, and implemented in `store/*`, `ratelimit`, `provider`.
- No global state. Wire dependencies explicitly in `cmd/*/main.go` — this includes Prometheus registries and the OTel tracer provider (never the library globals).
- Metric labels come from closed sets (channel, priority, outcome, route *pattern*); never put IDs or raw paths in labels.
- Config only via environment variables (`internal/config`), with sane defaults for local dev.

## Go conventions
- Go 1.27, stdlib first. Approved deps: chi, pgx/v5, go-redis/v9, golang-migrate, prometheus client, otel, gobreaker, coder/websocket, google/uuid, testcontainers-go. Ask before adding others.
- `context.Context` is the first parameter of anything doing I/O.
- Wrap errors with `fmt.Errorf("doing x: %w", err)`; sentinel errors in `domain` (`ErrNotFound`, `ErrConflict`, ...) mapped to HTTP codes in one place.
- Logging: `log/slog` JSON, always include `correlation_id` (and `notification_id` where relevant). No `fmt.Println`.
- Schema changes only through new files in `migrations/` (never edit an applied migration).
- `api/openapi.yaml` is the contract: update it in the same commit as any API change and run `make lint-api`.

## Testing
- `make test` runs everything (unit + integration with testcontainers; Docker required). `make test-unit` skips integration.
- Integration tests use the build tag `integration`.
- Table-driven tests; test behaviour, not implementation. New logic ships with tests in the same commit.
- Run `go test -race` before declaring a phase done.

## Workflow
- Work in phases from the approved plan; one logical change per commit, Conventional Commits (`feat:`, `fix:`, `test:`, `docs:`, `chore:`).
- Record non-obvious decisions as ADRs in `docs/adr/` (short: context, decision, consequences).
- Before committing: `make lint test-unit` must pass.

## Commands
- `make run` – `docker compose up --build`
- `make test` / `make test-unit` / `make lint` / `make lint-api` (OpenAPI, via Docker)
- `make migrate-up` / `make migrate-down`
