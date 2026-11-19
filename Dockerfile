# syntax=docker/dockerfile:1
# One image, four static binaries (api, worker, mockprovider, healthcheck); docker-compose picks
# the binary per service. Build: docker build -t insider-notify .

FROM golang:1.27.4-alpine AS build
WORKDIR /src
# Dependencies first, so source edits don't invalidate the module download layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/ ./cmd/api ./cmd/worker ./cmd/mockprovider ./cmd/healthcheck

# Distroless static: no shell or package manager, runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /
USER nonroot:nonroot
EXPOSE 8080 9091 8090
ENTRYPOINT ["/api"]
