package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/umutsahin222/insider/internal/domain"
	"github.com/umutsahin222/insider/internal/observability"
)

func TestSend_PropagatesTraceContext(t *testing.T) {
	var gotTraceparent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	c := New(srv.URL, time.Second, &observability.Tracing{Provider: tp, Propagator: observability.Propagator()})

	ctx, parent := tp.Tracer("test").Start(context.Background(), "deliver sms")
	if _, err := c.Send(ctx, domain.Notification{ID: uuid.New(), Recipient: "x", Content: "y"}); err != nil {
		t.Fatal(err)
	}
	parent.End()

	var client sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.SpanKind() == trace.SpanKindClient {
			client = s
		}
	}
	if client == nil {
		t.Fatal("no HTTP client span recorded")
	}
	if client.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Error("client span must be a child of the deliver span")
	}
	want := "00-" + client.SpanContext().TraceID().String() + "-" + client.SpanContext().SpanID().String() + "-01"
	if gotTraceparent != want {
		t.Errorf("provider received traceparent %q, want %q", gotTraceparent, want)
	}
}
