// Package apispec embeds api/openapi.yaml, the REST contract and source of truth.
package apispec

import _ "embed"

// Spec is the OpenAPI document served at GET /api/v1/openapi.yaml and used by contract tests.
//
//go:embed openapi.yaml
var Spec []byte
