# ADR-010: Packaging, local stack and CI

- Status: Accepted
- Date: 2026-11-19

## Context
Deliverables include a one-command setup (`docker compose up`) and an automated test and lint
pipeline. The stack has two scalable binaries, a mock provider, Postgres, Redis and the
observability tools from ADR-007.

## Decision
- **One image, four static binaries** (`api`, `worker`, `mockprovider`, `healthcheck`), built in a
  multi-stage Dockerfile (`CGO_ENABLED=0`, `-trimpath -s -w`, module and build caches as BuildKit
  cache mounts) onto **distroless `static:nonroot`**: no shell, no package manager, uid 65532.
  Compose selects the binary per service, so all services run the same build.
- Distroless has no `curl`/`wget`, so a **20-line `healthcheck` binary** probes `/readyz` for
  compose health checks; `depends_on: condition: service_healthy` orders start-up, and the
  one-shot **`migrate`** service (golang-migrate, versioned files in `migrations/`) must complete
  before the API or workers start.
- **Only user-facing ports are published** (API, Jaeger UI, Prometheus, mock provider stats), each
  overridable by env. Postgres and Redis stay internal so the stack never clashes with local
  databases.
- **Workers scale with `--scale worker=N`**: they publish no host port, and Prometheus discovers
  every replica through DNS service discovery on the compose service name.
- `PROVIDER_URL` switches from the mock (default, 10 % failures so retries are visible) to
  webhook.site without code changes.
- **`scripts/smoke.sh`** drives the public API end to end (rate limit as the provider sees it:
  300 SMS, and the mock's busiest second must stay within 100; templates, idempotent replay,
  validation, a 30-item mixed batch until all are sent, stats, metrics, Prometheus scraping every
  worker). It needs only bash, curl and grep, and runs locally (`make smoke`) and in CI.
- **`test/acceptance`** (`make acceptance`, build tag `acceptance`) checks the running stack
  against the assessment specification, one test per requirement. `requirements.md` maps each
  requirement to its tests. The suite is black-box on purpose: public API, mock provider
  `/stats`, Prometheus, Jaeger and `docker compose logs`, with no database access. So it needs
  no extra published ports, and it tests what a client can observe. The rate limit, for
  example, is measured where the provider receives requests.
- **CI (GitHub Actions)** runs three parallel jobs:
  - lint: go vet incl. the integration build tag, golangci-lint, OpenAPI via Redocly, and the
    Claude Code hook tests;
  - tests: `make test`, unit + integration with testcontainers, `-race`;
  - e2e: compose up, smoke test, the acceptance suite, then scale to three workers and smoke
    again.

## Consequences
- + The stack a reviewer starts is the stack CI tests, including multi-replica workers.
- + Verified locally: with three workers 1000 SMS were split 380/355/375 across replicas and the
  busiest one-second window held 95 sends — the Redis limiter holds across processes.
- − The image is ~100 MB uncompressed because each binary carries its own copy of shared
  dependencies; separate images per binary would be smaller but slower to build in CI.
- − Credentials in compose are local-development defaults; a real deployment injects them.
