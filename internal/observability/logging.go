// Package observability provides structured logging and request correlation helpers.
package observability

import (
	"context"
	"io"
	"log/slog"
)

type ctxKey int

const (
	correlationIDKey ctxKey = iota
	notificationIDKey
)

// WithCorrelationID returns a context carrying the request's correlation ID.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationIDKey, id)
}

// CorrelationID returns the correlation ID stored in ctx, or "".
func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationIDKey).(string)
	return id
}

// WithNotificationID returns a context whose log records carry notification_id.
func WithNotificationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, notificationIDKey, id)
}

// NewLogger returns a JSON logger that adds correlation_id / notification_id from the
// context to every record logged with the *Context methods.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(contextHandler{slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})})
}

type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := CorrelationID(ctx); id != "" {
		r.AddAttrs(slog.String("correlation_id", id))
	}
	if id, _ := ctx.Value(notificationIDKey).(string); id != "" {
		r.AddAttrs(slog.String("notification_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}
