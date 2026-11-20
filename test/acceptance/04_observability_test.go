//go:build acceptance

package acceptance

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// REQ-OBS-3: health check endpoints.
func TestREQ_OBS3_HealthChecks(t *testing.T) {
	mustStatus(t, call(t, http.MethodGet, "/healthz", nil), http.StatusOK, "liveness")
	r := call(t, http.MethodGet, "/readyz", nil)
	mustStatus(t, r, http.StatusOK, "readiness")
	if b := string(r.Body); !strings.Contains(b, "postgres") || !strings.Contains(b, "redis") {
		t.Errorf("readiness does not report its dependencies: %s", b)
	}
}

// REQ-OBS-1: real-time metrics endpoint with queue depth, success/failure rates and latency.
func TestREQ_OBS1_Metrics(t *testing.T) {
	m := call(t, http.MethodGet, "/metrics", nil)
	mustStatus(t, m, http.StatusOK, "/metrics")
	if !strings.Contains(string(m.Body), "notification_queue_depth{") {
		t.Error("/metrics has no notification_queue_depth series")
	}

	// Make sure the window has deliveries, then read the summary endpoint.
	n := create(t, map[string]any{"channel": "sms", "recipient": "+905551234567", "content": run + " metrics"})
	eventually(t, 60*time.Second, "a delivery", func() bool { return notification(t, n["id"].(string))["status"] == "sent" })
	s := call(t, http.MethodGet, "/v1/stats?window=10m", nil)
	mustStatus(t, s, http.StatusOK, "/v1/stats")
	stats := s.obj(t)
	if q, _ := stats["queue"].([]any); len(q) != 3 {
		t.Errorf("queue depth per channel missing: %v", stats["queue"])
	}
	var sms map[string]any
	for _, d := range stats["delivery"].([]any) {
		if d.(map[string]any)["channel"] == "sms" {
			sms = d.(map[string]any)
		}
	}
	if sms == nil || sms["success_rate"] == nil || sms["failed"] == nil ||
		sms["provider_latency_ms"].(map[string]any)["p95"] == nil || sms["end_to_end_latency_ms"].(map[string]any)["p99"] == nil {
		t.Errorf("sms delivery stats incomplete: %v", sms)
	}

	eventually(t, 30*time.Second, "Prometheus to have worker delivery counters", func() bool {
		r := call(t, http.MethodGet, promURL+"/api/v1/query?query=sum(notification_deliveries_total)", nil)
		return r.Status == http.StatusOK && strings.Contains(string(r.Body), `"value":[`)
	})
}

// REQ-OBS-2: structured logging with correlation IDs, from the API request to the worker.
func TestREQ_OBS2_StructuredLogsWithCorrelationIDs(t *testing.T) {
	corr := run + "-corr"
	r := call(t, http.MethodPost, "/v1/notifications", map[string]any{"channel": "email", "recipient": "log@example.com", "content": run + " logged"},
		"X-Correlation-ID", corr)
	mustStatus(t, r, http.StatusCreated, "create with correlation id")
	n := r.obj(t)
	if r.Header.Get("X-Correlation-ID") != corr || n["correlation_id"] != corr {
		t.Fatalf("correlation id not echoed and stored: header %q, body %v", r.Header.Get("X-Correlation-ID"), n["correlation_id"])
	}
	eventually(t, 60*time.Second, "notification to be sent", func() bool { return notification(t, n["id"].(string))["status"] == "sent" })

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not available; cannot read container logs")
	}
	logs := func(service string) string {
		cmd := exec.Command("docker", "compose", "logs", "--no-color", service)
		cmd.Dir = "../.."
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	eventually(t, 15*time.Second, "worker log line with the correlation id", func() bool {
		for _, line := range strings.Split(logs("worker"), "\n") {
			if strings.Contains(line, `"correlation_id":"`+corr+`"`) && strings.Contains(line, `"msg":"notification sent"`) &&
				strings.Contains(line, `"notification_id":"`+n["id"].(string)+`"`) {
				return true
			}
		}
		return false
	})
	if !strings.Contains(logs("api"), `"correlation_id":"`+corr+`"`) {
		t.Error("API access log has no line with the correlation id")
	}
}
