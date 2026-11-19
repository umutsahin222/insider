package domain

import (
	"errors"
	"regexp"
	"strings"
	"text/template"
	"text/template/parse"
	"time"
)

var (
	templateName = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
	missingKey   = regexp.MustCompile(`map has no entry for key "([^"]*)"`)
)

const (
	maxTemplateBody = 20_000
	// maxRenderedBytes bounds rendering output before content validation sees it.
	maxRenderedBytes = 64 << 10
	// maxVariableBytes bounds the variables of one render (names and values together), so
	// every allowed action works on a bounded input.
	maxVariableBytes = 64 << 10
	// maxRenderError caps rendering error messages, which can quote variable values.
	maxRenderError = 200
)

// allowedFuncs are the template functions a body may call: pure, and linear in their
// arguments. Everything else text/template offers is rejected at creation (see checkSubset).
var allowedFuncs = map[string]bool{
	"eq": true, "ne": true, "lt": true, "le": true, "gt": true, "ge": true,
	"and": true, "or": true, "not": true, "len": true, "index": true,
}

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
// The parsed body is cached on t; Render is not safe for concurrent use of one *Template.
func (t *Template) Render(vars map[string]string) (string, error) {
	if t.parsed == nil {
		tmpl, err := parseTemplate(t.Name, t.Body)
		if err != nil {
			return "", err
		}
		t.parsed = tmpl
	}
	return render(t.parsed, vars)
}

func render(tmpl *template.Template, vars map[string]string) (string, error) {
	size := 0
	for k, v := range vars {
		size += len(k) + len(v)
	}
	if size > maxVariableBytes {
		return "", errors.New("variables exceed " + itoa(maxVariableBytes) + " bytes")
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

// parseTemplate parses body and checks it stays within the supported subset.
func parseTemplate(name, body string) (*template.Template, error) {
	tmpl, err := template.New(name).Option("missingkey=error").Parse(body)
	if err != nil {
		return nil, err
	}
	if len(tmpl.Templates()) > 1 {
		return nil, errors.New("define, block and template are not supported")
	}
	if err := checkSubset(tmpl.Root); err != nil {
		return nil, err
	}
	return tmpl, nil
}

// checkSubset accepts variable substitution with light logic: {{.name}}, variables, literals,
// if/else, with, and the functions in allowedFuncs. It rejects what could make rendering
// cost unbounded work on the API: range (loops over integers or nested loops over the
// variables), define/template (recursion), break/continue, and functions that build large
// values from small inputs (printf "%099999999d", print, call, slice, html, js, urlquery).
// With this subset every node runs at most once, so rendering is linear in template and
// variable size.
func checkSubset(n parse.Node) error {
	switch n := n.(type) {
	case nil:
		return nil
	case *parse.ListNode:
		if n == nil {
			return nil
		}
		for _, c := range n.Nodes {
			if err := checkSubset(c); err != nil {
				return err
			}
		}
		return nil
	case *parse.TextNode, *parse.CommentNode, *parse.FieldNode, *parse.VariableNode,
		*parse.DotNode, *parse.StringNode, *parse.NumberNode, *parse.BoolNode, *parse.NilNode:
		return nil
	case *parse.ActionNode:
		return checkSubset(n.Pipe)
	case *parse.IfNode:
		return checkBranch(&n.BranchNode)
	case *parse.WithNode:
		return checkBranch(&n.BranchNode)
	case *parse.PipeNode:
		if n == nil {
			return nil
		}
		for _, cmd := range n.Cmds {
			for _, arg := range cmd.Args {
				if err := checkSubset(arg); err != nil {
					return err
				}
			}
		}
		return nil
	case *parse.ChainNode:
		return checkSubset(n.Node)
	case *parse.IdentifierNode:
		if !allowedFuncs[n.Ident] {
			return errors.New("function " + n.Ident + " is not supported")
		}
		return nil
	case *parse.RangeNode:
		return errors.New("range is not supported")
	case *parse.TemplateNode:
		return errors.New("define, block and template are not supported")
	case *parse.BreakNode, *parse.ContinueNode:
		return errors.New("break and continue are not supported")
	default:
		return errors.New("unsupported template construct")
	}
}

func checkBranch(b *parse.BranchNode) error {
	if err := checkSubset(b.Pipe); err != nil {
		return err
	}
	if err := checkSubset(b.List); err != nil {
		return err
	}
	return checkSubset(b.ElseList)
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
