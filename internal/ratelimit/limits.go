// Package ratelimit provides in-memory rate limiting and token budget enforcement.
package ratelimit

// Limits holds rate and token limit values for a single scope.
// Zero values mean unlimited.
type Limits struct {
	// RequestsPerMinute is the maximum number of requests allowed per minute.
	// Zero means unlimited.
	RequestsPerMinute int
	// RequestsPerDay is the maximum number of requests allowed per day.
	// Zero means unlimited.
	RequestsPerDay int
	// DailyTokenLimit is the maximum number of tokens allowed per calendar day (UTC).
	// Zero means unlimited.
	DailyTokenLimit int64
	// MonthlyTokenLimit is the maximum number of tokens allowed per calendar month (UTC).
	// Zero means unlimited.
	MonthlyTokenLimit int64
}

// Scopes identifies the key, user, team, and org a request belongs to, for
// counter keying and limit lookups. UserID is empty for keys that are not
// user-scoped (team keys, service-account keys); TeamID is empty for keys
// that are not team-scoped. Both are skipped by Checker and TokenCounter
// implementations when empty.
type Scopes struct {
	// KeyID is the API key's own ID. Always non-empty.
	KeyID string
	// UserID is the ID of the user the key belongs to, for enforcing per-user
	// limits set on the org membership across every key that user owns in
	// this org. Empty for keys that are not user-scoped.
	UserID string
	// TeamID is the ID of the team the key is scoped to. Empty for keys that
	// are not team-scoped.
	TeamID string
	// OrgID is the organization the key belongs to. Always non-empty.
	OrgID string
}

// ScopeLimits pairs each scope in Scopes with the Limits that apply to it.
type ScopeLimits struct {
	// Key holds the key-level limits.
	Key Limits
	// User holds the per-user limits, sourced from the org membership.
	User Limits
	// Team holds the team-level limits.
	Team Limits
	// Org holds the org-level limits.
	Org Limits
}
