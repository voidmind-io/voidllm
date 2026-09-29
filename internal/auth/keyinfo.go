package auth

import (
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// KeyInfoFromRecord maps a db.KeyRecord — the row shape produced by
// db.LoadAllActiveKeys and db.LoadActiveKey — to a cache-ready KeyInfo. It
// copies the key's own limits plus the org, team, and per-user (org
// membership) limits resolved via JOIN, and resolves the effective RBAC role:
//
//   - user_key and session_key: system_admin if the owning user has the
//     is_system_admin flag set, otherwise the org membership role.
//   - team_key: always team_admin.
//   - sa_key: the role is derived from the owning service account, not from
//     k.team_id — api_keys.team_id is never populated on sa_key rows, so the
//     service account's own team_id (resolved via r.ServiceAccountTeamID and
//     r.ServiceAccountActive) is the only source of truth for whether the key
//     is team-bound or org-level. team_admin if the service account is
//     team-bound, org_admin if it is org-scoped. Note that KeyInfo.TeamID is
//     NOT set from the service account's team here — it stays derived from
//     k.team_id as for every other key type, so model access and team limits
//     for sa_key are unaffected by this change. The service account's own
//     team, when it has one, is instead copied into the separate
//     KeyInfo.ServiceAccountTeamID field, for callers (e.g. MCP server read
//     permission checks) that need to authorize a team-bound sa_key against
//     its own team without conflating it with TeamID's key-scoping meaning.
//
// The returned bool is false when the role could not be resolved with
// confidence — a user/session key with no org membership row, a sa_key whose
// service account row is missing or soft-deleted, or an unrecognized key
// type — in which case Role is defaulted to RoleMember and the caller should
// log a warning. It is true in every other case.
//
// For a cached key, the "no org membership row" default branch of the
// user_key / session_key case (RoleMember, ok=false) is unreachable in
// practice: Cacheable already refuses to cache any user_key or session_key
// with neither a membership row nor the is_system_admin flag set (see
// Cacheable), so a KeyRecord that reaches this function via a cache-write
// call site always has one or the other when it is a user_key or
// session_key — and whichever it has is resolved by one of the two cases
// above it, never by the default. The branch is kept here anyway as a
// defensive least-privilege default for any caller that invokes
// KeyInfoFromRecord directly on an uncached or stale record.
func KeyInfoFromRecord(r db.KeyRecord) (KeyInfo, bool) {
	ki := KeyInfo{
		ID:                    r.ID,
		KeyType:               r.KeyType,
		Name:                  r.Name,
		OrgID:                 r.OrgID,
		DailyTokenLimit:       r.DailyTokenLimit,
		MonthlyTokenLimit:     r.MonthlyTokenLimit,
		RequestsPerMinute:     r.RequestsPerMinute,
		RequestsPerDay:        r.RequestsPerDay,
		OrgDailyTokenLimit:    r.OrgDailyTokenLimit,
		OrgMonthlyTokenLimit:  r.OrgMonthlyTokenLimit,
		OrgRequestsPerMinute:  r.OrgRequestsPerMinute,
		OrgRequestsPerDay:     r.OrgRequestsPerDay,
		TeamDailyTokenLimit:   r.TeamDailyTokenLimit,
		TeamMonthlyTokenLimit: r.TeamMonthlyTokenLimit,
		TeamRequestsPerMinute: r.TeamRequestsPerMinute,
		TeamRequestsPerDay:    r.TeamRequestsPerDay,
		UserDailyTokenLimit:   r.UserDailyTokenLimit,
		UserMonthlyTokenLimit: r.UserMonthlyTokenLimit,
		UserRequestsPerMinute: r.UserRequestsPerMinute,
		UserRequestsPerDay:    r.UserRequestsPerDay,
		ExpiresAt:             r.ExpiresAt,
	}

	if r.TeamID != nil {
		ki.TeamID = *r.TeamID
	}
	if r.UserID != nil {
		ki.UserID = *r.UserID
	}
	if r.ServiceAccountID != nil {
		ki.ServiceAccountID = *r.ServiceAccountID
	}
	if r.ServiceAccountTeamID != nil {
		ki.ServiceAccountTeamID = *r.ServiceAccountTeamID
	}

	ok := true

	// Resolve role inline from JOIN columns — no secondary DB query needed.
	switch r.KeyType {
	case keygen.KeyTypeUser, keygen.KeyTypeSession:
		switch {
		case r.IsSystemAdmin == 1:
			ki.Role = RoleSystemAdmin
		case r.MembershipRole != "":
			ki.Role = r.MembershipRole
		default:
			ki.Role = RoleMember
			ok = false
		}
	case keygen.KeyTypeTeam:
		ki.Role = RoleTeamAdmin
	case keygen.KeyTypeSA:
		switch {
		case !r.ServiceAccountActive:
			ki.Role = RoleMember
			ok = false
		case r.ServiceAccountTeamID != nil && *r.ServiceAccountTeamID != "":
			ki.Role = RoleTeamAdmin
		default:
			ki.Role = RoleOrgAdmin
		}
	default:
		ki.Role = RoleMember
		ok = false
	}

	return ki, ok
}

// Cacheable reports whether r may be written into the in-memory key cache at
// all. This is a different question from the "ok" return value of
// KeyInfoFromRecord: "ok" says whether the resolved role could be pinned down
// with confidence, whereas Cacheable says whether the key may authenticate at
// all — caching it under any role, including the RoleMember least-privilege
// default, would still let it authenticate.
//
// sa_key records: the key's entire scope — its role, and via
// ServiceAccountTeamID whether it is team- or org-bound — is derived from the
// owning service account, not from the key row itself. A sa_key whose service
// account has been soft-deleted or no longer exists (r.ServiceAccountActive
// == false) must never authenticate again once that happens; caching it under
// any role would mean deleting a service account does not revoke the keys
// minted under it.
//
// user_key and session_key records: the key's owning user must still exist
// and not be soft-deleted (r.UserActive) — a soft-deleted user's key must
// never authenticate again, regardless of what its role would otherwise
// resolve to. Beyond that, ordinary (non-system-admin) users belong to
// exactly one organization, so their key must also still have a surviving
// org_memberships row for the key's org (r.MembershipExists); a user removed
// from an org must not keep authenticating as RoleMember with no per-user
// limits just because KeyInfoFromRecord's "no membership" branch defaults to
// a cacheable RoleMember rather than refusing the key outright. System
// admins are the one exception: their privilege comes from
// users.is_system_admin, a global flag independent of any org membership,
// and legacy deployments may have system-admin users with no membership row
// at all — such a key is still cacheable as long as its owning user is
// active. r.IsSystemAdmin is sourced from COALESCE(u.is_system_admin, 0) in
// keySelectFromJoin, which is already 0 whenever the user is soft-deleted
// (the users LEFT JOIN itself filters u.deleted_at IS NULL, so a deleted
// user's row never matches and the COALESCE falls back to 0) — so a
// soft-deleted system admin's key is still correctly refused by the
// r.UserActive check above, never let back in via the IsSystemAdmin
// exception.
//
// team_key records are always cacheable — a team_key has no owning user or
// service account for this function to invalidate against.
//
// Every call site that writes a KeyInfo derived from a db.KeyRecord into the
// key cache — LoadKeysIntoCache and every key-creation, rotation, or update
// path that calls db.LoadActiveKey followed by KeyInfoFromRecord — must check
// Cacheable first and skip the cache write (logging a warning with the key
// ID, never the raw key) when it returns false. A cache write path that finds
// an existing cache entry for a key that is no longer Cacheable must evict
// that entry rather than leave it in place.
func Cacheable(r db.KeyRecord) bool {
	switch r.KeyType {
	case keygen.KeyTypeSA:
		return r.ServiceAccountActive
	case keygen.KeyTypeUser, keygen.KeyTypeSession:
		return r.UserActive && (r.MembershipExists || r.IsSystemAdmin == 1)
	default:
		return true
	}
}

// derefStr returns the string value pointed to by s, or "" if s is nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
