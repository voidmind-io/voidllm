package auth

import (
	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
)

// Role constants define the RBAC hierarchy for VoidLLM.
const (
	// RoleSystemAdmin has unrestricted access to all resources across all organizations.
	RoleSystemAdmin = "system_admin"
	// RoleOrgAdmin can manage all resources within their organization.
	RoleOrgAdmin = "org_admin"
	// RoleTeamAdmin can manage resources within their team.
	RoleTeamAdmin = "team_admin"
	// RoleMember has standard user-level access within their team.
	RoleMember = "member"
)

// roleRank maps each role to a numeric rank for hierarchy comparison.
// Higher values represent greater privilege.
var roleRank = map[string]int{
	RoleMember:      0,
	RoleTeamAdmin:   1,
	RoleOrgAdmin:    2,
	RoleSystemAdmin: 3,
}

// HasRole reports whether role has at least the required privilege level.
// Both role and required must be known roles; unknown values always return false.
func HasRole(role string, required string) bool {
	r, ok := roleRank[role]
	if !ok {
		return false
	}
	req, reqOK := roleRank[required]
	if !reqOK {
		return false
	}
	return r >= req
}

// RequireRole returns a Fiber middleware that rejects requests where the
// authenticated caller does not have at least the required role.
// Must be placed after Middleware in the handler chain.
func RequireRole(required string) fiber.Handler {
	return func(c fiber.Ctx) error {
		keyInfo := KeyInfoFromCtx(c)
		if keyInfo == nil {
			return apierror.Unauthorized(c, "missing authorization header")
		}
		if !HasRole(keyInfo.Role, required) {
			return apierror.Forbidden(c, "insufficient permissions")
		}
		return c.Next()
	}
}

