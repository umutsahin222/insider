//go:build acceptance

// Package acceptance checks the running system against the assessment specification,
// black-box: only the public API, the mock provider's /stats, Prometheus, Jaeger and container
// logs. Every test name carries the ID of the requirement it verifies; requirements.md maps the
// requirements of the specification to these tests.
//
// Start the stack first (make up), then: make acceptance
package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var (
	apiURL    = env("API_URL", "http://localhost:8080")
	mockURL   = env("MOCK_PROVIDER_URL", "http://localhost:8090")
	promURL   = env("PROMETHEUS_URL", "http://localhost:9090")
	jaegerURL = env("JAEGER_URL", "http://localhost:16686")
	// run marks everything this run creates, so tests can find their own rows.
	run    = "acc-" + uuid.NewString()[:8]
	client = &http.Client{Timeout: 60 * time.Second}
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/readyz", nil)
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "acceptance: the stack is not ready at %s; start it with `make up`\n", apiURL)
			os.Exit(1)
		}
		time.Sleep(500 * time.Millisecond)
	}
	os.Exit(m.Run())
}

// response is an HTTP response with its body read.
type response struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r response) obj(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(r.Body, &v); err != nil {
		t.Fatalf("decoding %d response %q: %v", r.Status, r.Body, err)
	}
	return v
}

// fields returns the field names of an RFC 9457 validation error.
func (r response) fields(t *testing.T) []string {
	t.Helper()
	var out []string
	errs, _ := r.obj(t)["errors"].([]any)
	for _, e := range errs {
		if m, ok := e.(map[string]any); ok {
			out = append(out, fmt.Sprint(m["field"]))
		}
	}
	return out
}

// call sends a request. A path starting with "http" is used as is; otherwise it is relative to
// the API. body may be nil, a []byte (sent raw) or a value encoded as JSON. headers are pairs.
func call(t *testing.T, method, path string, body any, headers ...string) response {
	t.Helper()
	url := path
	if !strings.HasPrefix(path, "http") {
		url = apiURL + path
	}
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return response{Status: resp.StatusCode, Header: resp.Header, Body: raw}
}

// mustStatus fails the test unless r has the wanted status.
func mustStatus(t *testing.T, r response, want int, what string) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("%s: status %d, want %d: %s", what, r.Status, want, r.Body)
	}
}

// eventually polls cond until it returns true or the timeout passes.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// create posts one notification and returns its JSON.
func create(t *testing.T, body map[string]any, headers ...string) map[string]any {
	t.Helper()
	r := call(t, http.MethodPost, "/v1/notifications", body, headers...)
	if r.Status != http.StatusCreated && r.Status != http.StatusOK {
		t.Fatalf("create: status %d: %s", r.Status, r.Body)
	}
	return r.obj(t)
}

// batch posts items and returns the batch ID and notification IDs.
func batch(t *testing.T, items []map[string]any, headers ...string) (string, []string) {
	t.Helper()
	r := call(t, http.MethodPost, "/v1/notifications/batch", map[string]any{"items": items}, headers...)
	mustStatus(t, r, http.StatusCreated, "create batch")
	b := r.obj(t)
	var ids []string
	for _, id := range b["notification_ids"].([]any) {
		ids = append(ids, id.(string))
	}
	return b["batch_id"].(string), ids
}

func notification(t *testing.T, id string) map[string]any {
	t.Helper()
	r := call(t, http.MethodGet, "/v1/notifications/"+id, nil)
	mustStatus(t, r, http.StatusOK, "get notification")
	return r.obj(t)
}

// batchCounts returns a batch summary's counts per status.
func batchCounts(t *testing.T, id string) map[string]int {
	t.Helper()
	r := call(t, http.MethodGet, "/v1/batches/"+id, nil)
	mustStatus(t, r, http.StatusOK, "get batch")
	out := map[string]int{}
	for k, v := range r.obj(t)["counts"].(map[string]any) {
		out[k] = int(v.(float64))
	}
	return out
}

// waitBatchSettled waits until no notification of the batch is pending or processing.
func waitBatchSettled(t *testing.T, id string, timeout time.Duration) map[string]int {
	t.Helper()
	var counts map[string]int
	eventually(t, timeout, "batch "+id+" to settle", func() bool {
		counts = batchCounts(t, id)
		return counts["pending"]+counts["processing"] == 0
	})
	return counts
}

// queueIdle reports whether no notification is ready, delayed or processing on any channel.
func queueIdle(t *testing.T) bool {
	t.Helper()
	r := call(t, http.MethodGet, "/v1/stats?window=10s", nil)
	if r.Status != http.StatusOK {
		return false
	}
	for _, q := range r.obj(t)["queue"].([]any) {
		m := q.(map[string]any)
		if m["ready"].(float64)+m["processing"].(float64) > 0 {
			return false
		}
	}
	return true
}

func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func sms(i int, content string) map[string]any {
	return map[string]any{"channel": "sms", "recipient": fmt.Sprintf("+90555%07d", i), "content": content}
}
