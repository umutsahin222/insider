package httpapi

import "net/http"

// swaggerUIVersion pins the Swagger UI assets the /docs page loads from a CDN. They are only
// fetched by the browser viewing the page; the API itself serves just the spec.
const swaggerUIVersion = "5.17.14"

const docsPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Notification API</title>
<link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@` + swaggerUIVersion + `/swagger-ui.css">
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@` + swaggerUIVersion + `/swagger-ui-bundle.js" crossorigin></script>
<script>
window.onload = function () {
  window.ui = SwaggerUIBundle({ url: "/openapi.yaml", dom_id: "#swagger-ui" });
};
</script>
</body>
</html>
`

// serveOpenAPI returns the embedded contract (api/openapi.yaml).
func serveOpenAPI(spec []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(spec)
	}
}

// serveDocs renders the contract with Swagger UI.
func serveDocs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(docsPage))
}
