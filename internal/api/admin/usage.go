package admin

import (
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
)

// usageResponse is the JSON envelope returned by GetOrgUsage and MyUsage.
type usageResponse struct {
	OrgID   string           `json:"org_id"`
	From    string           `json:"from"`
	To      string           `json:"to"`
	GroupBy string           `json:"group_by,omitempty"`
	Data    []usageDataPoint `json:"data"`
}

// usageDataPoint holds aggregated metrics for one group within a usage response.
type usageDataPoint struct {
	GroupKey         string  `json:"group_key,omitempty"`
	TotalRequests    int64   `json:"total_requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostEstimate     float64 `json:"cost_estimate"`
	AvgDurationMS    float64 `json:"avg_duration_ms"`
}

// validGroupBy is the set of accepted group_by values for usage endpoints.
var validGroupBy = map[string]bool{
	"":      true,
	"org":   true,
	"model": true,
	"team":  true,
	"key":   true,
	"user":  true,
	"day":   true,
	"hour":  true,
}

// validGroupByOrg is the subset of group_by values accepted by the org-scoped
// usage endpoints. The "org" dimension is only valid for system-wide queries.
var validGroupByOrg = map[string]bool{
	"":      true,
	"model": true,
	"team":  true,
	"key":   true,
	"user":  true,
	"day":   true,
	"hour":  true,
}

// maxUsageRangeDays is the maximum allowed time range for a usage query.
const maxUsageRangeDays = 90

// parseUsageRange parses and validates the from and to query parameters,
// writing a 400 response and returning false on any validation failure.
// It is called by parseUsageParams and parseSystemUsageParams.
func parseUsageRange(c fiber.Ctx) (from, to time.Time, ok bool) {
	fromStr := c.Query("from")
	toStr := c.Query("to")
	if fromStr == "" {
		_ = apierror.BadRequest(c, "from is required")
		return
	}
	if toStr == "" {
		_ = apierror.BadRequest(c, "to is required")
		return
	}

	var err error
	from, err = time.Parse(time.RFC3339, fromStr)
	if err != nil {
		_ = apierror.BadRequest(c, "from must be a valid RFC3339 timestamp")
		return
	}
	to, err = time.Parse(time.RFC3339, toStr)
	if err != nil {
		_ = apierror.BadRequest(c, "to must be a valid RFC3339 timestamp")
		return
	}

	if !from.Before(to) {
		_ = apierror.BadRequest(c, "from must be before to")
		return
	}
	if to.Sub(from) > maxUsageRangeDays*24*time.Hour {
		_ = apierror.BadRequest(c, "time range must not exceed 90 days")
		return
	}

	ok = true
	return
}

// parseUsageParams parses and validates the from, to, and group_by query
// parameters shared by org-scoped usage endpoints. It writes a 400 response and
// returns false on any validation failure so callers can return nil immediately.
func parseUsageParams(c fiber.Ctx) (from, to time.Time, groupBy string, ok bool) {
	from, to, ok = parseUsageRange(c)
	if !ok {
		return
	}

	groupBy = c.Query("group_by", "")
	if !validGroupByOrg[groupBy] {
		_ = apierror.BadRequest(c, "group_by must be one of: model, team, key, user, day, hour")
		ok = false
		return
	}

	ok = true
	return
}

// parseSystemUsageParams parses and validates the from, to, and group_by query
// parameters for system-admin cross-org usage endpoints. It additionally accepts
// "org" as a valid group_by dimension. Writes a 400 response and returns false
// on validation failure.
func parseSystemUsageParams(c fiber.Ctx) (from, to time.Time, groupBy string, ok bool) {
	from, to, ok = parseUsageRange(c)
	if !ok {
		return
	}

	groupBy = c.Query("group_by", "")
	if !validGroupBy[groupBy] {
		_ = apierror.BadRequest(c, "group_by must be one of: org, model, team, key, user, day, hour")
		ok = false
		return
	}

	ok = true
	return
}

// aggregatesToDataPoints converts a slice of UsageAggregate to the JSON-serialisable
// slice used in all usage responses.
func aggregatesToDataPoints(aggs []db.UsageAggregate) []usageDataPoint {
	data := make([]usageDataPoint, len(aggs))
	for i, a := range aggs {
		data[i] = usageDataPoint{
			GroupKey:         a.GroupKey,
			TotalRequests:    a.TotalRequests,
			PromptTokens:     a.PromptTokens,
			CompletionTokens: a.CompletionTokens,
			TotalTokens:      a.TotalTokens,
			CostEstimate:     a.CostEstimate,
			AvgDurationMS:    a.AvgDurationMS,
		}
	}
	return data
}

// GetOrgUsage handles GET /api/v1/orgs/:org_id/usage.
// Returns aggregated usage metrics for the organization within a time range.
// Query parameters:
//   - from (required): RFC3339 timestamp marking the start of the range (inclusive).
//   - to (required): RFC3339 timestamp marking the end of the range (inclusive).
//   - group_by (optional): aggregation dimension — "", "model", "team", "key", "user", "day", or "hour".
//
// @Summary      Get org usage
// @Description  Returns aggregated usage metrics for the organization over a time range (max 90 days). Requires org admin.
// @Tags         usage
// @Produce      json
// @Param        org_id    path      string  true   "Organization ID"
// @Param        from      query     string  true   "Start of range (RFC3339)"
// @Param        to        query     string  true   "End of range (RFC3339)"
// @Param        group_by  query     string  false  "Aggregation dimension: model, team, key, user, day, hour"
// @Success      200       {object}  usageResponse
// @Failure      400       {object}  swaggerErrorResponse
// @Failure      401       {object}  swaggerErrorResponse
// @Failure      403       {object}  swaggerErrorResponse
// @Failure      500       {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/usage [get]
func (h *Handler) GetOrgUsage(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	from, to, groupBy, ok := parseUsageParams(c)
	if !ok {
		return nil
	}

	aggregates, err := h.DB.GetUsageAggregates(c.Context(), orgID, from, to, groupBy)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "get org usage",
			slog.String("org_id", orgID),
			slog.String("error", err.Error()),
		)
		return apierror.InternalError(c, "failed to retrieve usage data")
	}

	return c.JSON(usageResponse{
		OrgID:   orgID,
		From:    from.UTC().Format(time.RFC3339),
		To:      to.UTC().Format(time.RFC3339),
		GroupBy: groupBy,
		Data:    aggregatesToDataPoints(aggregates),
	})
}

// SystemAdminUsage handles GET /api/v1/usage.
// Returns aggregated usage metrics across all organizations within a time range.
// Only system admins may call this endpoint. Supports all group_by dimensions
// accepted by GetOrgUsage plus "org" to aggregate by organization.
// Query parameters:
//   - from (required): RFC3339 timestamp marking the start of the range (inclusive).
//   - to (required): RFC3339 timestamp marking the end of the range (inclusive).
//   - group_by (optional): aggregation dimension — "", "org", "model", "team", "key", "user", "day", or "hour".
//
// @Summary      Get system-wide usage
// @Description  Returns aggregated usage metrics across all organizations over a time range (max 90 days). Requires system_admin.
// @Tags         usage
// @Produce      json
// @Param        from      query     string  true   "Start of range (RFC3339)"
// @Param        to        query     string  true   "End of range (RFC3339)"
// @Param        group_by  query     string  false  "Aggregation dimension: org, model, team, key, user, day, hour"
// @Success      200       {object}  usageResponse
// @Failure      400       {object}  swaggerErrorResponse
// @Failure      401       {object}  swaggerErrorResponse
// @Failure      403       {object}  swaggerErrorResponse
// @Failure      500       {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /usage [get]
func (h *Handler) SystemAdminUsage(c fiber.Ctx) error {
	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil || !auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin) {
		return apierror.Send(c, fiber.StatusForbidden, "forbidden", "system admin access required")
	}

	from, to, groupBy, ok := parseSystemUsageParams(c)
	if !ok {
		return nil
	}

	aggregates, err := h.DB.GetCrossOrgUsageAggregates(c.Context(), from, to, groupBy)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "system admin usage: get aggregates",
			slog.String("error", err.Error()),
		)
		return apierror.InternalError(c, "failed to retrieve usage data")
	}

	return c.JSON(usageResponse{
		From:    from.UTC().Format(time.RFC3339),
		To:      to.UTC().Format(time.RFC3339),
		GroupBy: groupBy,
		Data:    aggregatesToDataPoints(aggregates),
	})
}

// MyUsage handles GET /api/v1/usage/me.
// Returns aggregated usage metrics scoped to the authenticated user's own keys.
// No role restriction — any authenticated key can query its own usage.
// Query parameters:
//   - from (required): RFC3339 timestamp marking the start of the range (inclusive).
//   - to (required): RFC3339 timestamp marking the end of the range (inclusive).
//   - group_by (optional): aggregation dimension — "", "model", "team", "key", "user", "day", or "hour".
//
// @Summary      Get own usage
// @Description  Returns aggregated usage metrics scoped to the current key or user. Any authenticated key may call this endpoint.
// @Tags         usage
// @Produce      json
// @Param        from      query     string  true   "Start of range (RFC3339)"
// @Param        to        query     string  true   "End of range (RFC3339)"
// @Param        group_by  query     string  false  "Aggregation dimension: model, team, key, user, day, hour"
// @Success      200       {object}  usageResponse
// @Failure      400       {object}  swaggerErrorResponse
// @Failure      401       {object}  swaggerErrorResponse
// @Failure      500       {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /usage/me [get]
func (h *Handler) MyUsage(c fiber.Ctx) error {
	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil {
		return apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "missing authentication")
	}

	from, to, groupBy, ok := parseUsageParams(c)
	if !ok {
		return nil
	}

	filter := db.UsageFilter{
		OrgID: keyInfo.OrgID,
	}
	if keyInfo.UserID != "" {
		filter.UserID = keyInfo.UserID
	} else {
		// SA keys have no user_id — scope by key_id to see only own usage.
		filter.KeyID = keyInfo.ID
	}

	aggregates, err := h.DB.GetScopedUsageAggregates(c.Context(), filter, from, to, groupBy)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "my usage: get aggregates",
			slog.String("user_id", keyInfo.UserID),
			slog.String("error", err.Error()),
		)
		return apierror.InternalError(c, "failed to retrieve usage data")
	}

	return c.JSON(usageResponse{
		OrgID:   keyInfo.OrgID,
		From:    from.UTC().Format(time.RFC3339),
		To:      to.UTC().Format(time.RFC3339),
		GroupBy: groupBy,
		Data:    aggregatesToDataPoints(aggregates),
	})
}
