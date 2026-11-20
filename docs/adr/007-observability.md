# ADR-007: Observability — metrics, stats endpoint and tracing

- Status: Accepted
- Date: 2026-11-19

## Context
The brief asks for a real-time metrics endpoint (queue depth, success/failure rates, latency),
structured logs with correlation IDs and health checks; distributed tracing is a bonus. The system
is two binaries (API, worker) that may each run as several replicas, and a notification's life
spans both: accepted by the API, delivered later — possibly hours later, after retries — by a
worker.

## Decision

### Prometheus metrics (`internal/metrics`)
- **Each binary owns a registry** (no `prometheus.DefaultRegisterer`); components receive small
  consumer-defined interfaces (`httpapi.Metrics`, `delivery.Metrics`) with no-op defaults, so the
  business packages never import Prometheus.
- **API** (`/metrics`): `http_requests_total{method,route,status}`,
  `http_request_duration_seconds{method,route}` — labelled by **route pattern**, never raw path, and by a normalised method (standard methods, anything else `OTHER`: net/http accepts any token as a method, found by the hardened architecture-auditor) —
  and `notifications_accepted_total{channel,priority}` (replays excluded).
- **Worker** (`/metrics` on its own `HTTP_ADDR`, default `:9091`, with `/healthz` and `/readyz`):
  `notification_deliveries_total{channel,outcome,result}` (provider outcome × resulting state:
  sent / retry_scheduled / failed / lease_lost / store_error),
  `notification_provider_request_duration_seconds{channel,outcome}`,
  `notification_end_to_end_latency_seconds{channel}` (from *due* — creation or scheduled time — to
  sent, so scheduled messages don't look slow), `notification_rate_limit_wait_seconds{channel}`,
  `notification_releases_total{channel,reason}`, `notification_circuit_breaker_state{channel}`,
  `notification_reaper_reclaimed_total`.
- **Queue depth is exported by the API, read from the database at scrape time** by a custom
  collector: `notification_queue_depth{channel,priority,state=ready|delayed|processing}`,
  `notification_queue_oldest_ready_age_seconds{channel}` (queue lag — the alerting signal) and
  `notification_queue_stats_up`. The database is the only place that knows the true backlog across
  replicas; a periodic snapshot would be stale, so the collector queries on scrape, cached for 1 s
  so several scrapers can't hammer Postgres, and zero-fills every series so none vanish from graphs.
  All labels come from closed sets; cardinality is fixed (27 depth series).

### `GET /v1/stats`
Prometheus gives rates only through PromQL. `GET /v1/stats?window=5m` answers the brief's question
with one curl: current backlog per channel and priority, plus — over the window — provider calls by
outcome, success rate (`null`, not 0, without calls), sent/failed counts and p50/p95/p99 provider
and end-to-end latency. It is computed in SQL (`percentile_cont`) on the database clock, so it
aggregates every worker replica; migration 000005 adds the indexes the windowed queries need.

### Tracing (OpenTelemetry)
- The API opens a server span per request (otelhttp; renamed to the route pattern after routing;
  health probes and scrapes excluded) and **stores the W3C trace context in
  `notifications.trace_context`**. The worker extracts it and opens a consumer span per attempt
  (`deliver <channel>`, with a `ratelimit.wait` child); the provider call is an otelhttp client span
  that sends `traceparent` onward. The API request and every retry therefore form **one trace**.
- Parent-child (rather than span links) was chosen because each notification has exactly one
  producer and Jaeger then shows the whole story in one view; the trade-off is that a scheduled
  notification's trace spans hours.
- **No OTel globals:** the tracer provider and propagator are built in `main` and passed in
  (`observability.Tracing`). Export is OTLP/HTTP, on when `OTEL_EXPORTER_OTLP_ENDPOINT` is set;
  sampling is parent-based with `TRACE_SAMPLE_RATIO`. With export off, context still propagates,
  so enabling tracing later needs no data migration.
- Logs carry `trace_id`/`span_id` beside `correlation_id`; the correlation ID stays the business
  identifier clients see, the trace ID is for engineers.

## Consequences
- + Every requirement is observable from three angles: Prometheus for dashboards/alerts,
  `/v1/stats` for humans and API consumers, Jaeger for a single notification's journey.
- + It already paid off: the end-to-end latency in `/v1/stats` exposed a circuit-breaker policy
  that paused a healthy-but-noisy channel (ADR-002, revised).
- − Queue-depth `COUNT`s are O(backlog) per scrape (bounded by the 1 s cache); at very large
  backlogs an estimate (e.g. `pg_class.reltuples` per partition) would replace exact counts.
- − Worker end-to-end latency mixes the worker clock with database timestamps; skew is clamped
  at 0. `/v1/stats` uses the database clock only and is the reference figure.
- − Several API replicas export identical queue gauges; dashboards use `max by (...)`.
