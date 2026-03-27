package audit

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
)

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

// buildDescription creates a compact JSON representation of the request body
// fields that were sent. This shows exactly what the caller changed without
// requiring a pre-change DB read. Fields with zero values are omitted.
// The result is stored as the audit event description.
func buildDescription(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return ""
	}
	// Re-marshal only non-null, non-empty fields into compact JSON.
	clean := make(map[string]json.RawMessage, len(raw))
	for k, v := range raw {
		s := string(v)
		if s == "null" || s == `""` || s == "0" || s == "false" {
			continue
		}
		clean[k] = v
	}
	if len(clean) == 0 {
		return ""
	}
	out, err := json.Marshal(clean)
	if err != nil {
		return ""
	}
	return string(out)
}
