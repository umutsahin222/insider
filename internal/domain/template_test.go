package domain

import (
	"strings"
	"testing"
)

func mustTemplate(t *testing.T, name, channel, body string) Template {
	t.Helper()
	tmpl, err := NewTemplate(name, channel, body, now)
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

func TestNewTemplate(t *testing.T) {
	if _, err := NewTemplate("otp", "sms", "Your code is {{.code}}", now); err != nil {
		t.Fatalf("valid template rejected: %v", err)
	}
	tests := []struct {
		name, tname, channel, body string
		wantFields                 []string
	}{
		{"bad name", "OTP Code", "sms", "x", []string{"name"}},
		{"bad channel", "otp", "fax", "x", []string{"channel"}},
		{"empty body", "otp", "sms", "  ", []string{"body"}},
		{"unparsable body", "otp", "sms", "Hi {{.name", []string{"body"}},
		{"too long", "otp", "email", strings.Repeat("x", maxTemplateBody+1), []string{"body"}},
		{"everything wrong", "", "", "", []string{"name", "channel", "body"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewTemplate(tt.tname, tt.channel, tt.body, now)
			if got := fields(t, err); strings.Join(got, ",") != strings.Join(tt.wantFields, ",") {
				t.Errorf("fields = %v, want %v", got, tt.wantFields)
			}
		})
	}
}

func TestTemplateRender(t *testing.T) {
	tmpl := mustTemplate(t, "otp", "sms", "Hi {{.name}}, your code is {{.code}}")

	got, err := tmpl.Render(map[string]string{"name": "Ada", "code": "1234", "unused": "ok"})
	if err != nil || got != "Hi Ada, your code is 1234" {
		t.Errorf("Render = %q, %v", got, err)
	}
	if _, err := tmpl.Render(map[string]string{"name": "Ada"}); err == nil || err.Error() != `missing variable "code"` {
		t.Errorf("missing variable: err = %v", err)
	}
	if _, err := tmpl.Render(nil); err == nil {
		t.Error("nil variables must report the missing ones")
	}

	// Output is bounded before content validation ever sees it.
	// A self-recursive template would otherwise build a huge string before text/template's
	// depth limit stops it.
	bomb := mustTemplate(t, "bomb", "email", `{{define "a"}}{{.x}}{{template "a" .}}{{end}}{{template "a" .}}`)
	if _, err := bomb.Render(map[string]string{"x": strings.Repeat("a", 1000)}); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized render: err = %v", err)
	}

	// Error messages are returned to clients: they must not echo unbounded input back.
	bad := mustTemplate(t, "bad", "email", `{{range .x}}{{end}}`) // "range can't iterate over <value>"
	_, err = bad.Render(map[string]string{"x": strings.Repeat("a", 10_000)})
	if err == nil {
		t.Fatal("ranging over a string must fail")
	}
	if len(err.Error()) > maxRenderError+40 {
		t.Errorf("error = %d bytes, want it truncated", len(err.Error()))
	}
}

func TestNewNotification_FromTemplate(t *testing.T) {
	otp := mustTemplate(t, "otp", "sms", "Your code is {{.code}}")
	push := mustTemplate(t, "promo", "push", "Sale!")
	long := mustTemplate(t, "long", "sms", "{{.text}}")

	ref := func(tmpl *Template, vars map[string]string) *TemplateRef {
		r := &TemplateRef{Variables: vars, Resolved: tmpl}
		if tmpl != nil {
			r.Name = tmpl.Name
		} else {
			r.Name = "nope"
		}
		return r
	}
	sms := func(r *TemplateRef, content string) CreateInput {
		return CreateInput{Channel: "sms", Recipient: "+905551234567", Content: content, Template: r}
	}

	n, err := NewNotification(sms(ref(&otp, map[string]string{"code": "4242"}), ""), DefaultRules(), now)
	if err != nil {
		t.Fatal(err)
	}
	if n.Content != "Your code is 4242" || n.TemplateID == nil || *n.TemplateID != otp.ID {
		t.Errorf("content = %q template = %v", n.Content, n.TemplateID)
	}

	tests := []struct {
		name       string
		in         CreateInput
		wantFields []string
	}{
		{"unknown template", sms(ref(nil, nil), ""), []string{"template.name"}},
		{"wrong channel", sms(ref(&push, nil), ""), []string{"template.name"}},
		{"missing variable", sms(ref(&otp, nil), ""), []string{"template.variables"}},
		{"content and template", sms(ref(&otp, map[string]string{"code": "1"}), "literal"), []string{"content"}},
		{"rendered content over the sms limit", sms(ref(&long, map[string]string{"text": strings.Repeat("x", 161)}), ""), []string{"content"}},
		{"empty name", sms(&TemplateRef{}, ""), []string{"template.name"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewNotification(tt.in, DefaultRules(), now)
			if got := fields(t, err); strings.Join(got, ",") != strings.Join(tt.wantFields, ",") {
				t.Errorf("fields = %v, want %v", got, tt.wantFields)
			}
		})
	}

	t.Run("same template and variables fingerprint the same", func(t *testing.T) {
		a, _ := NewNotification(sms(ref(&otp, map[string]string{"code": "1"}), ""), DefaultRules(), now)
		b, _ := NewNotification(sms(ref(&otp, map[string]string{"code": "1"}), ""), DefaultRules(), now)
		c, _ := NewNotification(sms(ref(&otp, map[string]string{"code": "2"}), ""), DefaultRules(), now)
		if a.RequestHash != b.RequestHash || a.RequestHash == c.RequestHash {
			t.Error("fingerprint must follow the rendered content")
		}
	})
}
