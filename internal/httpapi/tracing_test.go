package httpapi

import (
	"net/http"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/umutsahin222/insider/internal/observability"
)

func newTracedServer(t *testing.T) (http.Handler, *fakeStore, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	store := newFakeStore()
	deps := testDeps(store, nil)
	deps.Tracing = &observability.Tracing{Provider: tp, Propagator: observability.Propagator()}
	return NewRouter(deps), store, rec
}

func TestTracing_StoresRequestTraceWithNotification(t *testing.T) {
	h, store, rec := newTracedServer(t)

	created := decode[notificationResponse](t, do(t, h, http.MethodPost, "/v1/notifications", validBody))

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1 server span", len(spans))
	}
	span := spans[0]
	if span.Name() != "POST /v1/notifications" {
		t.Errorf("span name = %q, want the route pattern", span.Name())
	}
	// The stored traceparent must point at this request's span: the worker continues from it.
	tp := store.byID[created.ID].TraceContext["traceparent"]
	want := "00-" + span.SpanContext().TraceID().String() + "-" + span.SpanContext().SpanID().String() + "-01"
	if tp != want {
		t.Errorf("stored traceparent = %q, want %q", tp, want)
	}
}

func TestTracing_ContinuesClientTrace(t *testing.T) {
	h, store, rec := newTracedServer(t)
	const clientTrace = "4bf92f3577b34da6a3ce929d0e0e4736"

	resp := decode[batchCreatedResponse](t, do(t, h, http.MethodPost, "/v1/notifications/batch",
		`{"items":[`+validBody+`,`+validBody+`]}`,
		"traceparent", "00-"+clientTrace+"-00f067aa0ba902b7-01"))

	if got := rec.Ended()[0].SpanContext().TraceID().String(); got != clientTrace {
		t.Errorf("server span trace = %s, want the client's %s", got, clientTrace)
	}
	for _, id := range resp.NotificationIDs {
		if tp := store.byID[id].TraceContext["traceparent"]; len(tp) < 35 || tp[3:35] != clientTrace {
			t.Errorf("notification %s traceparent = %q, want client trace", id, tp)
		}
	}
}

func TestTracing_SkipsProbes(t *testing.T) {
	h, _, rec := newTracedServer(t)
	do(t, h, http.MethodGet, "/healthz", "")
	do(t, h, http.MethodGet, "/readyz", "")
	if n := len(rec.Ended()); n != 0 {
		t.Errorf("spans = %d, want none for health probes", n)
	}
}
