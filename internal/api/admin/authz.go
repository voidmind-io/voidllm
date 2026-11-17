// Package admin provides HTTP handlers for the VoidLLM admin API.
// This file contains shared authorization helpers used across admin handlers.
package admin

import (
	"context"
	"errors"
	"log/slog"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
)

// requireOrgAccess checks that the caller belongs to the target org with at least
// the member role. System admins pass regardless of OrgID.
// On success it returns the caller's KeyInfo and true.
// On failure it writes the HTTP error response to c and returns nil, false.
// Callers must check the boolean and return nil from the handler when it is false,
// since the response has already been written.
func requireOrgAccess(c fiber.Ctx, orgID string) (*auth.KeyInfo, bool) {
	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil {
		_ = apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "missing authentication")
		return nil, false
	}
	isSystemAdmin := auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin)
	belongsToOrg := keyInfo.OrgID == orgID
	if !isSystemAdmin && !belongsToOrg {
		_ = apierror.Send(c, fiber.StatusForbidden, "forbidden", "insufficient permissions")
		return nil, false
	}
	return keyInfo, true
}

// requireOrgAdmin checks that the caller is either a system admin or an org admin
// of the target org. On success it returns the caller's KeyInfo and true.
// On failure it writes the HTTP error response to c and returns nil, false.
func requireOrgAdmin(c fiber.Ctx, orgID string) (*auth.KeyInfo, bool) {
	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil {
		_ = apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "missing authentication")
		return nil, false
	}
	isSystemAdmin := auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin)
	isOrgAdminOfTarget := auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) && keyInfo.OrgID == orgID
	if !isSystemAdmin && !isOrgAdminOfTarget {
		_ = apierror.Send(c, fiber.StatusForbidden, "forbidden", "insufficient permissions")
		return nil, false
	}
	return keyInfo, true
}

// requireTeamAccess checks that the caller belongs to the org and verifies the
// team (from URL param team_id) belongs to the org (from URL param org_id).
// For callers below org_admin level, an additional membership check is performed
// to ensure they are a member of the requested team.
// Returns keyInfo, the verified team, and ok. If !ok, the response has been written.
func (h *Handler) requireTeamAccess(c fiber.Ctx) (*auth.KeyInfo, *db.Team, bool) {
	orgID := c.Params("org_id")
	teamID := c.Params("team_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil, nil, false
	}

	team, err := h.DB.GetTeam(c.Context(), teamID)
	if err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			h.Log.ErrorContext(c.Context(), "requireTeamAccess: get team", slog.String("error", err.Error()))
		}
		_ = apierror.NotFound(c, "team not found")
		return nil, nil, false
	}
	if team.OrgID != orgID {
		_ = apierror.NotFound(c, "team not found")
		return nil, nil, false
	}

	// Callers below org_admin must be members of the team.
	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) {
		isMember, memberErr := h.DB.IsTeamMember(c.Context(), keyInfo.UserID, teamID)
		if memberErr != nil {
			h.Log.ErrorContext(c.Context(), "requireTeamAccess: check membership", slog.String("error", memberErr.Error()))
			_ = apierror.InternalError(c, "failed to verify team membership")
			return nil, nil, false
		}
		if !isMember {
			_ = apierror.NotFound(c, "team not found")
			return nil, nil, false
		}
	}

	return keyInfo, team, true
}

// canActOnUser decides whether caller may act on another user's credentials in
// orgID. A system admin caller, or a caller acting on itself, is always
// allowed. Otherwise the target must:
//  1. hold a membership in orgID (failing this yields 404 "user not found" so
//     existence is not revealed),
//  2. not be a system admin,
//  3. hold a strictly lower org role than the caller, and
//  4. when crossOrgSensitive is true (password or email changes, and issuing
//     or rotating a key for the user), hold no membership in any other
//     non-deleted organization.
//
// Rules 2-4 yield 403 "insufficient permissions". It returns status 0 when the
// action is allowed. A non-nil err reports a storage failure.
func (h *Handler) canActOnUser(ctx context.Context, caller *auth.KeyInfo, orgID, targetUserID string, crossOrgSensitive bool) (int, string, error) {
	if auth.HasRole(caller.Role, auth.RoleSystemAdmin) {
		return 0, "", nil
	}
	if caller.UserID != "" && caller.UserID == targetUserID {
		return 0, "", nil
	}

	targetRole, err := h.DB.GetOrgMembershipRole(ctx, targetUserID, orgID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return fiber.StatusNotFound, "user not found", nil
		}
		return 0, "", err
	}

	target, err := h.DB.GetUser(ctx, targetUserID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return fiber.StatusNotFound, "user not found", nil
		}
		return 0, "", err
	}
	if target.IsSystemAdmin {
		return fiber.StatusForbidden, "insufficient permissions", nil
	}

	// Strictly lower: the caller outranks the target and the target does not
	// reach the caller's rank. Unknown roles never satisfy HasRole.
	if !auth.HasRole(caller.Role, targetRole) || auth.HasRole(targetRole, caller.Role) {
		return fiber.StatusForbidden, "insufficient permissions", nil
	}

	if crossOrgSensitive {
		n, err := h.DB.CountOtherOrgMemberships(ctx, targetUserID, orgID)
		if err != nil {
			return 0, "", err
		}
		if n > 0 {
			return fiber.StatusForbidden, "insufficient permissions", nil
		}
	}
	return 0, "", nil
}
