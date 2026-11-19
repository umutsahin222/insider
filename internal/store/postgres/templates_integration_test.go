//go:build integration

package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/umutsahin222/insider/internal/domain"
)

func TestTemplates(t *testing.T) {
	ctx := context.Background()
	name := "otp-" + strings.ToLower(uuid.NewString()[:8])
	tmpl, err := domain.NewTemplate(name, "sms", "Your code is {{.code}}", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	created, err := testStore.CreateTemplate(ctx, tmpl)
	if err != nil || created.ID != tmpl.ID || created.Channel != domain.ChannelSMS || created.CreatedAt.IsZero() {
		t.Fatalf("create: %+v, %v", created, err)
	}
	dup, _ := domain.NewTemplate(name, "email", "other", time.Now())
	if _, err := testStore.CreateTemplate(ctx, dup); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("duplicate name: err = %v, want ErrConflict", err)
	}

	found, err := testStore.TemplatesByName(ctx, []string{name, "does-not-exist"})
	if err != nil || len(found) != 1 || found[name].Body != tmpl.Body {
		t.Fatalf("by name = %+v, %v", found, err)
	}

	all, err := testStore.ListTemplates(ctx)
	if err != nil || len(all) == 0 {
		t.Fatalf("list = %d, %v", len(all), err)
	}

	// A notification created from the template keeps the link and the rendered content.
	resolved := found[name]
	n, err := domain.NewNotification(domain.CreateInput{
		Channel: "sms", Recipient: "+905551234567",
		Template: &domain.TemplateRef{Name: name, Variables: map[string]string{"code": "77"}, Resolved: &resolved},
	}, domain.DefaultRules(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := testStore.CreateNotification(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Content != "Your code is 77" || stored.TemplateID == nil || *stored.TemplateID != tmpl.ID {
		t.Errorf("stored = content %q template %v", stored.Content, stored.TemplateID)
	}
}
