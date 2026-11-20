package httpapi

import (
	"fmt"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/umutsahin222/insider/internal/observability"
)

const correlationHeader = "X-Correlation-ID"

// validCorrelationID keeps client-supplied IDs short and log-safe.
var validCorrelationID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// correlation reads X-Correlation-ID (or generates one), stores it in the request
// context for logs and persistence, and echoes it in the response.
func correlation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(correlationHeader)
		if !validCorrelationID.MatchString(id) {
			id = uuid.NewString()
		}
		w.Header().Set(correlationHeader, id)
		next.ServeHTTP(w, r.WithContext(observability.WithCorrelationID(r.Context(), id)))
	})
}

// observe writes one structured access-log line per request and records request metrics.
// Metric labels and span names use the matched route pattern and a normalised method, so
// clients cannot create unbounded series: net/http accepts any token as a method.
func (a *api) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		elapsed := time.Since(start)

		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		status := ww.Status()
		if status == 0 { // handler wrote nothing: net/http sends 200
			status = http.StatusOK
		}
		method := knownMethod(r.Method)
		a.metrics.ObserveRequest(method, route, status, elapsed)
		if span := trace.SpanFromContext(r.Context()); span.IsRecording() {
			span.SetName(method + " " + route)
			span.SetAttributes(semconv.HTTPRoute(route))
		}

		level := a.log.InfoContext
		switch route {
		case "/healthz", "/readyz", "/metrics":
			level = a.log.DebugContext
		}
		level(r.Context(), "http request",
			"method", r.Method, "route", route, "path", r.URL.Path,
			"status", status, "bytes", ww.BytesWritten(),
			"duration_ms", elapsed.Milliseconds())
	})
}

// knownMethod maps the request method onto a closed set: the standard methods, and "OTHER"
// for anything else (as OpenTelemetry's semantic conventions do with "_OTHER").
func knownMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return m
	default:
		return "OTHER"
	}
}

// recoverer turns a handler panic into a logged 500 instead of a dropped connection.
func (a *api) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				a.log.ErrorContext(r.Context(), "panic", "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				a.writeError(w, r, fmt.Errorf("panic: %v", v))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// maxBody caps the request body size; reads beyond it fail with *http.MaxBytesError.
func maxBody(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}
