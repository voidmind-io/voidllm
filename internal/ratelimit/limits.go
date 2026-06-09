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
