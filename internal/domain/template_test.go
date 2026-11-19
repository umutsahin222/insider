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
		// Constructs that could make rendering unbounded work are rejected at creation.
		{"range over int", "otp", "sms", "{{range 100000}}{{range 100000}}{{end}}{{end}}x", []string{"body"}},
		{"range over variables", "otp", "sms", "{{range .}}{{.}}{{end}}", []string{"body"}},
		{"recursion", "otp", "sms", `{{define "a"}}{{template "a" .}}{{end}}{{template "a" .}}`, []string{"body"}},
		{"block", "otp", "sms", `{{block "a" .}}x{{end}}`, []string{"body"}},
		{"printf width", "otp", "sms", `{{printf "%099999999d" 1}}`, []string{"body"}},
		{"print", "otp", "sms", `{{print .a .a .a}}`, []string{"body"}},
		{"call", "otp", "sms", `{{call .f}}`, []string{"body"}},
		{"disallowed func in if", "otp", "sms", `{{if slice .a 1}}x{{end}}`, []string{"body"}},
		{"disallowed func nested", "otp", "sms", `{{eq (html .a) "x"}}`, []string{"body"}},
		{"disallowed func in else", "otp", "sms", `{{if .a}}x{{else}}{{js .a}}{{end}}`, []string{"body"}},
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

func TestNewTemplate_SupportedSubset(t *testing.T) {
	tmpl := mustTemplate(t, "subset", "email",
		`{{/* greeting */}}{{$n := .name}}Hi {{$n}}`+
			`{{if eq .tier "gold"}}, thanks for being gold{{else if .tier}}, tier {{.tier}}{{end}}`+
			`{{with .coupon}} (code {{.}}){{end}}`+
			`{{if and (gt (len .name) 2) (not (eq .name "Bob"))}}!{{end}} {{index . "name"}}`)
	got, err := tmpl.Render(map[string]string{"name": "Ada", "tier": "gold", "coupon": "X1"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "Hi Ada, thanks for being gold (code X1)! Ada"; got != want {
		t.Errorf("Render = %q, want %q", got, want)
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

	// Output and variables are bounded before content validation ever sees them.
	many := mustTemplate(t, "many", "email", strings.Repeat("{{.x}}", 100))
	if _, err := many.Render(map[string]string{"x": strings.Repeat("a", 1000)}); err == nil ||
		!strings.Contains(err.Error(), "rendered content exceeds") {
		t.Errorf("oversized render: err = %v", err)
	}
	if _, err := many.Render(map[string]string{"x": strings.Repeat("a", maxVariableBytes)}); err == nil ||
		!strings.Contains(err.Error(), "variables exceed") {
		t.Errorf("oversized variables: err = %v", err)
	}

	// Error messages are returned to clients: they must not echo unbounded input back.
	bad := mustTemplate(t, "bad", "email", `{{index .x 99999}}`) // "can't index item of type string"
	_, err = bad.Render(map[string]string{"x": strings.Repeat("a", 10_000)})
	if err == nil {
		t.Fatal("indexing a string variable must fail")
	}
	if len(err.Error()) > maxRenderError+40 {
		t.Errorf("error = %d bytes, want it truncated", len(err.Error()))
	}

	// A stored template that predates a subset change still cannot run unsupported constructs.
	legacy := Template{Name: "legacy", Channel: ChannelSMS, Body: "{{range 3}}x{{end}}"}
	if _, err := legacy.Render(nil); err == nil || !strings.Contains(err.Error(), "range is not supported") {
		t.Errorf("legacy range: err = %v", err)
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
