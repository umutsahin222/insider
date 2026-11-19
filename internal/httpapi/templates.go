package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/umutsahin222/insider/internal/domain"
)

type templateRefRequest struct {
	Name      string            `json:"name"`
	Variables map[string]string `json:"variables"`
}

type createTemplateRequest struct {
	Name    string `json:"name"`
	Channel string `json:"channel"`
	Body    string `json:"body"`
}

type templateResponse struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Channel   string    `json:"channel"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

func toTemplateResponse(t domain.Template) templateResponse {
	return templateResponse{ID: t.ID, Name: t.Name, Channel: string(t.Channel), Body: t.Body, CreatedAt: t.CreatedAt.UTC()}
}

func (a *api) createTemplate(w http.ResponseWriter, r *http.Request) {
	var req createTemplateRequest
	if err := decodeJSON(r, &req); err != nil {
		a.writeError(w, r, err)
		return
	}
	t, err := domain.NewTemplate(req.Name, req.Channel, req.Body, a.now())
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	created, err := a.store.CreateTemplate(r.Context(), t)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.log.InfoContext(r.Context(), "template created", "template", created.Name, "channel", created.Channel)
	writeJSON(w, http.StatusCreated, toTemplateResponse(created))
}

func (a *api) listTemplates(w http.ResponseWriter, r *http.Request) {
	templates, err := a.store.ListTemplates(r.Context())
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	items := make([]templateResponse, len(templates))
	for i, t := range templates {
		items[i] = toTemplateResponse(t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// resolveTemplates looks up every template the inputs reference — one query for a whole
// batch — and attaches it. Unknown names stay unresolved; the domain reports them as
// validation errors together with every other problem in the request.
func (a *api) resolveTemplates(ctx context.Context, inputs []domain.CreateInput) error {
	seen := map[string]bool{}
	var names []string
	for _, in := range inputs {
		if in.Template != nil && in.Template.Name != "" && !seen[in.Template.Name] {
			seen[in.Template.Name] = true
			names = append(names, in.Template.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	found, err := a.store.TemplatesByName(ctx, names)
	if err != nil {
		return err
	}
	// One *Template per name, so the domain parses each template once per request.
	shared := make(map[string]*domain.Template, len(found))
	for name, t := range found {
		shared[name] = &t
	}
	for i := range inputs {
		if ref := inputs[i].Template; ref != nil {
			ref.Resolved = shared[ref.Name] // nil if there is no such template
		}
	}
	return nil
}
