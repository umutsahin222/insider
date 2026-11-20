//go:build acceptance

package acceptance

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// REQ-ENG-4: content validation (character limits, required fields).
func TestREQ_ENG4_ContentValidation(t *testing.T) {
	r := call(t, http.MethodPost, "/v1/notifications", map[string]any{})
	mustStatus(t, r, http.StatusUnprocessableEntity, "empty request")
	if f := r.fields(t); !slices.Contains(f, "channel") || !slices.Contains(f, "recipient") || !slices.Contains(f, "content") {
		t.Errorf("empty request reported %v, want channel, recipient and content at once", f)
	}

	phone := "+905551234567"
	cases := []struct {
		name  string
		body  map[string]any
		want  int
		field string
	}{
		{"sms at 160 chars", map[string]any{"channel": "sms", "recipient": phone, "content": strings.Repeat("a", 160)}, 201, ""},
		{"sms over 160 chars", map[string]any{"channel": "sms", "recipient": phone, "content": strings.Repeat("a", 161)}, 422, "content"},
		{"limit counts characters, not bytes", map[string]any{"channel": "sms", "recipient": phone, "content": strings.Repeat("ş", 160)}, 201, ""},
		{"multi-byte over limit", map[string]any{"channel": "sms", "recipient": phone, "content": strings.Repeat("ğ", 161)}, 422, "content"},
		{"email at 10000 chars", map[string]any{"channel": "email", "recipient": "a@example.com", "content": strings.Repeat("x", 10000)}, 201, ""},
		{"email over 10000 chars", map[string]any{"channel": "email", "recipient": "a@example.com", "content": strings.Repeat("x", 10001)}, 422, "content"},
		{"push at 256 chars", map[string]any{"channel": "push", "recipient": "tok", "content": strings.Repeat("p", 256)}, 201, ""},
		{"push over 256 chars", map[string]any{"channel": "push", "recipient": "tok", "content": strings.Repeat("p", 257)}, 422, "content"},
		{"whitespace-only content", map[string]any{"channel": "sms", "recipient": phone, "content": "   "}, 422, "content"},
		{"sms recipient not E.164", map[string]any{"channel": "sms", "recipient": "05551234567", "content": "x"}, 422, "recipient"},
		{"invalid email", map[string]any{"channel": "email", "recipient": "not-an-email", "content": "x"}, 422, "recipient"},
		{"push token with space", map[string]any{"channel": "push", "recipient": "a b", "content": "x"}, 422, "recipient"},
		{"unknown channel", map[string]any{"channel": "fax", "recipient": "x", "content": "x"}, 422, "channel"},
		{"unknown priority", map[string]any{"channel": "sms", "recipient": phone, "content": "x", "priority": "urgent"}, 422, "priority"},
		{"unknown field", map[string]any{"channel": "sms", "recipient": phone, "content": "x", "foo": 1}, 400, ""},
	}
	for _, c := range cases {
		r := call(t, http.MethodPost, "/v1/notifications", c.body)
		if r.Status != c.want || (c.field != "" && !slices.Equal(r.fields(t), []string{c.field})) {
			t.Errorf("%s: %d %s; want %d on %q", c.name, r.Status, r.Body, c.want, c.field)
		}
	}
	mustStatus(t, call(t, http.MethodPost, "/v1/notifications", []byte(`{"channel": "sms",`)), http.StatusBadRequest, "malformed JSON")
}

// REQ-ENG-5: idempotency support to prevent duplicate sends.
func TestREQ_ENG5_Idempotency(t *testing.T) {
	key := run + "-key"
	body := map[string]any{"channel": "sms", "recipient": "+905551234567", "content": run + " idem"}
	first := call(t, http.MethodPost, "/v1/notifications", body, "Idempotency-Key", key)
	mustStatus(t, first, http.StatusCreated, "first create")
	again := call(t, http.MethodPost, "/v1/notifications", body, "Idempotency-Key", key)
	mustStatus(t, again, http.StatusOK, "replay")
	if again.obj(t)["id"] != first.obj(t)["id"] || again.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay returned %s (header %q), want the original notification", again.Body, again.Header.Get("Idempotent-Replayed"))
	}
	body["content"] = "different"
	mustStatus(t, call(t, http.MethodPost, "/v1/notifications", body, "Idempotency-Key", key), http.StatusUnprocessableEntity, "key reused with another body")

	items := []map[string]any{sms(1, run+" b1"), sms(2, run+" b2")}
	id, ids := batch(t, items, "Idempotency-Key", key+"-batch")
	r := call(t, http.MethodPost, "/v1/notifications/batch", map[string]any{"items": items}, "Idempotency-Key", key+"-batch")
	mustStatus(t, r, http.StatusOK, "batch replay")
	b := r.obj(t)
	if b["batch_id"] != id || fmt.Sprint(b["notification_ids"]) != fmt.Sprint(toAny(ids)) {
		t.Errorf("batch replay = %v, want batch %s with the same ids", b, id)
	}
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// REQ-ENG-1 and REQ-ENG-2: asynchronous queue workers deliver everything, and no channel ever
// exceeds 100 messages per second, measured where it matters: at the provider. Two channels run
// at the same time to show that each has its own limit.
func TestREQ_ENG2_RateLimit100PerSecondPerChannel(t *testing.T) {
	// Enqueued back to back (~70 ms per batch), so both channels drain at the same time.
	batches := map[string]string{}
	for _, ch := range []string{"sms", "email"} {
		items := make([]map[string]any, 1000)
		for i := range items {
			if ch == "sms" {
				items[i] = sms(i, run+" rate")
			} else {
				items[i] = map[string]any{"channel": "email", "recipient": fmt.Sprintf("u%d@example.com", i), "content": run + " rate"}
			}
		}
		batches[ch], _ = batch(t, items)
	}
	start := time.Now()
	for ch, id := range batches {
		counts := waitBatchSettled(t, id, 3*time.Minute)
		t.Logf("%s: 1000 settled (%v) %s after enqueueing", ch, counts, time.Since(start).Round(100*time.Millisecond))
		// With injected 10 % failures and five attempts, a dead letter is possible but rare.
		if counts["sent"] < 995 {
			t.Errorf("%s: only %d of 1000 sent: %v", ch, counts["sent"], counts)
		}
	}

	stats := call(t, http.MethodGet, mockURL+"/stats", nil)
	mustStatus(t, stats, http.StatusOK, "mock provider stats (is the mock the provider?)")
	peaks := stats.obj(t)["peak_per_second"].(map[string]any)
	for _, ch := range []string{"sms", "email"} {
		peak, _ := peaks[ch].(float64)
		t.Logf("%s: busiest second at the provider: %v requests", ch, peak)
		if peak == 0 || peak > 100 {
			t.Errorf("%s: busiest second at the provider = %v, want 1..100", ch, peak)
		}
	}
}

// REQ-ENG-3: priority queue support. High-priority notifications queued after a backlog of
// low-priority ones get their first attempt before most of it.
func TestREQ_ENG3_PriorityQueue(t *testing.T) {
	low := make([]map[string]any, 400)
	for i := range low {
		low[i] = sms(i, run+" low")
		low[i]["priority"] = "low"
	}
	high := make([]map[string]any, 50)
	for i := range high {
		high[i] = sms(i, run+" high")
		high[i]["priority"] = "high"
	}
	lowID, lowIDs := batch(t, low)
	highID, highIDs := batch(t, high)
	waitBatchSettled(t, lowID, 2*time.Minute)
	waitBatchSettled(t, highID, time.Minute)

	type first struct {
		at   time.Time
		high bool
	}
	var firsts []first
	for i, id := range append(slices.Clone(lowIDs), highIDs...) {
		for _, a := range notification(t, id)["delivery_attempts"].([]any) {
			m := a.(map[string]any)
			if m["attempt"].(float64) == 1 {
				at, _ := time.Parse(time.RFC3339Nano, m["created_at"].(string))
				firsts = append(firsts, first{at: at, high: i >= len(lowIDs)})
			}
		}
	}
	sort.Slice(firsts, func(i, j int) bool { return firsts[i].at.Before(firsts[j].at) })
	var highSum, lowSum, highN, lowN, lastHigh int
	for pos, f := range firsts {
		if f.high {
			highSum, highN, lastHigh = highSum+pos+1, highN+1, pos+1
		} else {
			lowSum, lowN = lowSum+pos+1, lowN+1
		}
	}
	avgHigh, avgLow := float64(highSum)/float64(highN), float64(lowSum)/float64(lowN)
	t.Logf("first attempts: 50 high at average position %.0f (last %d), 400 earlier-queued low at %.0f", avgHigh, lastHigh, avgLow)
	// Lows already claimed when the highs arrive (8 loops x 10 rows per worker) may go first.
	if avgHigh >= avgLow || lastHigh > 140 {
		t.Errorf("priority not honoured: high average %.0f (last %d) vs low average %.0f", avgHigh, lastHigh, avgLow)
	}
}
