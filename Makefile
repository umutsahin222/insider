.PHONY: run up down logs smoke build test test-unit lint lint-api test-hooks git-hooks migrate-up migrate-down tidy

# Inside the compose network (the migrate service talks to the postgres service).
COMPOSE_DB_URL ?= postgres://notify:notify@postgres:5432/notify?sslmode=disable
REDOCLY_VERSION ?= 1.25.11

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

lint:
	go vet ./...
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

migrate-up:
	docker compose run --rm migrate -path=/migrations -database "$(COMPOSE_DB_URL)" up

migrate-down:
	docker compose run --rm migrate -path=/migrations -database "$(COMPOSE_DB_URL)" down 1

tidy:
	go mod tidy
