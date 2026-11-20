//go:build acceptance

package acceptance

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"
)

// REQ-DEL-RETRY: failed provider calls are retried and every attempt is audited. The mock
// provider fails a share of calls on purpose (FAILURE_RATE, 10 % by default).
func TestREQ_DEL_RetriesAreAudited(t *testing.T) {
	items := make([]map[string]any, 200)
	for i := range items {
		items[i] = map[string]any{"channel": "push", "recipient": fmt.Sprintf("retry-%d", i), "content": run + " retry"}
	}
	id, ids := batch(t, items)
	counts := waitBatchSettled(t, id, 2*time.Minute)
	retried := 0
	for _, nid := range ids {
		n := notification(t, nid)
		attempts := n["delivery_attempts"].([]any)
		for _, a := range attempts {
			m := a.(map[string]any)
			// Timeouts have no status code, but every failed attempt records why it failed.
			if m["outcome"] != "success" && m["error"] == nil {
				t.Errorf("failed attempt without an error: %v", m)
			}
		}
		if len(attempts) > 1 {
			retried++
		}
	}
	t.Logf("%d of 200 needed more than one attempt; final counts %v", retried, counts)
	if retried == 0 {
		t.Skip("no failure was injected (is FAILURE_RATE 0?); retries not observable")
	}
	if counts["sent"] < 198 {
		t.Errorf("only %d of 200 sent after retries: %v", counts["sent"], counts)
	}
}

// REQ-BONUS-FAILURE: a notification that keeps failing is dead-lettered after its attempts and
// can be retried manually. Runs last: it makes the mock provider fail every call for a while.
func TestREQ_BONUS_DeadLetterAndManualRetry(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not available; cannot reconfigure the mock provider")
	}
	// Other traffic would trip the circuit breaker under a 100 % failure rate.
	eventually(t, 3*time.Minute, "the queue to be idle", func() bool { return queueIdle(t) })
	original, set := os.LookupEnv("FAILURE_RATE")
	setFailureRate(t, "1")
	t.Cleanup(func() {
		if set {
			setFailureRate(t, original)
		} else {
			setFailureRate(t, "")
		}
	})

	n := create(t, map[string]any{"channel": "push", "recipient": "tok-dead", "content": run + " dead letter"})
	id := n["id"].(string)
	var dead map[string]any
	eventually(t, 2*time.Minute, "the notification to be dead-lettered", func() bool {
		dead = notification(t, id)
		return dead["status"] == "failed"
	})
	if dead["attempts"].(float64) != 5 || len(dead["delivery_attempts"].([]any)) != 5 || dead["last_error"] == nil {
		t.Fatalf("dead letter = attempts %v, %d audited, last_error %v; want 5, 5, set",
			dead["attempts"], len(dead["delivery_attempts"].([]any)), dead["last_error"])
	}

	setFailureRate(t, "0")
	r := call(t, http.MethodPost, "/v1/notifications/"+id+"/retry", nil)
	mustStatus(t, r, http.StatusOK, "manual retry")
	if b := r.obj(t); b["status"] != "pending" || b["max_attempts"].(float64) != 10 {
		t.Errorf("after retry = %v, want pending with max_attempts 10", b)
	}
	eventually(t, time.Minute, "the retried notification to be sent", func() bool { return notification(t, id)["status"] == "sent" })
	if a := notification(t, id)["attempts"].(float64); a != 6 {
		t.Errorf("attempts after manual retry = %v, want 6 (attempt numbers continue)", a)
	}
	mustStatus(t, call(t, http.MethodPost, "/v1/notifications/"+id+"/retry", nil), http.StatusConflict, "retry a sent notification")
}

// setFailureRate restarts the mock provider with FAILURE_RATE (empty: the compose default).
func setFailureRate(t *testing.T, rate string) {
	t.Helper()
	cmd := exec.Command("docker", "compose", "up", "-d", "--wait", "mockprovider")
	cmd.Dir = "../.."
	cmd.Env = os.Environ()
	if rate != "" {
		cmd.Env = append(cmd.Env, "FAILURE_RATE="+rate)
	} else {
		cmd.Env = append(cmd.Env, "FAILURE_RATE=")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restarting the mock provider with FAILURE_RATE=%q: %v\n%s", rate, err, out)
	}
}
