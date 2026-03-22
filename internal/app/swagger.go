package app

import (
	"github.com/gofiber/fiber/v3"

	"github.com/voidmind-io/voidllm/internal/docs"
)

// swaggerUIHTML is the Swagger UI page served at /api/docs. It loads the
// official Swagger UI distribution from the unpkg CDN and points it at the
// spec endpoint on the same origin so no hard-coded host is required.
const swaggerUIHTML = `<!DOCTYPE html>
<html>
<head>
    <title>VoidLLM API</title>
    <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
    <div id="swagger-ui"></div>
    <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
    <script>
        SwaggerUIBundle({
            url: '/api/docs/swagger.json',
            dom_id: '#swagger-ui',
            deepLinking: true,
            presets: [SwaggerUIBundle.presets.apis],
            layout: 'BaseLayout',
        })
    </script>
</body>
</html>`

// registerSwaggerHandlers mounts the Swagger JSON spec and the Swagger UI HTML
// page onto fiberApp. The spec is generated at startup from swaggo annotations;
// the UI is a thin HTML page that loads Swagger UI from the unpkg CDN.
func registerSwaggerHandlers(fiberApp *fiber.App) {
	fiberApp.Get("/api/docs/swagger.json", func(c fiber.Ctx) error {
		c.Set("Content-Type", "application/json")
		return c.SendString(docs.SwaggerInfo.ReadDoc())
	})
	fiberApp.Get("/api/docs", func(c fiber.Ctx) error {
		c.Set("Content-Type", "text/html; charset=utf-8")
		return c.SendString(swaggerUIHTML)
	})
}
