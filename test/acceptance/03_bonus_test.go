//go:build acceptance

package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// REQ-BONUS-SCHED: scheduled notifications are delivered at, not before, their time.
func TestREQ_BONUS_ScheduledNotifications(t *testing.T) {
	due := time.Now().Add(6 * time.Second).Truncate(time.Second)
	n := create(t, map[string]any{"channel": "push", "recipient": "tok-sched", "content": run + " sched", "scheduled_at": iso(due)})
	id := n["id"].(string)
	time.Sleep(2 * time.Second)
	if s := notification(t, id)["status"]; s != "pending" {
		t.Fatalf("status 2 s before due = %v, want pending", s)
	}
	eventually(t, 60*time.Second, "scheduled notification to be sent", func() bool { return notification(t, id)["status"] == "sent" })
	for _, a := range notification(t, id)["delivery_attempts"].([]any) {
		at, _ := time.Parse(time.RFC3339Nano, a.(map[string]any)["created_at"].(string))
		if at.Before(due) {
			t.Errorf("attempt at %s, before scheduled_at %s", at, due)
		}
	}
}

// REQ-BONUS-TEMPLATES: message templates with variable substitution.
func TestREQ_BONUS_Templates(t *testing.T) {
	name := run + "-otp"
	r := call(t, http.MethodPost, "/v1/templates", map[string]any{"name": name, "channel": "sms",
		"body": `Hi {{.name}}, your code is {{.code}}{{if eq .tier "gold"}} (priority support){{end}}`})
	mustStatus(t, r, http.StatusCreated, "create template")

	n := create(t, map[string]any{"channel": "sms", "recipient": "+905551234567",
		"template": map[string]any{"name": name, "variables": map[string]string{"name": "Ada", "code": "4242", "tier": "gold"}}})
	if n["content"] != "Hi Ada, your code is 4242 (priority support)" || n["template_id"] == nil {
		t.Errorf("rendered = %v (template_id %v)", n["content"], n["template_id"])
	}

	for _, c := range []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"channel mismatch", map[string]any{"channel": "email", "recipient": "a@example.com", "template": map[string]any{"name": name, "variables": map[string]string{"name": "A", "code": "1", "tier": "x"}}}, "template.name"},
		{"missing variable", map[string]any{"channel": "sms", "recipient": "+905551234567", "template": map[string]any{"name": name, "variables": map[string]string{"name": "A"}}}, "template.variables"},
		{"unknown template", map[string]any{"channel": "sms", "recipient": "+905551234567", "template": map[string]any{"name": run + "-nope"}}, "template.name"},
	} {
		r := call(t, http.MethodPost, "/v1/notifications", c.body)
		if r.Status != http.StatusUnprocessableEntity || !slices.Equal(r.fields(t), []string{c.field}) {
			t.Errorf("%s: %d %s, want 422 on %s", c.name, r.Status, r.Body, c.field)
		}
	}
	// Bodies that could make rendering unbounded work are rejected at creation (ADR-009).
	r = call(t, http.MethodPost, "/v1/templates", map[string]any{"name": run + "-loop", "channel": "sms", "body": "{{range 1000000}}x{{end}}"})
	mustStatus(t, r, http.StatusUnprocessableEntity, "unsafe template")
}

// REQ-BONUS-WS: real-time status updates via WebSocket - a snapshot first, then live events.
func TestREQ_BONUS_WebSocket(t *testing.T) {
	items := make([]map[string]any, 20)
	for i := range items {
		items[i] = map[string]any{"channel": "push", "recipient": fmt.Sprintf("ws-%d", i), "content": run + " ws",
			"scheduled_at": iso(time.Now().Add(3 * time.Second))}
	}
	id, ids := batch(t, items)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(apiURL, "http")+"/v1/ws?batch_id="+id, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	type event struct {
		NotificationID string `json:"notification_id"`
		Status         string `json:"status"`
	}
	var events []event
	sent := map[string]bool{}
	for len(sent) < len(ids) {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read after %d events (%d sent): %v", len(events), len(sent), err)
		}
		var e event
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		events = append(events, e)
		if e.Status == "sent" {
			sent[e.NotificationID] = true
		}
	}
	t.Logf("%d events for %d notifications", len(events), len(ids))
	for i, e := range events[:len(ids)] {
		if e.Status != "pending" {
			t.Fatalf("event %d = %+v, want the snapshot (all pending) first", i, e)
		}
	}
	if !slices.ContainsFunc(events[len(ids):], func(e event) bool { return e.Status == "processing" }) {
		t.Error("no live processing events after the snapshot")
	}
}

// REQ-BONUS-TRACING: one distributed trace spans the API request, the worker's attempt and the
// provider call.
func TestREQ_BONUS_DistributedTracing(t *testing.T) {
	traceID := strings.ReplaceAll(uuid.NewString(), "-", "")
	n := create(t, map[string]any{"channel": "email", "recipient": "trace@example.com", "content": run + " traced"},
		"traceparent", "00-"+traceID+"-"+traceID[:16]+"-01")
	eventually(t, 60*time.Second, "traced notification to be sent", func() bool { return notification(t, n["id"].(string))["status"] == "sent" })
	eventually(t, 30*time.Second, "trace in Jaeger with both services", func() bool {
		r := call(t, http.MethodGet, jaegerURL+"/api/v3/traces/"+traceID, nil)
		body := string(r.Body)
		return r.Status == http.StatusOK && strings.Contains(body, "notification-api") && strings.Contains(body, "notification-worker")
	})
}

// REQ-DELIV-DOCS: the API documentation (OpenAPI) is served by the running API.
func TestREQ_DELIV_APIDocumentation(t *testing.T) {
	spec := call(t, http.MethodGet, "/openapi.yaml", nil)
	mustStatus(t, spec, http.StatusOK, "GET /openapi.yaml")
	if !strings.HasPrefix(string(spec.Body), "openapi: 3.1") {
		t.Errorf("/openapi.yaml does not start with an OpenAPI 3.1 header")
	}
	docs := call(t, http.MethodGet, "/docs", nil)
	mustStatus(t, docs, http.StatusOK, "GET /docs")
	if !strings.Contains(string(docs.Body), "swagger-ui") {
		t.Error("/docs is not the Swagger UI page")
	}
}
