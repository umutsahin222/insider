// Package provider is the HTTP client for the external notification provider
// (webhook.site or cmd/mockprovider).
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/umutsahin222/insider/internal/domain"
	"github.com/umutsahin222/insider/internal/observability"
)

// Client sends notifications to the provider endpoint.
type Client struct {
	url  string
	http *http.Client
}

// New returns a client posting to url. Per-request deadlines come from the context;
// timeout is an upper bound for any single request. Each call becomes an HTTP client span
// (a child of the worker's deliver span) and carries a traceparent header; nil tracing
// still propagates the incoming context.
func New(url string, timeout time.Duration, tracing *observability.Tracing) *Client {
	if tracing == nil {
		tracing = observability.DisabledTracing()
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 64 // one connection per concurrent channel worker
	traced := otelhttp.NewTransport(transport,
		otelhttp.WithTracerProvider(tracing.Provider), otelhttp.WithPropagators(tracing.Propagator))
	return &Client{url: url, http: &http.Client{Timeout: timeout, Transport: traced}}
}

type sendRequest struct {
	To      string `json:"to"`
	Channel string `json:"channel"`
	Content string `json:"content"`
}

type sendResponse struct {
	MessageID string `json:"messageId"`
}

// Send posts one notification. A non-nil error means no HTTP response was received
// (timeout, connection refused, ...); any HTTP status is returned in the reply instead.
func (c *Client) Send(ctx context.Context, n domain.Notification) (domain.ProviderReply, error) {
	body, err := json.Marshal(sendRequest{To: n.Recipient, Channel: string(n.Channel), Content: n.Content})
	if err != nil {
		return domain.ProviderReply{}, fmt.Errorf("encoding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return domain.ProviderReply{}, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// The notification ID is stable across retries, letting the provider de-duplicate
	// the at-least-once deliveries this system makes (ADR-002).
	req.Header.Set("X-Idempotency-Key", n.ID.String())
	if n.CorrelationID != "" {
		req.Header.Set("X-Correlation-ID", n.CorrelationID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return domain.ProviderReply{}, fmt.Errorf("calling provider: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	reply := domain.ProviderReply{StatusCode: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var sr sendResponse
		if json.Unmarshal(raw, &sr) == nil {
			reply.MessageID = sr.MessageID
		}
	}
	return reply, nil
}

// parseRetryAfter accepts both forms of the header: delay-seconds and an HTTP date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
