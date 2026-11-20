package httpapi

import (
	"net/http"
	"strings"
	"testing"

	apispec "github.com/umutsahin222/insider/api"
)

func TestDocs_ServesTheEmbeddedContractAndSwaggerUI(t *testing.T) {
	deps := testDeps(newFakeStore(), nil)
	deps.OpenAPI = apispec.OpenAPI
	h := NewRouter(deps)

	spec := do(t, h, http.MethodGet, "/openapi.yaml", "")
	if spec.Code != http.StatusOK || spec.Header().Get("Content-Type") != "application/yaml" {
		t.Fatalf("GET /openapi.yaml = %d %q", spec.Code, spec.Header().Get("Content-Type"))
	}
	if spec.Body.String() != string(apispec.OpenAPI) || !strings.HasPrefix(spec.Body.String(), "openapi: 3.1") {
		t.Errorf("GET /openapi.yaml did not return api/openapi.yaml")
	}

	page := do(t, h, http.MethodGet, "/docs", "")
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.HasPrefix(page.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(body, "swagger-ui-dist@"+swaggerUIVersion) || !strings.Contains(body, `url: "/openapi.yaml"`) {
		t.Errorf("GET /docs = %d %q:\n%s", page.Code, page.Header().Get("Content-Type"), body)
	}
}

func TestDocs_NotServedWithoutASpec(t *testing.T) {
	h := NewRouter(testDeps(newFakeStore(), nil))
	for _, path := range []string{"/openapi.yaml", "/docs"} {
		if rec := do(t, h, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 when no spec is configured", path, rec.Code)
		}
	}
}
