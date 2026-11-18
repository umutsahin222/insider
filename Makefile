.PHONY: run down build test test-unit lint lint-api migrate-up migrate-down tidy

DB_URL ?= postgres://notify:notify@localhost:5432/notify?sslmode=disable
REDOCLY_VERSION ?= 1.25.11

run:
	docker compose up --build

down:
	docker compose down -v

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

# Lint the OpenAPI contract (Docker). MSYS_NO_PATHCONV stops Git Bash rewriting container paths.
lint-api:
	MSYS_NO_PATHCONV=1 docker run --rm -v "$(CURDIR):/spec" -w /spec redocly/cli:$(REDOCLY_VERSION) lint api/openapi.yaml

migrate-up:
	docker compose run --rm migrate -path=/migrations -database "$(DB_URL)" up

migrate-down:
	docker compose run --rm migrate -path=/migrations -database "$(DB_URL)" down 1

tidy:
	go mod tidy
