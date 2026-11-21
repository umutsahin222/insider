.PHONY: run up down logs smoke acceptance build test test-unit lint lint-api lint-alerts test-hooks git-hooks migrate-up migrate-down tidy

# Inside the compose network (the migrate service talks to the postgres service).
COMPOSE_DB_URL ?= postgres://notify:notify@postgres:5432/notify?sslmode=disable
REDOCLY_VERSION ?= 1.25.11
# Same image as the prometheus service in docker-compose.yml.
PROMETHEUS_VERSION ?= v3.16.0

# Foreground, with logs.
run:
	docker compose up --build

# Background; returns once every service is healthy.
up:
	docker compose up --build -d --wait

down:
	docker compose down -v

logs:
	docker compose logs -f api worker

# End-to-end checks against a running stack (make up first).
smoke:
	bash scripts/smoke.sh

# Acceptance suite against a running stack (make up first): one test per requirement of the
# assessment specification, see test/acceptance/requirements.md. About two minutes.
acceptance:
	go test -count=1 -tags=acceptance -timeout 15m -v ./test/acceptance/

build:
	go build -o bin/ ./cmd/...

ifeq ($(OS),Windows_NT)
# Docker Desktop on Windows: testcontainers must mount the Linux-side socket for its reaper
# (not the npipe path), and Git Bash must not rewrite that path.
test: export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock
test: export MSYS_NO_PATHCONV=1
endif

# Unit + integration (testcontainers, requires Docker)
test:
	go test -race -count=1 -tags=integration ./...

test-unit:
	go test -race -count=1 ./...

# Vets every build tag CI vets (integration and acceptance test files too).
lint:
	go vet ./...
	go vet -tags integration ./...
	go vet -tags acceptance ./test/...
	golangci-lint run ./...

# Tests for the Claude Code hooks (.claude/hooks) and the git pre-push hook; bash only.
test-hooks:
	bash scripts/test-hooks.sh

# Use the repository's git hooks (pre-push refuses eval/* branches).
git-hooks:
	git config core.hooksPath scripts/git-hooks

# Lint the OpenAPI contract (Docker). MSYS_NO_PATHCONV stops Git Bash rewriting container paths.
lint-api:
	MSYS_NO_PATHCONV=1 docker run --rm -v "$(CURDIR):/spec" -w /spec redocly/cli:$(REDOCLY_VERSION) lint api/openapi.yaml

# Check the Prometheus alert rules and run their unit tests (deploy/prometheus/alerts_test.yml).
lint-alerts:
	MSYS_NO_PATHCONV=1 docker run --rm -v "$(CURDIR)/deploy/prometheus:/rules" -w /rules --entrypoint promtool prom/prometheus:$(PROMETHEUS_VERSION) check rules alerts.yml
	MSYS_NO_PATHCONV=1 docker run --rm -v "$(CURDIR)/deploy/prometheus:/rules" -w /rules --entrypoint promtool prom/prometheus:$(PROMETHEUS_VERSION) test rules alerts_test.yml

# MSYS_NO_PATHCONV stops Git Bash rewriting -path=/migrations into a Windows path.
migrate-up:
	MSYS_NO_PATHCONV=1 docker compose run --rm migrate -path=/migrations -database "$(COMPOSE_DB_URL)" up

migrate-down:
	MSYS_NO_PATHCONV=1 docker compose run --rm migrate -path=/migrations -database "$(COMPOSE_DB_URL)" down 1

tidy:
	go mod tidy
