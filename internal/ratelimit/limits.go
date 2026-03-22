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

// EffectiveLimits calculates the most restrictive limits across key, team, and org scopes.
// For each field, the smallest non-zero value among the three inputs is selected.
// If all three values for a field are zero the result is zero (unlimited).
func EffectiveLimits(keyLimits, teamLimits, orgLimits Limits) Limits {
	return Limits{
		RequestsPerMinute: minNonZero(keyLimits.RequestsPerMinute, minNonZero(teamLimits.RequestsPerMinute, orgLimits.RequestsPerMinute)),
		RequestsPerDay:    minNonZero(keyLimits.RequestsPerDay, minNonZero(teamLimits.RequestsPerDay, orgLimits.RequestsPerDay)),
		DailyTokenLimit:   minNonZero64(keyLimits.DailyTokenLimit, minNonZero64(teamLimits.DailyTokenLimit, orgLimits.DailyTokenLimit)),
		MonthlyTokenLimit: minNonZero64(keyLimits.MonthlyTokenLimit, minNonZero64(teamLimits.MonthlyTokenLimit, orgLimits.MonthlyTokenLimit)),
	}
}

// minNonZero returns the smallest non-zero value among a and b.
// If both are zero, zero is returned (unlimited).
func minNonZero(a, b int) int {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// minNonZero64 returns the smallest non-zero value among a and b.
// If both are zero, zero is returned (unlimited).
func minNonZero64(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}
