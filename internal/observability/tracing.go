package observability

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracerName identifies spans created by this module's own code.
const TracerName = "github.com/umutsahin222/insider"

// TracingConfig configures OpenTelemetry tracing.
type TracingConfig struct {
	ServiceName string
	// OTLPEndpoint is the base URL of an OTLP/HTTP collector, e.g. http://jaeger:4318.
	// Empty disables exporting; trace context is still propagated.
	OTLPEndpoint string
	// SampleRatio is the share of new traces recorded (0..1); child spans follow their parent.
	SampleRatio float64
}

// Tracing is the wired tracing setup. It is passed explicitly to the components that need it
// rather than installed as the OpenTelemetry global, in line with the no-global-state rule.
type Tracing struct {
	Provider   trace.TracerProvider
	Propagator propagation.TextMapPropagator
	shutdown   func(context.Context) error
}

// Shutdown flushes buffered spans; call it once on exit.
func (t *Tracing) Shutdown(ctx context.Context) error {
	if t.shutdown == nil {
		return nil
	}
	return t.shutdown(ctx)
}

// Propagator is the W3C Trace Context + Baggage propagator used everywhere.
func Propagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
}

// DisabledTracing propagates trace context but records nothing.
func DisabledTracing() *Tracing {
	return &Tracing{Provider: noop.NewTracerProvider(), Propagator: Propagator()}
}

// NewTracing builds the tracer provider. Spans are batched and exported over OTLP/HTTP.
func NewTracing(ctx context.Context, cfg TracingConfig) (*Tracing, error) {
	if cfg.OTLPEndpoint == "" {
		return DisabledTracing(), nil
	}
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(strings.TrimSuffix(cfg.OTLPEndpoint, "/")+"/v1/traces"))
	if err != nil {
		return nil, fmt.Errorf("creating OTLP exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(time.Second)),
		sdktrace.WithResource(resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(cfg.ServiceName))),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	)
	return &Tracing{Provider: tp, Propagator: Propagator(), shutdown: tp.Shutdown}, nil
}

// InjectTraceContext serialises the span context in ctx (traceparent/tracestate) so it can
// be stored with a notification and continued by the worker. It returns nil if ctx carries
// no valid span context.
func InjectTraceContext(ctx context.Context, p propagation.TextMapPropagator) map[string]string {
	carrier := propagation.MapCarrier{}
	p.Inject(ctx, carrier)
	if len(carrier) == 0 {
		return nil
	}
	return carrier
}

// ExtractTraceContext returns ctx with the remote span context stored by InjectTraceContext.
func ExtractTraceContext(ctx context.Context, p propagation.TextMapPropagator, carrier map[string]string) context.Context {
	if len(carrier) == 0 {
		return ctx
	}
	return p.Extract(ctx, propagation.MapCarrier(carrier))
}
