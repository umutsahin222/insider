package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/umutsahin222/insider/internal/domain"
)

func TestSend(t *testing.T) {
	n := domain.Notification{ID: uuid.New(), Channel: domain.ChannelSMS, Recipient: "+905551234567", Content: "hi", CorrelationID: "corr-1"}

	var got struct {
		body    sendRequest
		idemKey string
		corrID  string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		got.idemKey, got.corrID = r.Header.Get("X-Idempotency-Key"), r.Header.Get("X-Correlation-ID")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"messageId":"m-1","status":"accepted","timestamp":"2026-11-18T12:00:00Z"}`))
	}))
	defer srv.Close()

	reply, err := New(srv.URL, time.Second).Send(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	if reply.StatusCode != 202 || reply.MessageID != "m-1" {
		t.Errorf("reply = %+v", reply)
	}
	if got.body != (sendRequest{To: "+905551234567", Channel: "sms", Content: "hi"}) {
		t.Errorf("request body = %+v", got.body)
	}
	if got.idemKey != n.ID.String() || got.corrID != "corr-1" {
		t.Errorf("headers: idempotency=%q correlation=%q", got.idemKey, got.corrID)
	}
}

func TestSend_ErrorStatusesAreRepliesNotErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	reply, err := New(srv.URL, time.Second).Send(context.Background(), domain.Notification{ID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	if reply.StatusCode != 429 || reply.RetryAfter != 7*time.Second || reply.MessageID != "" {
		t.Errorf("reply = %+v", reply)
	}
}

func TestSend_TimeoutIsAnError(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	defer close(release) // runs before srv.Close, so the handler returns and Close doesn't block

	_, err := New(srv.URL, 50*time.Millisecond).Send(context.Background(), domain.Notification{ID: uuid.New()})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var netErr interface{ Timeout() bool }
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("err = %v, want a timeout", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	tests := map[string]time.Duration{
		"":                              0,
		"5":                             5 * time.Second,
		"0":                             0,
		"-3":                            0,
		"soon":                          0,
		"Wed, 18 Nov 2026 12:00:30 GMT": 30 * time.Second,
		"Wed, 18 Nov 2026 11:00:00 GMT": 0, // in the past
	}
	for in, want := range tests {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}
