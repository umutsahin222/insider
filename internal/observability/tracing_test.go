package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestTraceContext_RoundTrip(t *testing.T) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(tracetest.NewSpanRecorder()))
	ctx, span := tp.Tracer("test").Start(context.Background(), "api")
	defer span.End()

	carrier := InjectTraceContext(ctx, Propagator())
	if carrier["traceparent"] == "" {
		t.Fatalf("carrier = %v, want a traceparent", carrier)
	}

	// The worker side: a fresh context continues the stored trace.
	got := trace.SpanContextFromContext(ExtractTraceContext(context.Background(), Propagator(), carrier))
	if got.TraceID() != span.SpanContext().TraceID() || got.SpanID() != span.SpanContext().SpanID() || !got.IsRemote() {
		t.Errorf("extracted %v, want remote parent %v", got, span.SpanContext())
	}
}

func TestTraceContext_NoSpan(t *testing.T) {
	if c := InjectTraceContext(context.Background(), Propagator()); c != nil {
		t.Errorf("carrier = %v, want nil without a span", c)
	}
	ctx := context.Background()
	if ExtractTraceContext(ctx, Propagator(), nil) != ctx {
		t.Error("empty carrier must return ctx unchanged")
	}
}

func TestDisabledTracingStillPropagates(t *testing.T) {
	// With exporting off, an incoming traceparent must still flow API → DB → worker → provider.
	tr, err := NewTracing(context.Background(), TracingConfig{ServiceName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	remote := map[string]string{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}
	ctx := ExtractTraceContext(context.Background(), tr.Propagator, remote)
	ctx, span := tr.Provider.Tracer("test").Start(ctx, "deliver")
	defer span.End()

	if got := InjectTraceContext(ctx, tr.Propagator)["traceparent"]; got == "" || got[3:35] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("traceparent = %q, want trace 4bf9…4736 continued", got)
	}
	if err := tr.Shutdown(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestLoggerAddsTraceIDs(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	defer span.End()

	var buf bytes.Buffer
	NewLogger(&buf, slog.LevelInfo).InfoContext(ctx, "hello")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["trace_id"] != span.SpanContext().TraceID().String() || rec["span_id"] != span.SpanContext().SpanID().String() {
		t.Errorf("record = %v, want trace_id/span_id of the active span", rec)
	}
}
