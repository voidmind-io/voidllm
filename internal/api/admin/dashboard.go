package admin

import (
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
)

// budgetWarning describes a token budget that has reached or exceeded 80% of
// the configured limit.
type budgetWarning struct {
	// Window is "daily" or "monthly".
	Window string `json:"window"`
	// Scope is always "org" for organization-level limits.
	Scope       string  `json:"scope"`
	Limit       int64   `json:"limit"`
	Usage       int64   `json:"usage"`
	PercentUsed float64 `json:"percent_used"`
}

// dashboardStatsResponse is the JSON envelope returned by DashboardStats.
// Fields tagged with omitempty are only included when relevant to the caller's scope.
type dashboardStatsResponse struct {
	// Scope indicates the data boundary: "org", "team", or "user".
	Scope           string          `json:"scope"`
	ActiveKeys      int             `json:"active_keys"`
	TotalTeams      int             `json:"total_teams,omitempty"`
	TotalMembers    int             `json:"total_members,omitempty"`
	Requests24h     int64           `json:"requests_24h"`
	Tokens24h       int64           `json:"tokens_24h"`
	CostEstimate24h float64         `json:"cost_estimate_24h"`
	BudgetWarnings  []budgetWarning `json:"budget_warnings,omitempty"`
	// ModelsHealthy is the count of registered models whose current health
	// status is "healthy". Zero when health monitoring is not enabled.
	ModelsHealthy int `json:"models_healthy"`
	// ModelsUnhealthy is the count of registered models whose current health
	// status is "unhealthy". Zero when health monitoring is not enabled.
	ModelsUnhealthy int `json:"models_unhealthy"`
	// ModelsDegraded is the count of registered models whose current health
	// status is "degraded". Zero when health monitoring is not enabled.
	ModelsDegraded int `json:"models_degraded"`
}

// DashboardStats handles GET /api/v1/dashboard/stats.
// Returns aggregate counts and 24-hour usage metrics scoped to the caller's role:
//
// @Summary      Get dashboard statistics
// @Description  Returns aggregate key counts and 24-hour usage metrics. Scope is determined by the caller's role: org (admin), team (team_admin), or user (member).
// @Tags         dashboard
// @Produce      json
// @Success      200  {object}  dashboardStatsResponse
// @Failure      401  {object}  swaggerErrorResponse
// @Failure      500  {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /dashboard/stats [get]
//   - system_admin / org_admin: org-wide counts and usage.
//   - team_admin: team-scoped counts and usage; returns empty team stats when
//     the caller has no team association.
//   - member: per-user key count and usage.
func (h *Handler) DashboardStats(c fiber.Ctx) error {
	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil {
		return apierror.InternalError(c, "failed to load dashboard stats")
	}

	ctx := c.Context()
	orgID := keyInfo.OrgID
	from := time.Now().UTC().Add(-24 * time.Hour)
	filter := db.UsageFilter{OrgID: orgID}

	var resp dashboardStatsResponse

	switch {
	case keyInfo.Role == auth.RoleSystemAdmin || keyInfo.Role == auth.RoleOrgAdmin:
		resp.Scope = "org"

		keys, err := h.DB.CountActiveKeys(ctx, orgID)
		if err != nil {
			h.Log.LogAttrs(ctx, slog.LevelError, "dashboard: count keys",
				slog.String("org_id", orgID),
				slog.String("error", err.Error()),
			)
			return apierror.InternalError(c, "failed to load dashboard stats")
		}

		teams, err := h.DB.CountTeams(ctx, orgID)
		if err != nil {
			h.Log.LogAttrs(ctx, slog.LevelError, "dashboard: count teams",
				slog.String("org_id", orgID),
				slog.String("error", err.Error()),
			)
			return apierror.InternalError(c, "failed to load dashboard stats")
		}

		members, err := h.DB.CountOrgMembers(ctx, orgID)
		if err != nil {
			h.Log.LogAttrs(ctx, slog.LevelError, "dashboard: count org members",
				slog.String("org_id", orgID),
				slog.String("error", err.Error()),
			)
			return apierror.InternalError(c, "failed to load dashboard stats")
		}

		resp.ActiveKeys = keys
		resp.TotalTeams = teams
		resp.TotalMembers = members

	case keyInfo.Role == auth.RoleTeamAdmin:
		teamID := keyInfo.TeamID
		if teamID == "" {
			// Session-based key — resolve the user's team from the membership table.
			var lookupErr error
			teamID, lookupErr = h.DB.GetUserTeamID(ctx, orgID, keyInfo.UserID)
			if lookupErr != nil {
				h.Log.LogAttrs(ctx, slog.LevelError, "dashboard: get user team",
					slog.String("org_id", orgID),
					slog.String("user_id", keyInfo.UserID),
					slog.String("error", lookupErr.Error()),
				)
				return apierror.InternalError(c, "failed to load dashboard stats")
			}
		}

		if teamID != "" {
			resp.Scope = "team"
			filter.TeamID = teamID

			keys, err := h.DB.CountTeamKeys(ctx, teamID)
			if err != nil {
				h.Log.LogAttrs(ctx, slog.LevelError, "dashboard: count team keys",
					slog.String("team_id", teamID),
					slog.String("error", err.Error()),
				)
				return apierror.InternalError(c, "failed to load dashboard stats")
			}

			members, err := h.DB.CountTeamMembers(ctx, teamID)
			if err != nil {
				h.Log.LogAttrs(ctx, slog.LevelError, "dashboard: count team members",
					slog.String("team_id", teamID),
					slog.String("error", err.Error()),
				)
				return apierror.InternalError(c, "failed to load dashboard stats")
			}

			resp.ActiveKeys = keys
			resp.TotalMembers = members
		} else {
			// team_admin with no team association — return empty scoped response.
			resp.Scope = "team"
			return c.JSON(resp)
		}

	default: // RoleMember
		resp.Scope = "user"
		filter.UserID = keyInfo.UserID

		keys, err := h.DB.CountUserKeys(ctx, orgID, keyInfo.UserID)
		if err != nil {
			h.Log.LogAttrs(ctx, slog.LevelError, "dashboard: count user keys",
				slog.String("org_id", orgID),
				slog.String("user_id", keyInfo.UserID),
				slog.String("error", err.Error()),
			)
			return apierror.InternalError(c, "failed to load dashboard stats")
		}

		resp.ActiveKeys = keys
	}

	agg, err := h.DB.GetHourlyUsageTotals(ctx, filter, from)
	if err != nil {
		h.Log.LogAttrs(ctx, slog.LevelError, "dashboard: get scoped usage",
			slog.String("org_id", orgID),
			slog.String("scope", resp.Scope),
			slog.String("error", err.Error()),
		)
		return apierror.InternalError(c, "failed to load dashboard stats")
	}

	resp.Requests24h = agg.TotalRequests
	resp.Tokens24h = agg.TotalTokens
	resp.CostEstimate24h = agg.CostEstimate

	if resp.Scope == "org" {
		org, err := h.DB.GetOrg(ctx, orgID)
		if err != nil {
			h.Log.LogAttrs(ctx, slog.LevelError, "dashboard: get org for budget warnings",
				slog.String("org_id", orgID),
				slog.String("error", err.Error()),
			)
			return apierror.InternalError(c, "failed to load dashboard stats")
		}

		const warnThreshold = 0.80

		if org.DailyTokenLimit > 0 {
			pct := float64(agg.TotalTokens) / float64(org.DailyTokenLimit)
			if pct >= warnThreshold {
				resp.BudgetWarnings = append(resp.BudgetWarnings, budgetWarning{
					Window:      "daily",
					Scope:       "org",
					Limit:       org.DailyTokenLimit,
					Usage:       agg.TotalTokens,
					PercentUsed: pct,
				})
			}
		}

		if org.MonthlyTokenLimit > 0 {
			monthlyUsage, err := h.DB.GetMonthlyTokenUsage(ctx, orgID)
			if err != nil {
				h.Log.LogAttrs(ctx, slog.LevelError, "dashboard: get monthly token usage",
					slog.String("org_id", orgID),
					slog.String("error", err.Error()),
				)
				return apierror.InternalError(c, "failed to load dashboard stats")
			}
			pct := float64(monthlyUsage) / float64(org.MonthlyTokenLimit)
			if pct >= warnThreshold {
				resp.BudgetWarnings = append(resp.BudgetWarnings, budgetWarning{
					Window:      "monthly",
					Scope:       "org",
					Limit:       org.MonthlyTokenLimit,
					Usage:       monthlyUsage,
					PercentUsed: pct,
				})
			}
		}
	}

	if h.HealthChecker != nil {
		for _, mh := range h.HealthChecker.GetAllHealth() {
			switch mh.Status {
			case "healthy":
				resp.ModelsHealthy++
			case "unhealthy":
				resp.ModelsUnhealthy++
			case "degraded":
				resp.ModelsDegraded++
			}
		}
	}

	return c.JSON(resp)
}
