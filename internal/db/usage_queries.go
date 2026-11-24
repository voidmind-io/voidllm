package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// coalesceSelectCol wraps nullable group columns with COALESCE so that
// rows.Scan into a plain string never receives a NULL value.
func coalesceSelectCol(groupCol string) string {
	switch groupCol {
	case "team_id", "key_id", "user_id":
		return "COALESCE(" + groupCol + ", '')"
	default:
		return groupCol
	}
}

// UsageAggregate holds aggregated usage metrics for a single group key.
type UsageAggregate struct {
	// GroupKey is the value of the grouped column (e.g. model name, team ID, date).
	// It is empty when no grouping is requested.
	GroupKey string
	// GroupLabel is the resolved human-readable name for the entity identified by
	// GroupKey (e.g. key name/hint, user display name, team name, org name).
	// It is populated by the handler layer after the aggregate query, not by SQL.
	// Empty for non-resolvable dimensions such as model, day, or hour.
	GroupLabel       string
	TotalRequests    int64
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	// CachedReadTokens is the subset of PromptTokens served from an upstream
	// prompt cache, summed across the group.
	CachedReadTokens int64
	// CacheWriteTokens is the subset of PromptTokens written to an upstream
	// prompt cache (Anthropic only), summed across the group.
	CacheWriteTokens int64
	CostEstimate     float64
	AvgDurationMS    float64
}

// GetUsageAggregates returns aggregated usage metrics for an org within [from, to].
// groupBy controls the aggregation dimension: "" returns a single totals row,
// "model" groups by model_name, "team" by team_id, "key" by key_id, "user" by
// user_id, "day" by calendar day (UTC), "hour" by hour (UTC). Any other value
// returns an error.
func (d *DB) GetUsageAggregates(ctx context.Context, orgID string, from, to time.Time, groupBy string) ([]UsageAggregate, error) {
	// Map the caller-supplied groupBy string to a safe, hardcoded column expression.
	// User input never reaches the query string directly.
	var groupCol string
	switch groupBy {
	case "":
		// no-op: totals only
	case "model":
		groupCol = "model_name"
	case "team":
		groupCol = "team_id"
	case "key":
		groupCol = "key_id"
	case "user":
		groupCol = "user_id"
	case "day":
		groupCol = dayBucketExpr
	case "hour":
		groupCol = hourBucketExpr
	default:
		return nil, fmt.Errorf("GetUsageAggregates: invalid groupBy %q", groupBy)
	}

	selectCol := coalesceSelectCol(groupCol)

	fromStr := FormatTimestamp(from)
	toStr := FormatTimestamp(to)

	var query string
	if groupCol != "" {
		query = "SELECT " + selectCol + ", COUNT(*), " +
			"COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0), " +
			"COALESCE(SUM(total_tokens), 0), " +
			"COALESCE(SUM(cached_read_tokens), 0), COALESCE(SUM(cache_write_tokens), 0), " +
			"COALESCE(SUM(cost_estimate), 0), " +
			"COALESCE(AVG(request_duration_ms), 0) " +
			"FROM usage_events " +
			"WHERE org_id = " + d.dialect.Placeholder(1) +
			" AND created_at >= " + d.dialect.Placeholder(2) +
			" AND created_at <= " + d.dialect.Placeholder(3) +
			" GROUP BY " + groupCol +
			" ORDER BY " + groupCol
	} else {
		query = "SELECT '' AS group_key, COUNT(*), " +
			"COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0), " +
			"COALESCE(SUM(total_tokens), 0), " +
			"COALESCE(SUM(cached_read_tokens), 0), COALESCE(SUM(cache_write_tokens), 0), " +
			"COALESCE(SUM(cost_estimate), 0), " +
			"COALESCE(AVG(request_duration_ms), 0) " +
			"FROM usage_events " +
			"WHERE org_id = " + d.dialect.Placeholder(1) +
			" AND created_at >= " + d.dialect.Placeholder(2) +
			" AND created_at <= " + d.dialect.Placeholder(3)
	}

	rows, err := d.sql.QueryContext(ctx, query, orgID, fromStr, toStr)
	if err != nil {
		return nil, fmt.Errorf("GetUsageAggregates org %s: %w", orgID, err)
	}
	defer rows.Close()

	var results []UsageAggregate
	for rows.Next() {
		var a UsageAggregate
		if err := rows.Scan(
			&a.GroupKey,
			&a.TotalRequests,
			&a.PromptTokens,
			&a.CompletionTokens,
			&a.TotalTokens,
			&a.CachedReadTokens,
			&a.CacheWriteTokens,
			&a.CostEstimate,
			&a.AvgDurationMS,
		); err != nil {
			return nil, fmt.Errorf("GetUsageAggregates scan org %s: %w", orgID, err)
		}
		results = append(results, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetUsageAggregates rows org %s: %w", orgID, err)
	}
	return results, nil
}

// UsageScope identifies which column to filter on when querying token usage.
type UsageScope int

const (
	// ScopeKey filters usage by api key ID (column: key_id).
	ScopeKey UsageScope = iota
	// ScopeTeam filters usage by team ID (column: team_id).
	ScopeTeam
	// ScopeOrg filters usage by organization ID (column: org_id).
	ScopeOrg
)

// UsageFilter constrains usage aggregation to a subset of events.
// OrgID is always required. TeamID, UserID, and KeyID are optional additional filters.
type UsageFilter struct {
	// OrgID is the organization to filter by. Required.
	OrgID string
	// TeamID limits results to events belonging to this team. Optional.
	TeamID string
	// UserID limits results to events belonging to this user. Optional.
	UserID string
	// KeyID limits results to events recorded for a specific API key. Optional.
	// Used to scope SA key usage which has no associated user_id.
	KeyID string
}

// GetScopedUsageAggregates returns aggregated usage metrics for the given filter
// within [from, to]. It behaves like GetUsageAggregates but accepts a UsageFilter
// instead of a bare orgID, allowing additional team or user scoping.
// groupBy accepts the same values as GetUsageAggregates.
func (d *DB) GetScopedUsageAggregates(ctx context.Context, filter UsageFilter, from, to time.Time, groupBy string) ([]UsageAggregate, error) {
	var groupCol string
	switch groupBy {
	case "":
		// no-op: totals only
	case "model":
		groupCol = "model_name"
	case "team":
		groupCol = "team_id"
	case "key":
		groupCol = "key_id"
	case "user":
		groupCol = "user_id"
	case "day":
		groupCol = dayBucketExpr
	case "hour":
		groupCol = hourBucketExpr
	default:
		return nil, fmt.Errorf("GetScopedUsageAggregates: invalid groupBy %q", groupBy)
	}

	selectCol := coalesceSelectCol(groupCol)

	fromStr := FormatTimestamp(from)
	toStr := FormatTimestamp(to)

	// Build the WHERE clause dynamically. User input (filter values) is always
	// passed as bind parameters — never interpolated into the query string.
	argN := 1
	p := d.dialect.Placeholder
	var conditions []string
	var args []any

	conditions = append(conditions, "org_id = "+p(argN))
	args = append(args, filter.OrgID)
	argN++

	conditions = append(conditions, "created_at >= "+p(argN))
	args = append(args, fromStr)
	argN++

	conditions = append(conditions, "created_at <= "+p(argN))
	args = append(args, toStr)
	argN++

	if filter.TeamID != "" {
		conditions = append(conditions, "team_id = "+p(argN))
		args = append(args, filter.TeamID)
		argN++
	}

	if filter.UserID != "" {
		conditions = append(conditions, "user_id = "+p(argN))
		args = append(args, filter.UserID)
		argN++
	}

	if filter.KeyID != "" {
		conditions = append(conditions, "key_id = "+p(argN))
		args = append(args, filter.KeyID)
		argN++ //nolint:ineffassign // argN kept for consistency if further conditions are added
	}

	where := "WHERE " + strings.Join(conditions, " AND ")

	var query string
	if groupCol != "" {
		query = "SELECT " + selectCol + ", COUNT(*), " +
			"COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0), " +
			"COALESCE(SUM(total_tokens), 0), " +
			"COALESCE(SUM(cached_read_tokens), 0), COALESCE(SUM(cache_write_tokens), 0), " +
			"COALESCE(SUM(cost_estimate), 0), " +
			"COALESCE(AVG(request_duration_ms), 0) " +
			"FROM usage_events " +
			where +
			" GROUP BY " + groupCol +
			" ORDER BY " + groupCol
	} else {
		query = "SELECT '' AS group_key, COUNT(*), " +
			"COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0), " +
			"COALESCE(SUM(total_tokens), 0), " +
			"COALESCE(SUM(cached_read_tokens), 0), COALESCE(SUM(cache_write_tokens), 0), " +
			"COALESCE(SUM(cost_estimate), 0), " +
			"COALESCE(AVG(request_duration_ms), 0) " +
			"FROM usage_events " +
			where
	}

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("GetScopedUsageAggregates org %s: %w", filter.OrgID, err)
	}
	defer rows.Close()

	var results []UsageAggregate
	for rows.Next() {
		var a UsageAggregate
		if err := rows.Scan(
			&a.GroupKey,
			&a.TotalRequests,
			&a.PromptTokens,
			&a.CompletionTokens,
			&a.TotalTokens,
			&a.CachedReadTokens,
			&a.CacheWriteTokens,
			&a.CostEstimate,
			&a.AvgDurationMS,
		); err != nil {
			return nil, fmt.Errorf("GetScopedUsageAggregates scan org %s: %w", filter.OrgID, err)
		}
		results = append(results, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetScopedUsageAggregates rows org %s: %w", filter.OrgID, err)
	}
	return results, nil
}

// GetCrossOrgUsageAggregates returns aggregated usage metrics across all
// organizations within [from, to]. It behaves like GetUsageAggregates but
// omits the org_id WHERE clause, making it suitable for system-wide reporting.
// groupBy accepts the same values as GetUsageAggregates plus "org" which groups
// by org_id. Any other value returns an error.
func (d *DB) GetCrossOrgUsageAggregates(ctx context.Context, from, to time.Time, groupBy string) ([]UsageAggregate, error) {
	var groupCol string
	switch groupBy {
	case "":
		// no-op: totals only
	case "org":
		groupCol = "org_id"
	case "model":
		groupCol = "model_name"
	case "team":
		groupCol = "team_id"
	case "key":
		groupCol = "key_id"
	case "user":
		groupCol = "user_id"
	case "day":
		groupCol = dayBucketExpr
	case "hour":
		groupCol = hourBucketExpr
	default:
		return nil, fmt.Errorf("GetCrossOrgUsageAggregates: invalid groupBy %q", groupBy)
	}

	selectCol := coalesceSelectCol(groupCol)

	fromStr := FormatTimestamp(from)
	toStr := FormatTimestamp(to)
	p := d.dialect.Placeholder

	var query string
	if groupCol != "" {
		query = "SELECT " + selectCol + ", COUNT(*), " +
			"COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0), " +
			"COALESCE(SUM(total_tokens), 0), " +
			"COALESCE(SUM(cached_read_tokens), 0), COALESCE(SUM(cache_write_tokens), 0), " +
			"COALESCE(SUM(cost_estimate), 0), " +
			"COALESCE(AVG(request_duration_ms), 0) " +
			"FROM usage_events " +
			"WHERE created_at >= " + p(1) +
			" AND created_at <= " + p(2) +
			" GROUP BY " + groupCol +
			" ORDER BY " + groupCol
	} else {
		query = "SELECT '' AS group_key, COUNT(*), " +
			"COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0), " +
			"COALESCE(SUM(total_tokens), 0), " +
			"COALESCE(SUM(cached_read_tokens), 0), COALESCE(SUM(cache_write_tokens), 0), " +
			"COALESCE(SUM(cost_estimate), 0), " +
			"COALESCE(AVG(request_duration_ms), 0) " +
			"FROM usage_events " +
			"WHERE created_at >= " + p(1) +
			" AND created_at <= " + p(2)
	}

	rows, err := d.sql.QueryContext(ctx, query, fromStr, toStr)
	if err != nil {
		return nil, fmt.Errorf("GetCrossOrgUsageAggregates: %w", err)
	}
	defer rows.Close()

	var results []UsageAggregate
	for rows.Next() {
		var a UsageAggregate
		if err := rows.Scan(
			&a.GroupKey,
			&a.TotalRequests,
			&a.PromptTokens,
			&a.CompletionTokens,
			&a.TotalTokens,
			&a.CachedReadTokens,
			&a.CacheWriteTokens,
			&a.CostEstimate,
			&a.AvgDurationMS,
		); err != nil {
			return nil, fmt.Errorf("GetCrossOrgUsageAggregates scan: %w", err)
		}
		results = append(results, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetCrossOrgUsageAggregates rows: %w", err)
	}
	return results, nil
}

// QueryUsageSeed implements ratelimit.UsageSeeder. It returns rows of
// (key_id, team_id, org_id, user_id, total_tokens, cost_estimate, in_day) for
// all usage events recorded on or after monthStart. team_id and user_id are
// returned as empty strings when NULL; cost_estimate is returned in major
// currency units as a float, 0 when NULL (no pricing configured); in_day is 1
// for events recorded on or after dayStart and 0 otherwise, so one read
// serves both the monthly and the daily window. The returned *sql.Rows must
// be closed by the caller.
func (d *DB) QueryUsageSeed(ctx context.Context, monthStart, dayStart time.Time) (*sql.Rows, error) {
	query := "SELECT key_id, COALESCE(team_id, ''), org_id, COALESCE(user_id, ''), total_tokens, " +
		"COALESCE(cost_estimate, 0), " +
		"CASE WHEN created_at >= " + d.dialect.Placeholder(1) + " THEN 1 ELSE 0 END " +
		"FROM usage_events WHERE created_at >= " + d.dialect.Placeholder(2)
	rows, err := d.sql.QueryContext(ctx, query, FormatTimestamp(dayStart), FormatTimestamp(monthStart))
	if err != nil {
		return nil, fmt.Errorf("QueryUsageSeed: %w", err)
	}
	return rows, nil
}

// QueryUsageTotals implements ratelimit.UsageSeeder. It aggregates the usage
// events recorded from from (inclusive) until until (exclusive; the zero time
// means no upper bound) and returns one row per (org_id, team_id, user_id,
// key_id) with the columns (org_id, team_id, user_id, key_id, tokens,
// cost_micros, day_tokens, day_cost_micros): tokens and cost sum every event
// in the range, the day columns only those recorded on or after dayStart.
// team_id and user_id are empty strings when NULL. Costs are integers in
// micro-units of the currency: each event's cost estimate is rounded to the
// nearest micro-unit before it is summed, the same way the budget counters
// round the cost of a single request, so the aggregate equals what they
// counted event by event. Events without pricing cost 0, and negative token
// counts or costs count as 0. The range filter uses the created_at index, so
// the cost of the query follows the size of the range. The returned *sql.Rows
// must be closed by the caller.
func (d *DB) QueryUsageTotals(ctx context.Context, from, until, dayStart time.Time) (*sql.Rows, error) {
	p := d.dialect.Placeholder
	const tokens = "CASE WHEN total_tokens > 0 THEN total_tokens ELSE 0 END"
	// FLOOR(x + 0.5) rounds half up on both dialects; ROUND would not
	// (PostgreSQL rounds double precision half to even). cost_estimate is
	// single precision on PostgreSQL and is widened before the arithmetic,
	// like the driver widens it when the seed query returns it.
	const micros = "CASE WHEN cost_estimate > 0 " +
		"THEN FLOOR(CAST(cost_estimate AS DOUBLE PRECISION) * 1000000.0 + 0.5) ELSE 0 END"
	// The day start is bound twice: SQLite placeholders cannot be reused.
	query := "SELECT org_id, COALESCE(team_id, ''), COALESCE(user_id, ''), key_id, " +
		"CAST(COALESCE(SUM(" + tokens + "), 0) AS BIGINT), " +
		"CAST(COALESCE(SUM(" + micros + "), 0) AS BIGINT), " +
		"CAST(COALESCE(SUM(CASE WHEN created_at >= " + p(1) + " THEN " + tokens + " ELSE 0 END), 0) AS BIGINT), " +
		"CAST(COALESCE(SUM(CASE WHEN created_at >= " + p(2) + " THEN " + micros + " ELSE 0 END), 0) AS BIGINT) " +
		"FROM usage_events WHERE created_at >= " + p(3)
	day := FormatTimestamp(dayStart)
	args := []any{day, day, FormatTimestamp(from)}
	if !until.IsZero() {
		query += " AND created_at < " + p(4)
		args = append(args, FormatTimestamp(until))
	}
	query += " GROUP BY org_id, COALESCE(team_id, ''), COALESCE(user_id, ''), key_id"

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("QueryUsageTotals: %w", err)
	}
	return rows, nil
}

// GetMonthlyTokenUsage returns the total number of tokens consumed by an
// organization from the first day of the current calendar month (UTC) up to
// the present moment. Returns 0 with no error when there are no matching rows.
func (d *DB) GetMonthlyTokenUsage(ctx context.Context, orgID string) (int64, error) {
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	query := "SELECT COALESCE(SUM(total_tokens), 0) FROM usage_hourly " +
		"WHERE org_id = " + d.dialect.Placeholder(1) +
		// Plain text comparison: bucket_hour is always canonical, and casting
		// the column would prevent the (org_id, bucket_hour) index from serving
		// the range.
		" AND bucket_hour >= " + d.dialect.Placeholder(2)

	var total int64
	err := d.sql.QueryRowContext(ctx, query, orgID, FormatTimestamp(monthStart)).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("GetMonthlyTokenUsage org %s: %w", orgID, err)
	}
	return total, nil
}

// GetTokenUsageSince returns the total number of tokens consumed by the given
// scope/id combination since the provided time. The since parameter is
// interpreted in UTC. Returns 0 with no error when there are no matching rows.
func (d *DB) GetTokenUsageSince(ctx context.Context, scope UsageScope, id string, since time.Time) (int64, error) {
	var col string
	switch scope {
	case ScopeKey:
		col = "key_id"
	case ScopeTeam:
		col = "team_id"
	case ScopeOrg:
		col = "org_id"
	default:
		return 0, fmt.Errorf("GetTokenUsageSince: invalid usage scope %d", scope)
	}

	// col is selected from a controlled switch above — no user input reaches
	// the query string. The id and since values are passed as bind parameters.
	query := "SELECT COALESCE(SUM(total_tokens), 0) FROM usage_events WHERE " +
		col + " = " + d.dialect.Placeholder(1) +
		" AND created_at > " + d.dialect.Placeholder(2)

	var total int64
	row := d.sql.QueryRowContext(ctx, query, id, FormatTimestamp(since))
	if err := row.Scan(&total); err != nil {
		return 0, fmt.Errorf("GetTokenUsageSince %s %s: %w", col, id, err)
	}
	return total, nil
}
