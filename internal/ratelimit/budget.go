package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// ErrTokenBudgetExceeded is returned by BudgetCounter.Check when a token
// budget is exceeded.
var ErrTokenBudgetExceeded = errors.New("token budget exceeded")

// Usage is the consumption of one request, or the accumulated consumption of
// one window.
type Usage struct {
	// Tokens is the number of tokens consumed.
	Tokens int64
	// CostMicros is the cost in micro-units of the instance currency.
	CostMicros int64
}

// WindowUsage is the accumulated usage of one scope in the current windows.
type WindowUsage struct {
	// Daily is the usage since the start of the current UTC day.
	Daily Usage
	// Monthly is the usage since the start of the current UTC month.
	Monthly Usage
}

// CheckOptions controls which budgets BudgetCounter.Check evaluates.
type CheckOptions struct {
	// EnforceCost asks Check to evaluate cost budgets in addition to token
	// budgets. Limits carries no cost limits yet, so there is nothing for it
	// to select and it does not change the outcome.
	EnforceCost bool
}

// BudgetCounter tracks token and cost usage per scope in daily and monthly
// UTC windows and enforces the budgets configured for those scopes.
// Implementations must be safe for concurrent use.
type BudgetCounter interface {
	// Check verifies that no scope in s has used up a budget in l. A budget
	// is available while the usage is below its limit; a zero limit means
	// unlimited. The user scope is skipped when s.UserID is empty and the
	// team scope when s.TeamID is empty. When a budget is used up it returns
	// a *LimitError wrapping ErrTokenBudgetExceeded whose RetryAfter is the
	// time until the latest reset among all exceeded windows, because the
	// request stays blocked until every one of them has reset.
	Check(ctx context.Context, s Scopes, l ScopeLimits, opts CheckOptions) error
	// Add records the usage of one request for every scope in s. It never
	// blocks on I/O.
	Add(ctx context.Context, s Scopes, u Usage)
	// Snapshot returns the current daily and monthly usage of the given
	// scope keys (see KeyScope, UserScope, TeamScope, OrgScope). The result
	// has an entry for every requested key; unknown scopes report zero.
	Snapshot(ctx context.Context, scopeKeys []string) (map[string]WindowUsage, error)
	// EvictStale releases state of expired windows and runs the periodic
	// maintenance of the implementation. It is meant to be called from a
	// ticker.
	EvictStale()
}

// UsageSeeder provides the persisted usage that budget counters are seeded
// from.
type UsageSeeder interface {
	// QueryUsageSeed returns rows of (key_id, team_id, org_id, user_id,
	// total_tokens, cost_estimate) for usage events since the given time.
	// team_id and user_id are empty strings when unset, cost_estimate is 0
	// when the event has no cost.
	QueryUsageSeed(ctx context.Context, since time.Time) (RowScanner, error)
}

// RowScanner iterates over database rows. *sql.Rows satisfies this interface.
type RowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Close() error
	Err() error
}

// Scope kinds, used as LimitError.Scope and as the prefix of scope keys.
const (
	scopeKindKey  = "key"
	scopeKindUser = "user"
	scopeKindTeam = "team"
	scopeKindOrg  = "org"
)

// Values of LimitError.Metric and LimitError.Window for budget errors.
const (
	metricTokens  = "tokens"
	windowDaily   = "daily"
	windowMonthly = "monthly"
)

// maxBudgetChecks is the largest number of budgets one request can be subject
// to: daily and monthly tokens on each of the four scopes.
const maxBudgetChecks = 8

// KeyScope returns the counter scope key of an API key.
func KeyScope(keyID string) string { return scopeKindKey + ":" + keyID }

// UserScope returns the counter scope key of a user within an org. It is
// org-bound because per-user limits are defined on the org membership.
func UserScope(orgID, userID string) string {
	return scopeKindUser + ":" + orgID + ":" + userID
}

// TeamScope returns the counter scope key of a team.
func TeamScope(teamID string) string { return scopeKindTeam + ":" + teamID }

// OrgScope returns the counter scope key of an org.
func OrgScope(orgID string) string { return scopeKindOrg + ":" + orgID }

// forEachScope calls fn with the counter scope key of every scope in s, in
// the order key, user, team, org. The user and team scopes are skipped when
// their ID is empty.
func forEachScope(s Scopes, fn func(scope string)) {
	fn(KeyScope(s.KeyID))
	if s.UserID != "" {
		fn(UserScope(s.OrgID, s.UserID))
	}
	if s.TeamID != "" {
		fn(TeamScope(s.TeamID))
	}
	fn(OrgScope(s.OrgID))
}

// budgetWindows holds the instant a counter operation happens at and the
// starts of the UTC day and month it falls into.
type budgetWindows struct {
	now        time.Time
	dayStart   time.Time
	monthStart time.Time
}

// windowsAt returns the daily and monthly windows containing now.
func windowsAt(now time.Time) budgetWindows {
	now = now.UTC()
	return budgetWindows{
		now:        now,
		dayStart:   time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC),
		monthStart: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC),
	}
}

// budgetCheck is one budget a request is subject to: a token limit of one
// scope in one window.
type budgetCheck struct {
	scope   string // counter scope key, e.g. "team:<id>"
	kind    string // scope kind, e.g. "team"
	monthly bool   // monthly window when true, daily otherwise
	limit   int64  // always > 0
}

// appendBudgetChecks appends one budgetCheck per configured (non-zero) token
// limit of the scopes in s to dst, in the order key, user, team, org with the
// daily window before the monthly one. Scopes without any limit add nothing,
// so a request without budgets yields no checks at all.
func appendBudgetChecks(dst []budgetCheck, s Scopes, l ScopeLimits) []budgetCheck {
	if hasTokenLimit(l.Key) {
		dst = appendScopeChecks(dst, scopeKindKey, KeyScope(s.KeyID), l.Key)
	}
	if s.UserID != "" && hasTokenLimit(l.User) {
		dst = appendScopeChecks(dst, scopeKindUser, UserScope(s.OrgID, s.UserID), l.User)
	}
	if s.TeamID != "" && hasTokenLimit(l.Team) {
		dst = appendScopeChecks(dst, scopeKindTeam, TeamScope(s.TeamID), l.Team)
	}
	if hasTokenLimit(l.Org) {
		dst = appendScopeChecks(dst, scopeKindOrg, OrgScope(s.OrgID), l.Org)
	}
	return dst
}

// hasTokenLimit reports whether lim sets a daily or monthly token limit.
func hasTokenLimit(lim Limits) bool {
	return lim.DailyTokenLimit > 0 || lim.MonthlyTokenLimit > 0
}

// appendScopeChecks appends the daily and monthly token budgets that lim sets
// for one scope to dst.
func appendScopeChecks(dst []budgetCheck, kind, scope string, lim Limits) []budgetCheck {
	if lim.DailyTokenLimit > 0 {
		dst = append(dst, budgetCheck{scope: scope, kind: kind, limit: lim.DailyTokenLimit})
	}
	if lim.MonthlyTokenLimit > 0 {
		dst = append(dst, budgetCheck{scope: scope, kind: kind, monthly: true, limit: lim.MonthlyTokenLimit})
	}
	return dst
}

// evaluateBudget compares the usage spent[i] with the limit of checks[i]. It
// returns nil while every usage is below its limit. Otherwise it returns a
// *LimitError wrapping ErrTokenBudgetExceeded that describes the exceeded
// budget with the latest reset: daily budgets reset at the next UTC midnight,
// monthly ones on the 1st of the next UTC month.
func evaluateBudget(checks []budgetCheck, spent []int64, w budgetWindows) error {
	var exceeded *LimitError
	for i, c := range checks {
		if spent[i] < c.limit {
			continue
		}
		reset, window := nextUTCMidnight(w.now).Sub(w.now), windowDaily
		if c.monthly {
			reset, window = nextUTCMonth(w.now).Sub(w.now), windowMonthly
		}
		if exceeded != nil && reset <= exceeded.RetryAfter {
			continue
		}
		exceeded = &LimitError{
			Err:        ErrTokenBudgetExceeded,
			RetryAfter: reset,
			Metric:     metricTokens,
			Scope:      c.kind,
			Window:     window,
		}
	}
	if exceeded == nil {
		return nil
	}
	return exceeded
}

// scanUsageSeed reads the usage events persisted since the given time and
// calls fn once per event and scope the event counts towards. The user scope
// is skipped for events without a user (team keys, service-account keys) and
// the team scope for events without a team.
func scanUsageSeed(ctx context.Context, seeder UsageSeeder, since time.Time, fn func(scope string, u Usage)) error {
	rows, err := seeder.QueryUsageSeed(ctx, since)
	if err != nil {
		return fmt.Errorf("query usage_events: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor, rows.Err is checked below

	for rows.Next() {
		var keyID, teamID, orgID, userID string
		var tokens int64
		var cost float64
		if err := rows.Scan(&keyID, &teamID, &orgID, &userID, &tokens, &cost); err != nil {
			return fmt.Errorf("scan usage_events row: %w", err)
		}
		u := Usage{Tokens: tokens, CostMicros: costToMicros(cost)}
		forEachScope(Scopes{KeyID: keyID, UserID: userID, TeamID: teamID, OrgID: orgID}, func(scope string) {
			fn(scope, u)
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate usage_events: %w", err)
	}
	return nil
}

// costToMicros converts a cost estimate in major currency units to
// micro-units, rounded to the nearest micro-unit. Values that are not
// positive (including NaN) convert to 0; values beyond the int64 range are
// capped.
func costToMicros(cost float64) int64 {
	if !(cost > 0) {
		return 0
	}
	micros := math.Round(cost * 1e6)
	if micros >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(micros)
}
