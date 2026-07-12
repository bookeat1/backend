// Package docs embeds the generated OpenAPI/Swagger spec (produced by
// `make swagger`) so the Swagger UI endpoints can serve it from the binary
// without reading from disk. Regenerate the spec with `make swagger`.
package docs

import _ "embed"

// SwaggerYAML is the OpenAPI/Swagger spec in YAML form.
//
//go:embed swagger.yaml
var SwaggerYAML []byte

// SwaggerJSON is the OpenAPI/Swagger spec in JSON form.
//
//go:embed swagger.json
var SwaggerJSON []byte
