//go:build acceptance

package acceptance

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// REQ-API-1: create notification requests with recipient, channel, content and priority.
func TestREQ_API1_CreateWithRecipientChannelContentPriority(t *testing.T) {
	for _, c := range []struct{ channel, recipient, priority string }{
		{"sms", "+905551234567", "high"},
		{"email", "ada@example.com", "normal"},
		{"push", "device-token-abc", "low"},
	} {
		n := create(t, map[string]any{"channel": c.channel, "recipient": c.recipient, "content": run + " hello", "priority": c.priority})
		if n["channel"] != c.channel || n["recipient"] != c.recipient || n["priority"] != c.priority ||
			n["content"] != run+" hello" || n["status"] != "pending" {
			t.Errorf("%s: created %v", c.channel, n)
		}
	}
	if n := create(t, map[string]any{"channel": "sms", "recipient": "+905551234567", "content": run + " default"}); n["priority"] != "normal" {
		t.Errorf("priority defaults to %v, want normal", n["priority"])
	}
}

// REQ-API-2: batch creation of up to 1000 notifications per request, all or nothing.
func TestREQ_API2_BatchUpTo1000(t *testing.T) {
	items := make([]map[string]any, 1000)
	for i := range items {
		items[i] = sms(i, fmt.Sprintf("%s batch %d", run, i))
		items[i]["scheduled_at"] = iso(time.Now().Add(time.Hour)) // keep the queue free for other tests
	}
	start := time.Now()
	id, ids := batch(t, items)
	t.Logf("1000-item batch accepted in %s", time.Since(start).Round(time.Millisecond))
	if len(ids) != 1000 {
		t.Fatalf("got %d ids, want 1000", len(ids))
	}
	t.Cleanup(func() { call(t, http.MethodPost, "/v1/batches/"+id+"/cancel", nil) })

	r := call(t, http.MethodPost, "/v1/notifications/batch", map[string]any{"items": append(items, items[0])})
	mustStatus(t, r, http.StatusUnprocessableEntity, "1001 items")

	r = call(t, http.MethodPost, "/v1/notifications/batch", map[string]any{"items": []any{}})
	mustStatus(t, r, http.StatusUnprocessableEntity, "empty batch")
}

func TestREQ_API2_BatchIsAllOrNothing(t *testing.T) {
	marker := run + "-atomic"
	items := make([]map[string]any, 10)
	for i := range items {
		items[i] = sms(i, marker)
	}
	items[5]["recipient"] = "not-a-phone"
	from := time.Now().Add(-2 * time.Second)
	r := call(t, http.MethodPost, "/v1/notifications/batch", map[string]any{"items": items})
	mustStatus(t, r, http.StatusUnprocessableEntity, "batch with an invalid item")
	if f := r.fields(t); !slices.Equal(f, []string{"items[5].recipient"}) {
		t.Errorf("fields = %v, want [items[5].recipient]", f)
	}
	// Nothing of the batch may have been stored.
	lr := call(t, http.MethodGet, "/v1/notifications?limit=200&from="+iso(from), nil)
	mustStatus(t, lr, http.StatusOK, "list recent notifications")
	for _, n := range lr.obj(t)["items"].([]any) {
		if n.(map[string]any)["content"] == marker {
			t.Fatal("an item of the rejected batch was stored")
		}
	}
}

// REQ-API-3: query notification status by ID or batch ID.
func TestREQ_API3_QueryByIDAndBatchID(t *testing.T) {
	id, ids := batch(t, []map[string]any{sms(1, run+" q1"), sms(2, run+" q2")})
	counts := waitBatchSettled(t, id, 60*time.Second)
	if counts["sent"]+counts["failed"] != 2 {
		t.Errorf("batch counts = %v", counts)
	}
	n := notification(t, ids[0])
	if n["status"] != "sent" || len(n["delivery_attempts"].([]any)) == 0 {
		t.Errorf("notification = %v, want sent with its delivery attempts", n)
	}
	mustStatus(t, call(t, http.MethodGet, "/v1/notifications/"+uuid.NewString(), nil), http.StatusNotFound, "unknown id")
	// A malformed ID cannot name any resource: 404, by design (internal/httpapi).
	mustStatus(t, call(t, http.MethodGet, "/v1/notifications/not-a-uuid", nil), http.StatusNotFound, "malformed id")
	mustStatus(t, call(t, http.MethodGet, "/v1/batches/"+uuid.NewString(), nil), http.StatusNotFound, "unknown batch")
}

// REQ-API-4: cancel pending notifications.
func TestREQ_API4_CancelPending(t *testing.T) {
	later := iso(time.Now().Add(time.Hour))
	n := create(t, map[string]any{"channel": "sms", "recipient": "+905551234567", "content": run + " cancel", "scheduled_at": later})
	r := call(t, http.MethodPost, "/v1/notifications/"+n["id"].(string)+"/cancel", nil)
	mustStatus(t, r, http.StatusOK, "cancel pending")
	if r.obj(t)["status"] != "cancelled" {
		t.Errorf("status after cancel = %v", r.obj(t)["status"])
	}
	mustStatus(t, call(t, http.MethodPost, "/v1/notifications/"+n["id"].(string)+"/cancel", nil), http.StatusConflict, "cancel twice")

	items := make([]map[string]any, 5)
	for i := range items {
		items[i] = map[string]any{"channel": "email", "recipient": fmt.Sprintf("c%d@example.com", i), "content": run + " cb", "scheduled_at": later}
	}
	id, _ := batch(t, items)
	mustStatus(t, call(t, http.MethodPost, "/v1/batches/"+id+"/cancel", nil), http.StatusOK, "cancel batch")
	if c := batchCounts(t, id); c["cancelled"] != 5 {
		t.Errorf("batch counts after cancel = %v", c)
	}

	sent := create(t, map[string]any{"channel": "push", "recipient": "tok-sent", "content": run + " sent"})
	eventually(t, 60*time.Second, "notification to be sent", func() bool { return notification(t, sent["id"].(string))["status"] == "sent" })
	mustStatus(t, call(t, http.MethodPost, "/v1/notifications/"+sent["id"].(string)+"/cancel", nil), http.StatusConflict, "cancel sent")
}

// REQ-API-5: list notifications with filtering (status, channel, date range) and pagination.
func TestREQ_API5_ListFilterAndPaginate(t *testing.T) {
	items := make([]map[string]any, 250)
	for i := range items {
		items[i] = sms(i, run+" page")
		items[i]["scheduled_at"] = iso(time.Now().Add(time.Hour))
	}
	id, ids := batch(t, items)
	t.Cleanup(func() { call(t, http.MethodPost, "/v1/batches/"+id+"/cancel", nil) })

	var got []string
	cursor, pages := "", 0
	for {
		r := call(t, http.MethodGet, "/v1/notifications?limit=100&batch_id="+id+cursor, nil)
		mustStatus(t, r, http.StatusOK, "list page")
		page := r.obj(t)
		for _, n := range page["items"].([]any) {
			got = append(got, n.(map[string]any)["id"].(string))
		}
		pages++
		next, _ := page["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = "&cursor=" + next
	}
	slices.Sort(got)
	want := slices.Clone(ids)
	slices.Sort(want)
	if pages != 3 || !slices.Equal(got, want) {
		t.Errorf("paginated %d pages, %d ids; want 3 pages covering all 250 exactly once", pages, len(got))
	}

	first := call(t, http.MethodGet, "/v1/notifications/"+ids[0], nil).obj(t)
	created, _ := time.Parse(time.RFC3339Nano, first["created_at"].(string))
	from, to := iso(created.Add(-time.Second)), iso(created.Add(time.Second))
	lr := call(t, http.MethodGet, "/v1/notifications?status=pending&channel=sms&limit=200&from="+from+"&to="+to, nil)
	mustStatus(t, lr, http.StatusOK, "filtered list")
	list := lr.obj(t)
	if len(list["items"].([]any)) == 0 {
		t.Fatal("filtered list is empty")
	}
	for _, x := range list["items"].([]any) {
		n := x.(map[string]any)
		c := n["created_at"].(string)
		if n["status"] != "pending" || n["channel"] != "sms" || c < from[:19] || c >= to {
			t.Fatalf("filter returned %v", n)
		}
	}

	r := call(t, http.MethodGet, "/v1/notifications?status=weird&channel=fax&limit=abc", nil)
	mustStatus(t, r, http.StatusBadRequest, "invalid filters")
	if f := strings.Join(r.fields(t), ","); !strings.Contains(f, "status") || !strings.Contains(f, "channel") || !strings.Contains(f, "limit") {
		t.Errorf("invalid filters reported %v, want status, channel and limit", f)
	}
	mustStatus(t, call(t, http.MethodGet, "/v1/notifications?cursor=garbage", nil), http.StatusBadRequest, "invalid cursor")
}
