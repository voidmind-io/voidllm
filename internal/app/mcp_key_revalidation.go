package app

import (
	"time"

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// keyRevalidator returns an mcp.KeyValidator built entirely from keyCache —
// the same in-memory *cache.Cache[string, auth.KeyInfo] auth.Middleware
// itself looks up on every ordinary request — so a subscriptions/listen
// Subscriber's captured identity (mcp.KeyIdentity, including the KeyHash
// field auth.Middleware populates at authentication time) can later be
// re-checked against the exact same source of truth, with no database call.
//
// The returned validator reports true only when all of the following hold:
//
//   - id.KeyHash is non-empty and resolves to an entry in keyCache at all —
//     a revoked or soft-deleted key is evicted from keyCache by
//     auth.LoadKeysIntoCache/StartCacheRefresh's own periodic reload (or
//     immediately, via Redis cache invalidation — see internal/app.New's
//     ChannelKeys subscriber), so a gone entry means a gone key;
//   - that entry's own ExpiresAt, if set, has not since passed;
//   - that entry's own ID, OrgID, TeamID, and Role are byte-for-byte
//     identical to what id captured at registration time — a key that still
//     exists but was reassigned to a different org/team, or had its role
//     changed, no longer represents the identity the subscriber was
//     registered under, and must not go on being treated as if it still
//     were.
//
// An identity with no captured KeyHash at all (id.KeyHash == "", e.g. a
// subscriptions/listen request with no authenticated caller attached — see
// mcp.KeyIdentityFromCtx) fails closed: there is nothing here to revalidate
// against, so it is never reported as still valid.
func keyRevalidator(keyCache *cache.Cache[string, auth.KeyInfo]) mcp.KeyValidator {
	return func(id mcp.KeyIdentity) bool {
		if id.KeyHash == "" {
			return false
		}
		ki, ok := keyCache.Get(id.KeyHash)
		if !ok {
			return false
		}
		if ki.ExpiresAt != nil && time.Now().After(*ki.ExpiresAt) {
			return false
		}
		return ki.ID == id.KeyID &&
			ki.OrgID == id.OrgID &&
			ki.TeamID == id.TeamID &&
			ki.Role == id.Role
	}
}
