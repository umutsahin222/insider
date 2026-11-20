// Package api embeds the OpenAPI contract so the API binary can serve it (GET /openapi.yaml,
// rendered at GET /docs). openapi.yaml stays the single source; nothing is generated.
package api

import _ "embed"

// OpenAPI is the contract in api/openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
