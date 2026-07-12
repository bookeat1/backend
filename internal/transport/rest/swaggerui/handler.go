// Package swaggerui serves the interactive Swagger UI and the raw OpenAPI spec.
// It is mounted only outside production so the API surface is never advertised
// to end users in a live environment.
package swaggerui

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"backend-core/docs"
)

// page is a minimal Swagger UI shell. The UI assets are loaded from the public
// unpkg CDN (this is a dev/staging-only page), and it renders the spec served
// at /docs/openapi.yaml.
const page = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>BookEat backend-core API — Swagger UI</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js" crossorigin></script>
  <script>
    window.ui = SwaggerUIBundle({
      url: "/docs/openapi.yaml",
      dom_id: "#swagger-ui",
      deepLinking: true
    });
  </script>
</body>
</html>`

// Register mounts the Swagger UI at /docs and the raw spec at
// /docs/openapi.yaml and /docs/openapi.json. It is a no-op when env is
// "production", so those routes are never registered there and requests to
// them fall through to a 404.
func Register(r gin.IRouter, env string) {
	if env == "production" {
		return
	}
	r.GET("/docs", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(page))
	})
	r.GET("/docs/openapi.yaml", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/yaml; charset=utf-8", docs.SwaggerYAML)
	})
	r.GET("/docs/openapi.json", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json; charset=utf-8", docs.SwaggerJSON)
	})
}
