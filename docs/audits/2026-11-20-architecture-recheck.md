# Audit report: architecture-auditor re-check after hardening

- **Date:** 2026-11-20
- **Commit:** `693366b` (branch `main`, clean working tree)
- **Scope:** `all`, every non-test Go file under `internal/`, `cmd/` and `api/`
- **Why:** in the first `/audit all` (`2026-11-20-all.md`), architecture-auditor returned "no
  findings" after three tool calls. The agent was then given four mandatory checks and a
  `checks_executed` output field (commit `693366b`). This run checks that the hardened agent
  really covers the ground.

## Before and after

| Run | Agent definition | Tool calls | Duration | Coverage reported | Findings |
|---|---|---|---|---|---|
| First `/audit all` | original | 3 | 38 s | none | 0 |
| Re-run, same session | hardened on disk, old one still loaded | 3 | 20 s | none (plain `[]`) | 0 |
| **Re-run after restart** | **hardened** | **25** | **94 s** | **4 of 4 checks, with evidence** | **1 (confirmed)** |

The middle row is a lesson in itself. Subagent definitions are loaded when a Claude Code session
starts, so the edited agent only took effect after a restart. Under the new `/audit` rule, that
plain `[]` would have been reported as "incomplete coverage", not as a clean result.

## Checks executed

| Check | Evidence | Result |
|---|---|---|
| dependency-direction | `go list` over 15 packages; 27 project-internal edges checked. `domain` has 0 internal imports. Only `cmd/api` and `cmd/worker` wire implementations. | pass |
| global-state-scan | 38 non-test files, 6 pattern groups. 0 Prometheus or OTel globals. The 2 `slog.SetDefault` calls are in `main` (wiring layer, nothing under `internal/` reads the default). 3 `os.LookupEnv`/`Getenv` hits (config, tooling). All 20 package-level vars were opened: sentinels, regexps, constant tables, a Lua script, `go:embed`. | pass |
| metric-label-cardinality | 14 metrics, 9 label names, about 13 caller sites traced to their sources (enums, constants, `RoutePattern()`). | **fail → ARCH-1** |
| business-rule-leakage | All 14 non-test files in `internal/httpapi` (9) and `internal/store/postgres` (5) read in full. Every status change is a conditional UPDATE; the reaper's rule is the documented exception. | pass |

## Confirmed finding

### ARCH-1 · medium · `internal/httpapi/middleware.go:54`
- **Rule:** CLAUDE.md and ADR-007, "metric labels come from closed sets".
- **Evidence:** `observe()` passes the raw, client-supplied `r.Method` to `ObserveRequest`, and
  `internal/metrics/metrics.go:74-77` uses it unmodified as the `method` label of
  `http_requests_total` and `http_request_duration_seconds`.
- **Failure scenario:** an unauthenticated client sends `FOO1`, `FOO2`, …. Each token creates a
  new counter series and a new histogram series with 13 samples, and series are never evicted.
  API memory, the Prometheus series count and the scrape payload grow without limit.
- **Verifier (finding-verifier, confidence 9):**
  - net/http accepts any RFC token as a method (`validMethod` is `isToken`).
  - chi v5 sends an unregistered method straight to the project's `MethodNotAllowed` handler
    (405) without rewriting `r.Method`.
  - `observe` wraps routing, so it records every such request with route `unmatched`.
  - No code bounds the method: otelhttp, correlation and the recoverer do not touch it.
  - Existing tests use only standard methods.
- **Suggested fix:** map the method onto a closed set (the standard methods; anything else
  becomes `OTHER`, like OpenTelemetry's `_OTHER`) before `ObserveRequest`, with a table-driven
  test that sends a non-standard method.

The first audit missed this finding: its architecture pass never traced label values.

This audit changed no code.
