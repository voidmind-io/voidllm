package audit

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// sensitiveFields lists JSON field names whose values must never be persisted
// in audit log descriptions. All keys are lowercase; redactMap normalises
// incoming field names with strings.ToLower before the lookup so that
// mixed-case variants such as "Password" or "API_KEY" are caught correctly.
// Matching is exact (not substring) to avoid false positives such as
// "max_tokens". When a field in this set is present in the request body, its
// value is replaced with the string "[REDACTED]" rather than being dropped
// entirely — so the audit trail shows that the field was sent (e.g. "password
// was changed") without exposing the value.
//
// Sources:
//   - "password":            createUserRequest, updateUserRequest (users.go)
//   - "api_key":             createModelRequest, updateModelRequest (models.go);
//     createDeploymentRequest, updateDeploymentRequest (deployments.go);
//     testConnectionRequest (models.go)
//   - "auth_token":          createMCPServerRequest, updateMCPServerRequest (mcp_servers.go)
//   - "oauth_client_secret": createMCPServerRequest, updateMCPServerRequest (mcp_servers.go)
//   - "client_secret":       ssoConfigRequest (org_sso.go)
//   - "key":                 setLicenseRequest (license.go)
//   - "token":               defense-in-depth for invite tokens and similar
//     single-field token payloads
var sensitiveFields = map[string]struct{}{
	"password":            {},
	"api_key":             {},
	"auth_token":          {},
	"oauth_client_secret": {},
	"client_secret":       {},
	"key":                 {},
	"token":               {},
}

// normalizeResourceType maps plural URL path segments to their canonical
// resource type names used in audit events.
var normalizeResourceType = map[string]string{
	"orgs":             "org",
	"teams":            "team",
	"users":            "user",
	"keys":             "key",
	"models":           "model",
	"members":          "membership",
	"service-accounts": "service_account",
	"invites":          "invite",
	"model-access":     "model_access",
	"model-aliases":    "model_alias",
	"mcp-servers":      "mcp_server",
	"mcp-access":       "mcp_access",
	"sso":              "sso_config",
	"settings":         "setting",
}

// verbOverrides lists path segments that represent an explicit action verb
// rather than a resource type, and maps them to the canonical action name.
var verbOverrides = map[string]string{
	"revoke":     "revoke",
	"activate":   "activate",
	"deactivate": "deactivate",
	"login":      "login",
	"logout":     "logout",
}

// Middleware returns a Fiber handler that records audit events for admin API
// mutations. It runs AFTER the downstream handler via c.Next() so that the
// HTTP status code is available. Only successful (2xx) mutation requests
// (POST, PUT, PATCH, DELETE) are logged. GET, OPTIONS, and HEAD are skipped.
func Middleware(logger *Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		err := c.Next()

		// Only audit mutation methods.
		method := c.Method()
		if method == fiber.MethodGet || method == fiber.MethodOptions || method == fiber.MethodHead {
			return err
		}

		// If the handler returned an error, do not log as successful.
		if err != nil {
			return err
		}

		// Only audit successful responses.
		status := c.Response().StatusCode()
		if status < 200 || status >= 300 {
			return err
		}

		action, resourceType, resourceID := parseRoute(c)
		if resourceType == "" {
			return err
		}

		keyInfo := auth.KeyInfoFromCtx(c)

		var actorID, actorType, actorKeyID, orgID string
		if keyInfo != nil {
			actorKeyID = keyInfo.ID
			orgID = keyInfo.OrgID
			if keyInfo.ServiceAccountID != "" {
				actorID = keyInfo.ServiceAccountID
				actorType = "service_account"
			} else if keyInfo.UserID != "" {
				actorID = keyInfo.UserID
				actorType = "user"
			} else {
				actorID = keyInfo.ID
				actorType = "key"
			}
		}

		description := buildDescription(c.Body())

		logger.Log(Event{
			Timestamp:    time.Now().UTC(),
			OrgID:        orgID,
			ActorID:      actorID,
			ActorType:    actorType,
			ActorKeyID:   actorKeyID,
			Action:       action,
			ResourceType: resourceType,
			ResourceID:   resourceID,
			Description:  description,
			IPAddress:    c.IP(),
			StatusCode:   status,
			RequestID:    apierror.RequestIDFromCtx(c),
		})

		return err
	}
}

// parseRoute derives the action, resource type, and resource ID from the
// matched Fiber route pattern. It strips the /api/v1/ prefix then inspects
// the remaining path segments.
//
// Verb-override segments (revoke, activate, deactivate, login, logout)
// take precedence over the HTTP method. Otherwise the action is inferred from
// the HTTP method: POST→create, PUT/PATCH→update, DELETE→delete.
//
// The resource type is the last non-parameter segment, normalized via
// normalizeResourceType. The resource ID is the value of the last path
// parameter, or empty for create actions (POST without a resource-specific ID
// in the final position).
func parseRoute(c fiber.Ctx) (action, resourceType, resourceID string) {
	routePath := c.Route().Path

	// MCP endpoints carry opaque JSON-RPC payloads that do not map to the
	// admin resource/action taxonomy. Skip them entirely.
	if strings.HasPrefix(routePath, "/api/v1/mcp/") {
		return "", "", ""
	}

	// Strip the /api/v1 prefix.
	trimmed := strings.TrimPrefix(routePath, "/api/v1")
	if trimmed == routePath {
		// Route is not under /api/v1 — not an admin route.
		return "", "", ""
	}
	trimmed = strings.TrimPrefix(trimmed, "/")
	if trimmed == "" {
		return "", "", ""
	}

	segments := strings.Split(trimmed, "/")

	// Walk segments to find verb override, last resource segment, and last param.
	// lastSegmentWasParam tracks whether the most recently processed segment was
	// a route parameter. This is used to avoid treating an org_id as the
	// resourceID for collection-level routes such as PUT /orgs/:org_id/model-access.
	var lastResource string
	var lastParam string
	var lastSegmentWasParam bool
	var verbAction string

	for _, seg := range segments {
		if seg == "" {
			continue
		}
		if strings.HasPrefix(seg, ":") {
			lastParam = c.Params(strings.TrimPrefix(seg, ":"))
			lastSegmentWasParam = true
			continue
		}
		if v, ok := verbOverrides[seg]; ok {
			verbAction = v
			// The segment before this verb is the resource type.
			// lastResource already holds it from the previous iteration.
			break
		}
		lastResource = seg
		lastSegmentWasParam = false
	}

	if lastResource == "" {
		return "", "", ""
	}

	normalized, ok := normalizeResourceType[lastResource]
	if !ok {
		return "", "", ""
	}
	resourceType = normalized

	// Determine action.
	if verbAction != "" {
		action = verbAction
	} else {
		switch c.Method() {
		case fiber.MethodPost:
			action = "create"
		case fiber.MethodPut, fiber.MethodPatch:
			action = "update"
		case fiber.MethodDelete:
			action = "delete"
		default:
			action = strings.ToLower(c.Method())
		}
	}

	// Resource ID: for create actions (POST to a collection), the response body
	// carries the new ID but it is not available here — leave it empty. For all
	// other mutations the last route parameter is the resource ID, but only when
	// the final meaningful segment was a parameter (not a resource-type segment).
	// This prevents collection-level routes like PUT /orgs/:org_id/model-access
	// from incorrectly using org_id as the resourceID.
	if action != "create" && lastParam != "" && lastSegmentWasParam {
		resourceID = lastParam
	}

	return action, resourceType, resourceID
}

// redactedValue is the JSON literal substituted for any sensitive field value.
const redactedValue = `"[REDACTED]"`

// buildDescription creates a compact JSON representation of the request body
// fields that were sent. This shows exactly what the caller changed without
// requiring a pre-change DB read. Fields with zero values are omitted.
// Sensitive field values (see sensitiveFields) are replaced with "[REDACTED]"
// rather than being dropped — the audit trail records that the field was
// present without exposing the secret. Redaction is applied recursively into
// nested objects and into objects contained in arrays.
// The result is stored as the audit event description.
func buildDescription(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	// any is intentional here: jsonx.Unmarshal of an arbitrary JSON document
	// into interface{} is the standard approach for schema-agnostic traversal.
	// The alternative — operating purely on RawMessage bytes — would require a
	// custom JSON tokeniser and is not meaningfully safer.
	var raw map[string]any
	if jsonx.Unmarshal(body, &raw) != nil {
		return ""
	}
	clean := redactMap(raw)
	if len(clean) == 0 {
		return ""
	}
	out, err := jsonx.Marshal(clean)
	if err != nil {
		return ""
	}
	return string(out)
}

// redactMap processes a JSON object map: it drops zero-value fields for
// non-sensitive keys, replaces sensitive key values with redactedValue, and
// recurses into nested objects and arrays of objects.
//
// The return type is map[string]any. any is required here because the values
// may be strings, numbers, booleans, nested maps, or slices — all originating
// from jsonx.Unmarshal into interface{}.
func redactMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if _, sensitive := sensitiveFields[strings.ToLower(k)]; sensitive {
			// Always include sensitive fields, but replace their value so the
			// audit trail shows the field was present (e.g. "password changed").
			out[k] = jsonx.RawMessage(redactedValue)
			continue
		}
		// Drop zero/empty values for non-sensitive fields (existing behaviour).
		if isZeroValue(v) {
			continue
		}
		// Recurse into nested objects.
		if nested, ok := v.(map[string]any); ok {
			if rec := redactMap(nested); len(rec) > 0 {
				out[k] = rec
			}
			continue
		}
		// Recurse into arrays: process object elements, pass scalars through.
		if arr, ok := v.([]any); ok {
			out[k] = redactSlice(arr)
			continue
		}
		out[k] = v
	}
	return out
}

// redactSlice processes a JSON array: object elements are passed through
// redactMap; scalar elements are included as-is.
//
// []any is the natural type produced by jsonx.Unmarshal for JSON arrays.
func redactSlice(arr []any) []any {
	out := make([]any, 0, len(arr))
	for _, elem := range arr {
		if nested, ok := elem.(map[string]any); ok {
			out = append(out, redactMap(nested))
		} else {
			out = append(out, elem)
		}
	}
	return out
}

// isZeroValue reports whether v represents a JSON zero/empty value that should
// be omitted from the audit description. It mirrors the original string-based
// checks: null, empty string, numeric zero, and boolean false.
func isZeroValue(v any) bool {
	if v == nil {
		return true
	}
	switch val := v.(type) {
	case string:
		return val == ""
	case float64:
		return val == 0
	case bool:
		return !val
	}
	return false
}
