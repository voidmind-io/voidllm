package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/voidmind-io/voidllm/internal/money"
)

// ErrTokenBudgetExceeded is returned by BudgetCounter.Check when a token
// budget is exceeded.
var ErrTokenBudgetExceeded = errors.New("token budget exceeded")

// ErrCostBudgetExceeded is returned by BudgetCounter.Check when a cost budget
// is exceeded and no token budget is.
var ErrCostBudgetExceeded = errors.New("cost budget exceeded")

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

// BudgetCounter tracks token and cost usage per scope in daily and monthly
// UTC windows and enforces the budgets configured for those scopes.
// Implementations must be safe for concurrent use.
type BudgetCounter interface {
	// Check verifies that no scope in s has used up a token or cost budget
	// in l. A budget is available while the usage is below its limit; a zero
	// limit means unlimited, so a caller that must not enforce cost budgets
	// passes l.WithoutCostLimits(). The user scope is skipped when s.UserID
	// is empty and the team scope when s.TeamID is empty. When a budget is
	// used up it returns a *LimitError whose RetryAfter is the time until the
	// latest reset among all exceeded budgets, because the request stays
	// blocked until every one of them has reset. The error wraps
	// ErrTokenBudgetExceeded when at least one token budget is exceeded and
	// ErrCostBudgetExceeded when only cost budgets are.
	Check(ctx context.Context, s Scopes, l ScopeLimits) error
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
	// total_tokens, cost_estimate, in_day) for usage events since monthStart.
	// team_id and user_id are empty strings when unset, cost_estimate is 0
	// when the event has no cost, and in_day is 1 for events since dayStart
	// and 0 for earlier ones.
	QueryUsageSeed(ctx context.Context, monthStart, dayStart time.Time) (RowScanner, error)
	// QueryUsageTotals returns the usage of the events from from (inclusive)
	// until until (exclusive; the zero time means no upper bound),
	// aggregated by the database: rows of (org_id, team_id, user_id, key_id,
	// tokens, cost_micros, day_tokens, day_cost_micros), where the day
	// columns only cover events since dayStart. team_id and user_id are
	// empty strings when unset; costs are integer micro-units, rounded per
	// event like money.FromCost does.
	QueryUsageTotals(ctx context.Context, from, until, dayStart time.Time) (RowScanner, error)
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
	metricCost    = "cost"
	windowDaily   = "daily"
	windowMonthly = "monthly"
)

// maxBudgetChecks is the largest number of budgets one request can be subject
// to: 4 scopes x daily/monthly x tokens/cost. Cost budgets are enforced in
// addition to token budgets, so the bound covers both.
const maxBudgetChecks = 16

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

// budgetCheck is one budget a request is subject to: a token or cost limit of
// one scope in one window.
type budgetCheck struct {
	scope   string // counter scope key, e.g. "team:<id>"
	kind    string // scope kind, e.g. "team"
	monthly bool   // monthly window when true, daily otherwise
	cost    bool   // cost budget (micro-units) when true, token budget otherwise
	limit   int64  // always > 0
}

// spentOf returns the part of u that the budget c is measured against.
func (c budgetCheck) spentOf(u WindowUsage) int64 {
	w := u.Daily
	if c.monthly {
		w = u.Monthly
	}
	if c.cost {
		return w.CostMicros
	}
	return w.Tokens
}

// appendBudgetChecks appends one budgetCheck per configured (non-zero) token
// or cost limit of the scopes in s to dst, in the order key, user, team, org;
// within a scope the token budgets come before the cost budgets and the daily
// window before the monthly one. Scopes without any limit add nothing, so a
// request without budgets yields no checks at all.
func appendBudgetChecks(dst []budgetCheck, s Scopes, l ScopeLimits) []budgetCheck {
	if hasBudget(l.Key) {
		dst = appendScopeChecks(dst, scopeKindKey, KeyScope(s.KeyID), l.Key)
	}
	if s.UserID != "" && hasBudget(l.User) {
		dst = appendScopeChecks(dst, scopeKindUser, UserScope(s.OrgID, s.UserID), l.User)
	}
	if s.TeamID != "" && hasBudget(l.Team) {
		dst = appendScopeChecks(dst, scopeKindTeam, TeamScope(s.TeamID), l.Team)
	}
	if hasBudget(l.Org) {
		dst = appendScopeChecks(dst, scopeKindOrg, OrgScope(s.OrgID), l.Org)
	}
	return dst
}

// hasBudget reports whether lim sets a daily or monthly token or cost limit.
func hasBudget(lim Limits) bool {
	return lim.DailyTokenLimit > 0 || lim.MonthlyTokenLimit > 0 ||
		lim.DailyCostLimit > 0 || lim.MonthlyCostLimit > 0
}

// appendScopeChecks appends the daily and monthly token and cost budgets that
// lim sets for one scope to dst.
func appendScopeChecks(dst []budgetCheck, kind, scope string, lim Limits) []budgetCheck {
	if lim.DailyTokenLimit > 0 {
		dst = append(dst, budgetCheck{scope: scope, kind: kind, limit: lim.DailyTokenLimit})
	}
	if lim.MonthlyTokenLimit > 0 {
		dst = append(dst, budgetCheck{scope: scope, kind: kind, monthly: true, limit: lim.MonthlyTokenLimit})
	}
	if lim.DailyCostLimit > 0 {
		dst = append(dst, budgetCheck{scope: scope, kind: kind, cost: true, limit: lim.DailyCostLimit})
	}
	if lim.MonthlyCostLimit > 0 {
		dst = append(dst, budgetCheck{scope: scope, kind: kind, monthly: true, cost: true, limit: lim.MonthlyCostLimit})
	}
	return dst
}

// evaluateBudget compares the usage spent[i] with the limit of checks[i]. It
// returns nil while every usage is below its limit. Otherwise it returns a
// *LimitError whose RetryAfter, Metric, Scope and Window describe the exceeded
// budget with the latest reset: daily budgets reset at the next UTC midnight,
// monthly ones on the 1st of the next UTC month. Among budgets that reset at
// the same time a token budget is described rather than a cost budget, and
// otherwise the first in check order. The error wraps ErrTokenBudgetExceeded
// when any token budget is exceeded, whichever budget resets last, and
// ErrCostBudgetExceeded when only cost budgets are.
func evaluateBudget(checks []budgetCheck, spent []int64, w budgetWindows) error {
	var (
		latest         budgetCheck
		latestReset    time.Duration
		found          bool
		tokensExceeded bool
	)
	for i, c := range checks {
		if spent[i] < c.limit {
			continue
		}
		if !c.cost {
			tokensExceeded = true
		}
		reset := nextUTCMidnight(w.now).Sub(w.now)
		if c.monthly {
			reset = nextUTCMonth(w.now).Sub(w.now)
		}
		if found && (reset < latestReset || (reset == latestReset && (c.cost || !latest.cost))) {
			continue
		}
		latest, latestReset, found = c, reset, true
	}
	if !found {
		return nil
	}
	exceeded := &LimitError{
		Err:        ErrCostBudgetExceeded,
		RetryAfter: latestReset,
		Metric:     metricTokens,
		Scope:      latest.kind,
		Window:     windowDaily,
	}
	if tokensExceeded {
		exceeded.Err = ErrTokenBudgetExceeded
	}
	if latest.cost {
		exceeded.Metric = metricCost
	}
	if latest.monthly {
		exceeded.Window = windowMonthly
	}
	return exceeded
}

// scanUsageSeed reads the usage events persisted in the month of w, in one
// query, and calls fn once per event and scope the event counts towards.
// inDay tells whether the event also falls into the day of w. The user scope
// is skipped for events without a user (team keys, service-account keys) and
// the team scope for events without a team.
func scanUsageSeed(ctx context.Context, seeder UsageSeeder, w budgetWindows, fn func(scope string, u Usage, inDay bool)) error {
	rows, err := seeder.QueryUsageSeed(ctx, w.monthStart, w.dayStart)
	if err != nil {
		return fmt.Errorf("query usage_events: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor, rows.Err is checked below

	for rows.Next() {
		var keyID, teamID, orgID, userID string
		var tokens, inDay int64
		var cost float64
		if err := rows.Scan(&keyID, &teamID, &orgID, &userID, &tokens, &cost, &inDay); err != nil {
			return fmt.Errorf("scan usage_events row: %w", err)
		}
		// A negative token count can only be bad data; it must not lower
		// the usage of a scope.
		u := Usage{Tokens: tokens, CostMicros: int64(money.FromCost(cost))}.clamped()
		forEachScope(Scopes{KeyID: keyID, UserID: userID, TeamID: teamID, OrgID: orgID}, func(scope string) {
			fn(scope, u, inDay != 0)
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate usage_events: %w", err)
	}
	return nil
}
