package delivery

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/umutsahin222/insider/internal/domain"
	"github.com/umutsahin222/insider/internal/observability"
)

const apiTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

func tracedWorker(t *testing.T, s Sender) (*Worker, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	w := NewWorker(testConfig(), Deps{
		Queue: &fakeQueue{}, Sender: s, Limiter: allow, Logger: testLogger(),
		Tracing: &observability.Tracing{Provider: tp, Propagator: observability.Propagator()},
	})
	return w, rec
}

func fromAPI(attempts int) domain.Notification {
	n := claimed(attempts, 5)
	n.TraceContext = map[string]string{"traceparent": "00-" + apiTraceID + "-00f067aa0ba902b7-01"}
	return n
}

func spanNamed(t *testing.T, rec *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range rec.Ended() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("no span %q among %d", name, len(rec.Ended()))
	return nil
}

func attr(s sdktrace.ReadOnlySpan, key string) attribute.Value {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value
		}
	}
	return attribute.Value{}
}

func TestTracing_DeliveryContinuesAPITrace(t *testing.T) {
	var sendCtx context.Context
	s := senderFunc(func(ctx context.Context, _ domain.Notification) (domain.ProviderReply, error) {
		sendCtx = ctx
		return domain.ProviderReply{StatusCode: 202}, nil
	})
	w, rec := tracedWorker(t, s)

	w.process(context.Background(), fromAPI(2))

	span := spanNamed(t, rec, "deliver sms")
	if span.SpanContext().TraceID().String() != apiTraceID || span.Parent().SpanID().String() != "00f067aa0ba902b7" {
		t.Errorf("deliver span trace=%s parent=%s, want child of the API span", span.SpanContext().TraceID(), span.Parent().SpanID())
	}
	if span.SpanKind() != trace.SpanKindConsumer || attr(span, "notification.attempt").AsInt64() != 2 ||
		attr(span, "delivery.result").AsString() != "sent" || span.Status().Code == codes.Error {
		t.Errorf("deliver span kind=%v attrs=%v status=%v", span.SpanKind(), span.Attributes(), span.Status())
	}
	if wait := spanNamed(t, rec, "ratelimit.wait"); wait.Parent().SpanID() != span.SpanContext().SpanID() {
		t.Error("ratelimit.wait must be a child of the deliver span")
	}
	// The provider call runs inside the deliver span, so its HTTP client span nests under it.
	if got := trace.SpanContextFromContext(sendCtx).SpanID(); got != span.SpanContext().SpanID() {
		t.Errorf("sender ctx span = %s, want the deliver span", got)
	}
}

func TestTracing_FailedAttemptMarksSpanError(t *testing.T) {
	w, rec := tracedWorker(t, &scriptedSender{reply: domain.ProviderReply{StatusCode: 503}})

	w.process(context.Background(), fromAPI(1))

	span := spanNamed(t, rec, "deliver sms")
	if span.Status().Code != codes.Error || attr(span, "delivery.result").AsString() != "retry_scheduled" ||
		attr(span, "http.response.status_code").AsInt64() != 503 {
		t.Errorf("status=%v attrs=%v", span.Status(), span.Attributes())
	}
}

type senderFunc func(context.Context, domain.Notification) (domain.ProviderReply, error)

func (f senderFunc) Send(ctx context.Context, n domain.Notification) (domain.ProviderReply, error) {
	return f(ctx, n)
}
