package admin

import (
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
)

// mcpUsageResponse is the JSON envelope returned by GetOrgMCPUsage and
// GetSystemMCPUsage.
type mcpUsageResponse struct {
	OrgID   string              `json:"org_id,omitempty"`
	From    string              `json:"from"`
	To      string              `json:"to"`
	GroupBy string              `json:"group_by,omitempty"`
	Data    []mcpUsageDataPoint `json:"data"`
}

// mcpUsageDataPoint holds aggregated MCP tool call metrics for one group within
// an MCP usage response.
type mcpUsageDataPoint struct {
	GroupKey      string  `json:"group_key,omitempty"`
	TotalCalls    int64   `json:"total_calls"`
	SuccessCount  int64   `json:"success_count"`
	ErrorCount    int64   `json:"error_count"`
	TimeoutCount  int64   `json:"timeout_count"`
	AvgDurationMS float64 `json:"avg_duration_ms"`
	CodeModeCalls int64   `json:"code_mode_calls"`
}

// validMCPGroupBy is the set of accepted group_by values for system-wide MCP
// usage endpoints. It extends the org-scoped set with "org".
var validMCPGroupBy = map[string]bool{
	"":       true,
	"org":    true,
	"server": true,
	"tool":   true,
	"team":   true,
	"key":    true,
	"user":   true,
	"day":    true,
	"hour":   true,
	"status": true,
}

// validMCPGroupByOrg is the subset of group_by values accepted by the
// org-scoped MCP usage endpoints. The "org" dimension is only valid for
// system-wide queries.
var validMCPGroupByOrg = map[string]bool{
	"":       true,
	"server": true,
	"tool":   true,
	"team":   true,
	"key":    true,
	"user":   true,
	"day":    true,
	"hour":   true,
	"status": true,
}

// parseMCPUsageParams parses and validates the from, to, and group_by query
// parameters shared by org-scoped MCP usage endpoints. It writes a 400
// response and returns false on any validation failure so callers can return
// nil immediately.
func parseMCPUsageParams(c fiber.Ctx) (from, to time.Time, groupBy string, ok bool) {
	from, to, ok = parseUsageRange(c)
	if !ok {
		return
	}

	groupBy = c.Query("group_by", "")
	if !validMCPGroupByOrg[groupBy] {
		_ = apierror.BadRequest(c, "group_by must be one of: server, tool, team, key, user, day, hour, status")
		ok = false
		return
	}

	ok = true
	return
}

// parseMCPSystemUsageParams parses and validates the from, to, and group_by
// query parameters for system-admin cross-org MCP usage endpoints. It
// additionally accepts "org" as a valid group_by dimension. Writes a 400
// response and returns false on validation failure.
func parseMCPSystemUsageParams(c fiber.Ctx) (from, to time.Time, groupBy string, ok bool) {
	from, to, ok = parseUsageRange(c)
	if !ok {
		return
	}

	groupBy = c.Query("group_by", "")
	if !validMCPGroupBy[groupBy] {
		_ = apierror.BadRequest(c, "group_by must be one of: org, server, tool, team, key, user, day, hour, status")
		ok = false
		return
	}

	ok = true
	return
}

// mcpAggregatesToDataPoints converts a slice of MCPUsageAggregate to the
// JSON-serialisable slice used in all MCP usage responses.
func mcpAggregatesToDataPoints(aggs []db.MCPUsageAggregate) []mcpUsageDataPoint {
	data := make([]mcpUsageDataPoint, len(aggs))
	for i, a := range aggs {
		data[i] = mcpUsageDataPoint{
			GroupKey:      a.GroupKey,
			TotalCalls:    a.TotalCalls,
			SuccessCount:  a.SuccessCount,
			ErrorCount:    a.ErrorCount,
			TimeoutCount:  a.TimeoutCount,
			AvgDurationMS: a.AvgDurationMS,
			CodeModeCalls: a.CodeModeCalls,
		}
	}
	return data
}

// GetOrgMCPUsage handles GET /api/v1/orgs/:org_id/mcp-usage.
// Returns aggregated MCP tool call metrics for the organization within a time
// range. Query parameters:
//   - from (required): RFC3339 timestamp marking the start of the range (inclusive).
//   - to (required): RFC3339 timestamp marking the end of the range (inclusive).
//   - group_by (optional): aggregation dimension — "", "server", "tool", "team", "key", "user", "day", "hour", or "status".
//
// @Summary      Get org MCP usage
// @Description  Returns aggregated MCP tool call metrics for the organization over a time range (max 90 days). Requires org admin.
// @Tags         usage
// @Produce      json
// @Param        org_id    path      string  true   "Organization ID"
// @Param        from      query     string  true   "Start of range (RFC3339)"
// @Param        to        query     string  true   "End of range (RFC3339)"
// @Param        group_by  query     string  false  "Aggregation dimension: server, tool, team, key, user, day, hour, status"
// @Success      200       {object}  mcpUsageResponse
// @Failure      400       {object}  swaggerErrorResponse
// @Failure      401       {object}  swaggerErrorResponse
// @Failure      403       {object}  swaggerErrorResponse
// @Failure      500       {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/mcp-usage [get]
func (h *Handler) GetOrgMCPUsage(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	from, to, groupBy, ok := parseMCPUsageParams(c)
	if !ok {
		return nil
	}

	aggregates, err := h.DB.GetMCPUsageAggregates(c.Context(), orgID, from, to, groupBy)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "get org mcp usage",
			slog.String("org_id", orgID),
			slog.String("error", err.Error()),
		)
		return apierror.InternalError(c, "failed to retrieve MCP usage data")
	}

	return c.JSON(mcpUsageResponse{
		OrgID:   orgID,
		From:    from.UTC().Format(time.RFC3339),
		To:      to.UTC().Format(time.RFC3339),
		GroupBy: groupBy,
		Data:    mcpAggregatesToDataPoints(aggregates),
	})
}

// GetSystemMCPUsage handles GET /api/v1/mcp-usage.
// Returns aggregated MCP tool call metrics across all organizations within a
// time range. Only system admins may call this endpoint. Supports all group_by
// dimensions accepted by GetOrgMCPUsage plus "org" to aggregate by
// organization. Query parameters:
//   - from (required): RFC3339 timestamp marking the start of the range (inclusive).
//   - to (required): RFC3339 timestamp marking the end of the range (inclusive).
//   - group_by (optional): aggregation dimension — "", "org", "server", "tool", "team", "key", "user", "day", "hour", or "status".
//
// @Summary      Get system-wide MCP usage
// @Description  Returns aggregated MCP tool call metrics across all organizations over a time range (max 90 days). Requires system_admin.
// @Tags         usage
// @Produce      json
// @Param        from      query     string  true   "Start of range (RFC3339)"
// @Param        to        query     string  true   "End of range (RFC3339)"
// @Param        group_by  query     string  false  "Aggregation dimension: org, server, tool, team, key, user, day, hour, status"
// @Success      200       {object}  mcpUsageResponse
// @Failure      400       {object}  swaggerErrorResponse
// @Failure      401       {object}  swaggerErrorResponse
// @Failure      403       {object}  swaggerErrorResponse
// @Failure      500       {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /mcp-usage [get]
func (h *Handler) GetSystemMCPUsage(c fiber.Ctx) error {
	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil || !auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin) {
		return apierror.Send(c, fiber.StatusForbidden, "forbidden", "system admin access required")
	}

	from, to, groupBy, ok := parseMCPSystemUsageParams(c)
	if !ok {
		return nil
	}

	aggregates, err := h.DB.GetCrossOrgMCPUsageAggregates(c.Context(), from, to, groupBy)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "system mcp usage: get aggregates",
			slog.String("error", err.Error()),
		)
		return apierror.InternalError(c, "failed to retrieve MCP usage data")
	}

	return c.JSON(mcpUsageResponse{
		From:    from.UTC().Format(time.RFC3339),
		To:      to.UTC().Format(time.RFC3339),
		GroupBy: groupBy,
		Data:    mcpAggregatesToDataPoints(aggregates),
	})
}
