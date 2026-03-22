// Package apierror provides a unified JSON error response format for all
// VoidLLM API surfaces (proxy, admin, and auth). Every error response
// produced by the server uses the envelope {"error":{"code":"...","message":"..."}},
// with an optional "request_id" field when a request ID middleware is active.
package apierror

import "github.com/gofiber/fiber/v3"

// Response is the standard JSON error envelope for all VoidLLM API responses.
type Response struct {
	Error Detail `json:"error"`
}

// Detail holds the machine-readable error code, a human-readable message,
// and an optional request correlation ID.
type Detail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// Send writes a JSON error response with the given HTTP status code.
// The request ID, if present in the Fiber context, is included automatically.
func Send(c fiber.Ctx, status int, code, message string) error {
	return c.Status(status).JSON(Response{
		Error: Detail{
			Code:      code,
			Message:   message,
			RequestID: RequestIDFromCtx(c),
		},
	})
}

// NotFound sends a 404 error response.
func NotFound(c fiber.Ctx, message string) error {
	return Send(c, fiber.StatusNotFound, "not_found", message)
}

// BadRequest sends a 400 error response.
func BadRequest(c fiber.Ctx, message string) error {
	return Send(c, fiber.StatusBadRequest, "bad_request", message)
}

// Conflict sends a 409 error response.
func Conflict(c fiber.Ctx, message string) error {
	return Send(c, fiber.StatusConflict, "conflict", message)
}

// InternalError sends a 500 error response.
func InternalError(c fiber.Ctx, message string) error {
	return Send(c, fiber.StatusInternalServerError, "internal_error", message)
}

// Unauthorized sends a 401 error response.
func Unauthorized(c fiber.Ctx, message string) error {
	return Send(c, fiber.StatusUnauthorized, "unauthorized", message)
}

// Forbidden sends a 403 error response.
func Forbidden(c fiber.Ctx, message string) error {
	return Send(c, fiber.StatusForbidden, "forbidden", message)
}

// SwaggerResponse is used only for OpenAPI/Swagger documentation annotations.
type SwaggerResponse struct {
	Error SwaggerDetail `json:"error"`
}

// SwaggerDetail is the swagger-annotated version of Detail with examples.
type SwaggerDetail struct {
	Code    string `json:"code" example:"not_found"`
	Message string `json:"message" example:"resource not found"`
}
