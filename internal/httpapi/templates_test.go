package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestTemplates_CreateAndList(t *testing.T) {
	h := newTestServer(t, newFakeStore(), nil)

	rec := do(t, h, http.MethodPost, "/v1/templates", `{"name":"otp","channel":"sms","body":"Your code is {{.code}}"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if tmpl := decode[templateResponse](t, rec); tmpl.Name != "otp" || tmpl.Channel != "sms" {
		t.Errorf("created = %+v", tmpl)
	}
	if rec := do(t, h, http.MethodPost, "/v1/templates", `{"name":"otp","channel":"sms","body":"again"}`); rec.Code != http.StatusConflict {
		t.Errorf("duplicate: status = %d, want 409", rec.Code)
	}
	rec = do(t, h, http.MethodPost, "/v1/templates", `{"name":"Bad Name","channel":"sms","body":"{{.x"}`)
	if rec.Code != http.StatusUnprocessableEntity || len(decode[problem](t, rec).Errors) != 2 {
		t.Errorf("invalid: status = %d body = %s", rec.Code, rec.Body)
	}

	list := decode[struct {
		Items []templateResponse `json:"items"`
	}](t, do(t, h, http.MethodGet, "/v1/templates", ""))
	if len(list.Items) != 1 {
		t.Errorf("list = %+v", list.Items)
	}
}

func TestCreateNotification_FromTemplate(t *testing.T) {
	store := newFakeStore()
	h := newTestServer(t, store, nil)
	do(t, h, http.MethodPost, "/v1/templates", `{"name":"otp","channel":"sms","body":"Your code is {{.code}}"}`)

	rec := do(t, h, http.MethodPost, "/v1/notifications",
		`{"channel":"sms","recipient":"+905551234567","template":{"name":"otp","variables":{"code":"4242"}}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	n := decode[notificationResponse](t, rec)
	if n.Content != "Your code is 4242" || n.TemplateID == nil {
		t.Errorf("content = %q template_id = %v", n.Content, n.TemplateID)
	}

	rec = do(t, h, http.MethodPost, "/v1/notifications",
		`{"channel":"sms","recipient":"+905551234567","template":{"name":"otp"}}`)
	if p := decode[problem](t, rec); rec.Code != http.StatusUnprocessableEntity || len(p.Errors) != 1 ||
		p.Errors[0].Field != "template.variables" || !strings.Contains(p.Errors[0].Message, `"code"`) {
		t.Errorf("missing variable: %d %+v", rec.Code, p)
	}
}

func TestCreateBatch_TemplatesResolvedOnceWithIndexedErrors(t *testing.T) {
	store := newFakeStore()
	h := newTestServer(t, store, nil)
	do(t, h, http.MethodPost, "/v1/templates", `{"name":"otp","channel":"sms","body":"Code {{.code}}"}`)
	store.templateLookups = 0

	item := func(name, code string) string {
		return `{"channel":"sms","recipient":"+905551234567","template":{"name":"` + name + `","variables":{"code":"` + code + `"}}}`
	}
	ok := `{"items":[` + item("otp", "1") + `,` + item("otp", "2") + `,` + item("otp", "3") + `]}`
	if rec := do(t, h, http.MethodPost, "/v1/notifications/batch", ok); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if store.templateLookups != 1 {
		t.Errorf("template lookups = %d, want 1 for the whole batch", store.templateLookups)
	}

	bad := `{"items":[` + item("otp", "1") + `,` + item("missing", "2") + `]}`
	rec := do(t, h, http.MethodPost, "/v1/notifications/batch", bad)
	if p := decode[problem](t, rec); rec.Code != http.StatusUnprocessableEntity || len(p.Errors) != 1 ||
		p.Errors[0].Field != "items[1].template.name" {
		t.Errorf("unknown template in batch: %d %+v", rec.Code, p)
	}
}
