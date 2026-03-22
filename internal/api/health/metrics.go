package health

import (
	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
)

// Metrics returns a Fiber handler that serves Prometheus metrics in the standard
// text exposition format. The default Prometheus registry is used, which
// automatically includes Go runtime metrics (goroutines, memory, GC pause times).
func Metrics() fiber.Handler {
	handler := fasthttpadaptor.NewFastHTTPHandler(promhttp.Handler())
	return func(c fiber.Ctx) error {
		handler(c.RequestCtx())
		return nil
	}
}
