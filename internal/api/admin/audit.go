package admin

import (
	"log/slog"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/audit"
	"github.com/voidmind-io/voidllm/internal/auth"
)

// auditEventResponse is the JSON representation of a single audit log event.
type auditEventResponse struct {
	ID           string `json:"id"`
	Timestamp    string `json:"timestamp"`
	OrgID        string `json:"org_id"`
	ActorID      string `json:"actor_id"`
	ActorType    string `json:"actor_type"`
	ActorName    string `json:"actor_name,omitempty"`
	ActorKeyID   string `json:"actor_key_id"`
	Action       string `json:"action"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	Description  string `json:"description"`
	IPAddress    string `json:"ip_address"`
	StatusCode   int    `json:"status_code"`
	RequestID    string `json:"request_id,omitempty"`
}

// auditListResponse is the JSON envelope for GET /api/v1/audit-logs.
type auditListResponse struct {
	Data    []auditEventResponse `json:"data"`
	HasMore bool                 `json:"has_more"`
	// Cursor is the ID of the last event in this page. Pass it as the ?cursor
	// query parameter on the next request to retrieve the following page.
	Cursor string `json:"cursor,omitempty"`
}

// ListAuditLogs handles GET /api/v1/audit-logs.
// system_admin may query any org. org_admin is restricted to their own org.
// All other roles receive 403.
//
// @Summary      List audit logs
// @Description  Returns a paginated list of audit log events, ordered newest-first. system_admin can filter by any org_id; org_admin is scoped to their own org. Use the returned cursor value as the ?cursor query parameter to retrieve the next page.
// @Tags         Audit
// @Produce      json
// @Security     BearerAuth
// @Param        org_id        query  string  false  "Organization ID"
// @Param        actor_id      query  string  false  "Actor ID"
// @Param        resource_type query  string  false  "Resource type"
// @Param        action        query  string  false  "Action"
// @Param        from          query  string  false  "Start time (RFC3339)"
// @Param        to            query  string  false  "End time (RFC3339)"
// @Param        limit         query  int     false  "Page size (1–200, default 50)"
// @Param        cursor        query  string  false  "Cursor from previous page for forward pagination"
// @Success      200  {object}  auditListResponse
// @Failure      400  {object}  swaggerErrorResponse
// @Failure      403  {object}  swaggerErrorResponse
// @Failure      500  {object}  swaggerErrorResponse
// @Router       /audit-logs [get]
func (h *Handler) ListAuditLogs(c fiber.Ctx) error {
	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil {
		return apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "missing authentication")
	}

	// org_admin may only see their own org; system_admin may query any org.
	orgID := c.Query("org_id", "")
	if auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin) {
		// system_admin: use query param as-is (empty = all orgs).
	} else if auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) {
		// org_admin: force to own org regardless of query param.
		if keyInfo.OrgID == "" {
			return apierror.Send(c, fiber.StatusForbidden, "forbidden", "org context required")
		}
		orgID = keyInfo.OrgID
	} else {
		return apierror.Send(c, fiber.StatusForbidden, "forbidden", "insufficient permissions")
	}

	// Parse optional time range.
	var from, to time.Time
	if raw := c.Query("from", ""); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return apierror.BadRequest(c, "invalid 'from' timestamp; expected RFC3339 format")
		}
		from = parsed
	}
	if raw := c.Query("to", ""); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return apierror.BadRequest(c, "invalid 'to' timestamp; expected RFC3339 format")
		}
		to = parsed
	}

	limitStr := c.Query("limit", "50")
	limit, err := strconv.Atoi(limitStr)
	if err != nil {
		return apierror.BadRequest(c, "limit must be an integer")
	}

	cursor := c.Query("cursor", "")
	if cursor != "" {
		if _, err := uuid.Parse(cursor); err != nil {
			return apierror.BadRequest(c, "invalid cursor format")
		}
	}

	result, err := audit.Query(c.Context(), h.DB, audit.QueryParams{
		OrgID:        orgID,
		ActorID:      c.Query("actor_id", ""),
		ResourceType: c.Query("resource_type", ""),
		Action:       c.Query("action", ""),
		From:         from,
		To:           to,
		Cursor:       cursor,
		Limit:        limit,
	})
	if err != nil {
		h.Log.ErrorContext(c.Context(), "list audit logs", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to retrieve audit logs")
	}

	// Collect actor IDs by type so we can resolve human-readable names in bulk.
	var userIDs, saIDs []string
	for _, ev := range result.Events {
		if ev.ActorID == "" {
			continue
		}
		switch ev.ActorType {
		case "user":
			userIDs = append(userIDs, ev.ActorID)
		case "service_account":
			saIDs = append(saIDs, ev.ActorID)
		}
	}

	// Resolve actor IDs to display labels. system_admin uses an unscoped global
	// lookup (by design — they can see all orgs). org_admin path is restricted to
	// actors belonging to their own org to prevent cross-org name disclosure.
	actorNames := map[string]string{}
	if auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin) {
		userNames, err := h.DB.ResolveGroupLabels(c.Context(), "user", userIDs)
		if err != nil {
			h.Log.WarnContext(c.Context(), "resolve audit actor names", slog.String("error", err.Error()))
			userNames = map[string]string{}
		}
		saNames, err := h.DB.ResolveGroupLabels(c.Context(), "service_account", saIDs)
		if err != nil {
			h.Log.WarnContext(c.Context(), "resolve audit actor names", slog.String("error", err.Error()))
			saNames = map[string]string{}
		}
		for id, name := range userNames {
			actorNames[id] = name
		}
		for id, name := range saNames {
			actorNames[id] = name
		}
	} else {
		// org_admin: orgID is already forced to keyInfo.OrgID above.
		resolved, err := h.DB.ResolveOrgActorLabels(c.Context(), orgID, userIDs, saIDs)
		if err != nil {
			h.Log.WarnContext(c.Context(), "resolve audit actor names", slog.String("error", err.Error()))
		} else {
			actorNames = resolved
		}
	}

	resp := auditListResponse{
		Data:    make([]auditEventResponse, 0, len(result.Events)),
		HasMore: result.HasMore,
	}
	for _, ev := range result.Events {
		var actorName string
		switch ev.ActorType {
		case "user", "service_account":
			actorName = actorNames[ev.ActorID]
		}
		resp.Data = append(resp.Data, auditEventResponse{
			ID:           ev.ID,
			Timestamp:    ev.Timestamp.UTC().Format(time.RFC3339),
			OrgID:        ev.OrgID,
			ActorID:      ev.ActorID,
			ActorType:    ev.ActorType,
			ActorName:    actorName,
			ActorKeyID:   ev.ActorKeyID,
			Action:       ev.Action,
			ResourceType: ev.ResourceType,
			ResourceID:   ev.ResourceID,
			Description:  ev.Description,
			IPAddress:    ev.IPAddress,
			StatusCode:   ev.StatusCode,
			RequestID:    ev.RequestID,
		})
	}
	if result.HasMore && len(result.Events) > 0 {
		resp.Cursor = result.Events[len(result.Events)-1].ID
	}

	return c.JSON(resp)
}
