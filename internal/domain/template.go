package domain

import (
	"errors"
	"regexp"
	"strings"
	"text/template"
	"time"
)

var (
	templateName = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
	missingKey   = regexp.MustCompile(`map has no entry for key "([^"]*)"`)
)

const (
	maxTemplateBody = 20_000
	// maxRenderedBytes bounds rendering output before content validation sees it, so a
	// template like {{range}}…{{end}} cannot make the API build arbitrarily large strings.
	maxRenderedBytes = 64 << 10
	// maxRenderError caps rendering error messages, which can quote variable values.
	maxRenderError = 200
)

// TemplateRef selects a template by name in a create request. Resolved is filled in by the
// caller (the API looks templates up in the store) and is nil if no such template exists.
type TemplateRef struct {
	Name      string
	Variables map[string]string
	Resolved  *Template
}

// NewTemplate validates a template definition. Templates are immutable once created: the
// rendered content and template ID of existing notifications must keep meaning the same.
func NewTemplate(name, channel, body string, now time.Time) (Template, error) {
	ve := &ValidationError{}
	if !templateName.MatchString(name) {
		ve.add("name", "must be 1-64 characters of a-z, 0-9, '_' or '-'")
	}
	ch, err := ParseChannel(channel)
	if err != nil {
		ve.add("channel", "must be one of sms, email, push")
	}
	switch {
	case strings.TrimSpace(body) == "":
		ve.add("body", "is required")
	case len(body) > maxTemplateBody:
		ve.add("body", "must be at most "+itoa(maxTemplateBody)+" bytes")
	default:
		if _, err := parseTemplate(name, body); err != nil {
			ve.add("body", "is not a valid template: "+err.Error())
		}
	}
	if err := ve.errOrNil(); err != nil {
		return Template{}, err
	}
	return Template{ID: newID(), Name: name, Channel: ch, Body: body, CreatedAt: now}, nil
}

// Render executes the template with vars. Every placeholder must have a variable.
func (t Template) Render(vars map[string]string) (string, error) {
	tmpl, err := parseTemplate(t.Name, t.Body)
	if err != nil {
		return "", err
	}
	if vars == nil {
		vars = map[string]string{}
	}
	var out limitedBuilder
	if err := tmpl.Execute(&out, vars); err != nil {
		if m := missingKey.FindStringSubmatch(err.Error()); m != nil {
			return "", errors.New(`missing variable "` + m[1] + `"`)
		}
		if errors.Is(err, errRenderTooLarge) {
			return "", err
		}
		msg := err.Error()
		if len(msg) > maxRenderError {
			msg = msg[:maxRenderError] + "…"
		}
		return "", errors.New("cannot be rendered: " + msg)
	}
	return out.String(), nil
}

func parseTemplate(name, body string) (*template.Template, error) {
	return template.New(name).Option("missingkey=error").Parse(body)
}

var errRenderTooLarge = errors.New("rendered content exceeds " + itoa(maxRenderedBytes) + " bytes")

// limitedBuilder is a strings.Builder that refuses to grow past maxRenderedBytes.
type limitedBuilder struct{ strings.Builder }

func (b *limitedBuilder) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxRenderedBytes {
		return 0, errRenderTooLarge
	}
	return b.Builder.Write(p)
}

// applyTemplate renders in.Template into n.Content, reporting problems under prefix.
// It reports whether content is now available for validation.
func applyTemplate(in CreateInput, ch Channel, n *Notification, prefix string, ve *ValidationError) bool {
	ref := in.Template
	if in.Content != "" {
		ve.add(prefix+"content", "must be empty when a template is used")
	}
	switch {
	case ref.Name == "":
		ve.add(prefix+"template.name", "is required")
		return false
	case ref.Resolved == nil:
		ve.add(prefix+"template.name", `no template named "`+ref.Name+`"`)
		return false
	case ref.Resolved.Channel != ch:
		ve.add(prefix+"template.name", "is a "+string(ref.Resolved.Channel)+" template; cannot be used for "+string(ch))
		return false
	}
	content, err := ref.Resolved.Render(ref.Variables)
	if err != nil {
		ve.add(prefix+"template.variables", err.Error())
		return false
	}
	id := ref.Resolved.ID
	n.Content, n.TemplateID = content, &id
	return true
}
