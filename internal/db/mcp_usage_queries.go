package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// MCPUsageAggregate holds aggregated MCP tool call metrics for a single group key.
type MCPUsageAggregate struct {
	// GroupKey is the value of the grouped column (e.g. server alias, tool name, date).
	// It is empty when no grouping is requested.
	GroupKey      string
	TotalCalls    int64
	SuccessCount  int64
	ErrorCount    int64
	TimeoutCount  int64
	AvgDurationMS float64
	CodeModeCalls int64
}

// GetMCPUsageAggregates returns aggregated MCP tool call metrics for an org
// within [from, to]. groupBy controls the aggregation dimension: "" returns a
// single totals row, "server" groups by server_alias, "tool" by tool_name,
// "team" by team_id, "key" by key_id, "user" by user_id, "day" by calendar
// day (UTC), "hour" by hour (UTC), "status" by status. Any other value returns
// an error.
func (d *DB) GetMCPUsageAggregates(ctx context.Context, orgID string, from, to time.Time, groupBy string) ([]MCPUsageAggregate, error) {
	// Map the caller-supplied groupBy string to a safe, hardcoded column expression.
	// User input never reaches the query string directly.
	var groupCol string
	switch groupBy {
	case "":
		// no-op: totals only
	case "server":
		groupCol = "server_alias"
	case "tool":
		groupCol = "tool_name"
	case "team":
		groupCol = "team_id"
	case "key":
		groupCol = "key_id"
	case "user":
		groupCol = "user_id"
	case "day":
		groupCol = "DATE(created_at)"
	case "hour":
		groupCol = d.dialect.HourTrunc()
	case "status":
		groupCol = "status"
	default:
		return nil, fmt.Errorf("GetMCPUsageAggregates: invalid groupBy %q", groupBy)
	}

	selectCol := coalesceSelectCol(groupCol)

	fromStr := from.UTC().Format(time.RFC3339)
	toStr := to.UTC().Format(time.RFC3339)
	p := d.dialect.Placeholder

	orderClause := " ORDER BY COUNT(*) DESC"
	if groupCol == "DATE(created_at)" || groupCol == d.dialect.HourTrunc() {
		orderClause = " ORDER BY " + groupCol
	}

	var query string
	if groupCol != "" {
		query = "SELECT " + selectCol + ", COUNT(*), " +
			"SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END), " +
			"SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END), " +
			"SUM(CASE WHEN status = 'timeout' THEN 1 ELSE 0 END), " +
			"COALESCE(AVG(duration_ms), 0), " +
			"SUM(CASE WHEN code_mode = 1 THEN 1 ELSE 0 END) " +
			"FROM mcp_tool_calls " +
			"WHERE org_id = " + p(1) +
			" AND created_at >= " + p(2) +
			" AND created_at <= " + p(3) +
			" GROUP BY " + groupCol +
			orderClause
	} else {
		query = "SELECT '' AS group_key, COUNT(*), " +
			"SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END), " +
			"SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END), " +
			"SUM(CASE WHEN status = 'timeout' THEN 1 ELSE 0 END), " +
			"COALESCE(AVG(duration_ms), 0), " +
			"SUM(CASE WHEN code_mode = 1 THEN 1 ELSE 0 END) " +
			"FROM mcp_tool_calls " +
			"WHERE org_id = " + p(1) +
			" AND created_at >= " + p(2) +
			" AND created_at <= " + p(3)
	}

	rows, err := d.sql.QueryContext(ctx, query, orgID, fromStr, toStr)
	if err != nil {
		return nil, fmt.Errorf("GetMCPUsageAggregates org %s: %w", orgID, err)
	}
	defer rows.Close()

	var results []MCPUsageAggregate
	for rows.Next() {
		var a MCPUsageAggregate
		if err := rows.Scan(
			&a.GroupKey,
			&a.TotalCalls,
			&a.SuccessCount,
			&a.ErrorCount,
			&a.TimeoutCount,
			&a.AvgDurationMS,
			&a.CodeModeCalls,
		); err != nil {
			return nil, fmt.Errorf("GetMCPUsageAggregates scan org %s: %w", orgID, err)
		}
		results = append(results, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetMCPUsageAggregates rows org %s: %w", orgID, err)
	}
	return results, nil
}

// MCPUsageFilter constrains MCP usage aggregation to a subset of tool call events.
// OrgID is always required. TeamID, UserID, and KeyID are optional additional filters.
type MCPUsageFilter struct {
	// OrgID is the organization to filter by. Required.
	OrgID string
	// TeamID limits results to events belonging to this team. Optional.
	TeamID string
	// UserID limits results to events belonging to this user. Optional.
	UserID string
	// KeyID limits results to events recorded for a specific API key. Optional.
	KeyID string
}

// GetScopedMCPUsageAggregates returns aggregated MCP tool call metrics for the
// given filter within [from, to]. It behaves like GetMCPUsageAggregates but
// accepts a MCPUsageFilter instead of a bare orgID, allowing additional team or
// user scoping. groupBy accepts the same values as GetMCPUsageAggregates.
func (d *DB) GetScopedMCPUsageAggregates(ctx context.Context, filter MCPUsageFilter, from, to time.Time, groupBy string) ([]MCPUsageAggregate, error) {
	var groupCol string
	switch groupBy {
	case "":
		// no-op: totals only
	case "server":
		groupCol = "server_alias"
	case "tool":
		groupCol = "tool_name"
	case "team":
		groupCol = "team_id"
	case "key":
		groupCol = "key_id"
	case "user":
		groupCol = "user_id"
	case "day":
		groupCol = "DATE(created_at)"
	case "hour":
		groupCol = d.dialect.HourTrunc()
	case "status":
		groupCol = "status"
	default:
		return nil, fmt.Errorf("GetScopedMCPUsageAggregates: invalid groupBy %q", groupBy)
	}

	selectCol := coalesceSelectCol(groupCol)

	fromStr := from.UTC().Format(time.RFC3339)
	toStr := to.UTC().Format(time.RFC3339)

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

	orderClause := " ORDER BY COUNT(*) DESC"
	if groupCol == "DATE(created_at)" || groupCol == d.dialect.HourTrunc() {
		orderClause = " ORDER BY " + groupCol
	}

	var query string
	if groupCol != "" {
		query = "SELECT " + selectCol + ", COUNT(*), " +
			"SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END), " +
			"SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END), " +
			"SUM(CASE WHEN status = 'timeout' THEN 1 ELSE 0 END), " +
			"COALESCE(AVG(duration_ms), 0), " +
			"SUM(CASE WHEN code_mode = 1 THEN 1 ELSE 0 END) " +
			"FROM mcp_tool_calls " +
			where +
			" GROUP BY " + groupCol +
			orderClause
	} else {
		query = "SELECT '' AS group_key, COUNT(*), " +
			"SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END), " +
			"SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END), " +
			"SUM(CASE WHEN status = 'timeout' THEN 1 ELSE 0 END), " +
			"COALESCE(AVG(duration_ms), 0), " +
			"SUM(CASE WHEN code_mode = 1 THEN 1 ELSE 0 END) " +
			"FROM mcp_tool_calls " +
			where
	}

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("GetScopedMCPUsageAggregates org %s: %w", filter.OrgID, err)
	}
	defer rows.Close()

	var results []MCPUsageAggregate
	for rows.Next() {
		var a MCPUsageAggregate
		if err := rows.Scan(
			&a.GroupKey,
			&a.TotalCalls,
			&a.SuccessCount,
			&a.ErrorCount,
			&a.TimeoutCount,
			&a.AvgDurationMS,
			&a.CodeModeCalls,
		); err != nil {
			return nil, fmt.Errorf("GetScopedMCPUsageAggregates scan org %s: %w", filter.OrgID, err)
		}
		results = append(results, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetScopedMCPUsageAggregates rows org %s: %w", filter.OrgID, err)
	}
	return results, nil
}

// GetCrossOrgMCPUsageAggregates returns aggregated MCP tool call metrics across
// all organizations within [from, to]. It behaves like GetMCPUsageAggregates
// but omits the org_id WHERE clause, making it suitable for system-wide
// reporting. groupBy accepts the same values as GetMCPUsageAggregates plus
// "org" which groups by org_id. Any other value returns an error.
func (d *DB) GetCrossOrgMCPUsageAggregates(ctx context.Context, from, to time.Time, groupBy string) ([]MCPUsageAggregate, error) {
	var groupCol string
	switch groupBy {
	case "":
		// no-op: totals only
	case "org":
		groupCol = "org_id"
	case "server":
		groupCol = "server_alias"
	case "tool":
		groupCol = "tool_name"
	case "team":
		groupCol = "team_id"
	case "key":
		groupCol = "key_id"
	case "user":
		groupCol = "user_id"
	case "day":
		groupCol = "DATE(created_at)"
	case "hour":
		groupCol = d.dialect.HourTrunc()
	case "status":
		groupCol = "status"
	default:
		return nil, fmt.Errorf("GetCrossOrgMCPUsageAggregates: invalid groupBy %q", groupBy)
	}

	selectCol := coalesceSelectCol(groupCol)

	fromStr := from.UTC().Format(time.RFC3339)
	toStr := to.UTC().Format(time.RFC3339)
	p := d.dialect.Placeholder

	orderClause := " ORDER BY COUNT(*) DESC"
	if groupCol == "DATE(created_at)" || groupCol == d.dialect.HourTrunc() {
		orderClause = " ORDER BY " + groupCol
	}

	var query string
	if groupCol != "" {
		query = "SELECT " + selectCol + ", COUNT(*), " +
			"SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END), " +
			"SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END), " +
			"SUM(CASE WHEN status = 'timeout' THEN 1 ELSE 0 END), " +
			"COALESCE(AVG(duration_ms), 0), " +
			"SUM(CASE WHEN code_mode = 1 THEN 1 ELSE 0 END) " +
			"FROM mcp_tool_calls " +
			"WHERE created_at >= " + p(1) +
			" AND created_at <= " + p(2) +
			" GROUP BY " + groupCol +
			orderClause
	} else {
		query = "SELECT '' AS group_key, COUNT(*), " +
			"SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END), " +
			"SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END), " +
			"SUM(CASE WHEN status = 'timeout' THEN 1 ELSE 0 END), " +
			"COALESCE(AVG(duration_ms), 0), " +
			"SUM(CASE WHEN code_mode = 1 THEN 1 ELSE 0 END) " +
			"FROM mcp_tool_calls " +
			"WHERE created_at >= " + p(1) +
			" AND created_at <= " + p(2)
	}

	rows, err := d.sql.QueryContext(ctx, query, fromStr, toStr)
	if err != nil {
		return nil, fmt.Errorf("GetCrossOrgMCPUsageAggregates: %w", err)
	}
	defer rows.Close()

	var results []MCPUsageAggregate
	for rows.Next() {
		var a MCPUsageAggregate
		if err := rows.Scan(
			&a.GroupKey,
			&a.TotalCalls,
			&a.SuccessCount,
			&a.ErrorCount,
			&a.TimeoutCount,
			&a.AvgDurationMS,
			&a.CodeModeCalls,
		); err != nil {
			return nil, fmt.Errorf("GetCrossOrgMCPUsageAggregates scan: %w", err)
		}
		results = append(results, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetCrossOrgMCPUsageAggregates rows: %w", err)
	}
	return results, nil
}
