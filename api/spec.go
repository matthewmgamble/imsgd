// Package api holds the imsgd OpenAPI specification, the source of truth
// for the HTTP API. Handlers in internal/api implement it and contract
// tests validate responses against it.
package api

import _ "embed"

//go:embed openapi.yaml
var Spec []byte
