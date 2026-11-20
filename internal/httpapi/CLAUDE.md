# internal/httpapi

The HTTP layer: decode → domain → store → encode. Invariants for this package:

- **`api/openapi.yaml` is the contract.** Any change to routes, request/response fields or
  status codes updates it in the same commit, followed by `make lint-api`. A Stop hook sends
  Claude back if handlers change without the spec.
- **Error mapping lives in one place** (`errors.go`): sentinel errors from `domain` become
  RFC 9457 problems. Internal errors are logged, never returned. Handlers do not pick
  status codes for domain errors themselves.
- No business rules in handlers: validation, state transitions and template rendering happen
  in `internal/domain`. Handlers only parse, call and encode.
- Strict JSON decoding (`DisallowUnknownFields`) and body limits (`maxBody`) on every write route.
- A malformed path ID returns `404` (it cannot name a resource). A malformed query parameter
  returns `400` with field errors.
- Metrics use the chi **route pattern**, never the raw path, and `knownMethod(r.Method)`, never
  the raw method. Consumer-defined interfaces (`Metrics`, `Events`, `EventStream`, `StatsStore`)
  keep Prometheus and Redis types out of this package.
- WebSocket order is subscribe → snapshot (read after subscribing) → live events (ADR-008).
- Tests use `httptest` with the fakes in `notifications_test.go`. Write routes need a test for
  validation errors and idempotent replay where relevant.
