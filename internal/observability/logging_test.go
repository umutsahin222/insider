package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestLoggerAddsContextIDs(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo).With("component", "test")

	ctx := WithNotificationID(WithCorrelationID(context.Background(), "corr-1"), "n-1")
	log.InfoContext(ctx, "hello")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, buf.String())
	}
	for k, want := range map[string]string{"correlation_id": "corr-1", "notification_id": "n-1", "component": "test"} {
		if rec[k] != want {
			t.Errorf("%s = %v, want %q", k, rec[k], want)
		}
	}
}

func TestLoggerOmitsMissingIDs(t *testing.T) {
	var buf bytes.Buffer
	NewLogger(&buf, slog.LevelInfo).InfoContext(context.Background(), "hello")
	if bytes.Contains(buf.Bytes(), []byte("correlation_id")) {
		t.Errorf("unexpected correlation_id in %s", buf.String())
	}
}
