package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/voidmind-io/voidllm/internal/money"
)

// orgMembershipSelectColumns is the ordered column list used in all org_memberships
// SELECT queries. It must match the scan order in scanOrgMembership.
const orgMembershipSelectColumns = "id, org_id, user_id, role, created_at, " +
	"daily_token_limit, monthly_token_limit, requests_per_minute, requests_per_day, " +
	"daily_cost_limit, monthly_cost_limit"

// OrgMembership represents a single org_memberships record in the database.
// The four limit fields cap a single user's aggregate usage within this org,
// enforced across every API key that user owns in that org. Zero means unlimited.
type OrgMembership struct {
	ID        string
	OrgID     string
	UserID    string
	Role      string
	CreatedAt string

	// DailyTokenLimit is the maximum number of tokens this user may consume per
	// day (UTC) across all their keys in this org. Zero means unlimited.
	DailyTokenLimit int64
	// MonthlyTokenLimit is the maximum number of tokens this user may consume
	// per calendar month (UTC) across all their keys in this org. Zero means
	// unlimited.
	MonthlyTokenLimit int64
	// RequestsPerMinute is the maximum number of requests this user may make
	// per minute across all their keys in this org. Zero means unlimited.
	RequestsPerMinute int
	// RequestsPerDay is the maximum number of requests this user may make per
	// day across all their keys in this org. Zero means unlimited.
	RequestsPerDay int
	// DailyCostLimit is the maximum cost this user may incur per day (UTC)
	// across all their keys in this org. Zero means unlimited.
	DailyCostLimit money.Micros
	// MonthlyCostLimit is the maximum cost this user may incur per calendar
	// month (UTC) across all their keys in this org. Zero means unlimited.
	MonthlyCostLimit money.Micros
}

// CreateOrgMembershipParams holds the input for creating an org membership.
type CreateOrgMembershipParams struct {
	OrgID  string
	UserID string
	Role   string
}

// UpdateOrgMembershipParams holds optional fields for updating an org membership.
// A nil pointer means the field is not changed. Only system admins and org
// admins may set the limit fields; enforcement happens in the API layer.
type UpdateOrgMembershipParams struct {
	Role              *string
	DailyTokenLimit   *int64
	MonthlyTokenLimit *int64
	RequestsPerMinute *int
	RequestsPerDay    *int
	DailyCostLimit    *money.Micros
	MonthlyCostLimit  *money.Micros
}

// CreateOrgMembership inserts a new org membership and returns the persisted record.
// It returns ErrConflict if the (org_id, user_id) pair already exists.
func (d *DB) CreateOrgMembership(ctx context.Context, params CreateOrgMembershipParams) (*OrgMembership, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("create org membership: generate id: %w", err)
	}

	p := d.dialect.Placeholder
	insertQuery := "INSERT INTO org_memberships (id, org_id, user_id, role, created_at) " +
		"VALUES (" + p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", CURRENT_TIMESTAMP)"

	selectQuery := "SELECT " + orgMembershipSelectColumns +
		" FROM org_memberships WHERE id = " + p(1)

	var m *OrgMembership
	err = d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx, insertQuery,
			id.String(),
			params.OrgID,
			params.UserID,
			params.Role,
		)
		if execErr != nil {
			return translateError(execErr)
		}

		row := q.QueryRowContext(ctx, selectQuery, id.String())
		var scanErr error
		m, scanErr = scanOrgMembership(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("create org membership: %w", err)
	}
	return m, nil
}

// GetOrgMembership retrieves an org membership by its ID.
// It returns ErrNotFound if the record does not exist.
func (d *DB) GetOrgMembership(ctx context.Context, id string) (*OrgMembership, error) {
	query := "SELECT " + orgMembershipSelectColumns +
		" FROM org_memberships WHERE id = " + d.dialect.Placeholder(1)

	row := d.sql.QueryRowContext(ctx, query, id)
	m, err := scanOrgMembership(row)
	if err != nil {
		return nil, fmt.Errorf("GetOrgMembership %s: %w", id, translateError(err))
	}
	return m, nil
}

// ListOrgMemberships returns a page of memberships for the given org, ordered by ID
// ascending. cursor is an exclusive lower bound on ID for keyset pagination; pass ""
// to start from the beginning. limit controls the maximum number of records returned.
func (d *DB) ListOrgMemberships(ctx context.Context, orgID string, cursor string, limit int) ([]OrgMembership, error) {
	q := d.newQueryArgs()
	conditions := orgMembershipScopeConditions(q, orgID)
	if cursor != "" {
		conditions = append(conditions, "id > "+q.bind(cursor))
	}

	query := "SELECT " + orgMembershipSelectColumns + " FROM org_memberships" +
		" WHERE " + strings.Join(conditions, " AND ") +
		" ORDER BY id ASC LIMIT " + q.bind(limit)

	rows, err := d.sql.QueryContext(ctx, query, q.args...)
	if err != nil {
		return nil, fmt.Errorf("ListOrgMemberships query: %w", err)
	}
	defer rows.Close()

	var memberships []OrgMembership
	for rows.Next() {
		var m OrgMembership
		if err := rows.Scan(
			&m.ID, &m.OrgID, &m.UserID, &m.Role, &m.CreatedAt,
			&m.DailyTokenLimit, &m.MonthlyTokenLimit, &m.RequestsPerMinute, &m.RequestsPerDay,
			&m.DailyCostLimit, &m.MonthlyCostLimit,
		); err != nil {
			return nil, fmt.Errorf("ListOrgMemberships scan: %w", err)
		}
		memberships = append(memberships, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListOrgMemberships rows: %w", err)
	}

	return memberships, nil
}

// UpdateOrgMembership applies a partial update to an org membership.
// Only non-nil fields in params are written. If all fields are nil the record
// is returned unchanged without issuing an UPDATE.
// It returns ErrNotFound if the record does not exist.
func (d *DB) UpdateOrgMembership(ctx context.Context, id string, params UpdateOrgMembershipParams) (*OrgMembership, error) {
	if params.Role == nil && params.DailyTokenLimit == nil && params.MonthlyTokenLimit == nil &&
		params.RequestsPerMinute == nil && params.RequestsPerDay == nil &&
		params.DailyCostLimit == nil && params.MonthlyCostLimit == nil {
		return d.GetOrgMembership(ctx, id)
	}

	p := d.dialect.Placeholder
	argN := 1
	var setClauses []string
	var args []any

	if params.Role != nil {
		setClauses = append(setClauses, "role = "+p(argN))
		args = append(args, *params.Role)
		argN++
	}
	if params.DailyTokenLimit != nil {
		setClauses = append(setClauses, "daily_token_limit = "+p(argN))
		args = append(args, *params.DailyTokenLimit)
		argN++
	}
	if params.MonthlyTokenLimit != nil {
		setClauses = append(setClauses, "monthly_token_limit = "+p(argN))
		args = append(args, *params.MonthlyTokenLimit)
		argN++
	}
	if params.RequestsPerMinute != nil {
		setClauses = append(setClauses, "requests_per_minute = "+p(argN))
		args = append(args, *params.RequestsPerMinute)
		argN++
	}
	if params.RequestsPerDay != nil {
		setClauses = append(setClauses, "requests_per_day = "+p(argN))
		args = append(args, *params.RequestsPerDay)
		argN++
	}
	if params.DailyCostLimit != nil {
		setClauses = append(setClauses, "daily_cost_limit = "+p(argN))
		args = append(args, int64(*params.DailyCostLimit))
		argN++
	}
	if params.MonthlyCostLimit != nil {
		setClauses = append(setClauses, "monthly_cost_limit = "+p(argN))
		args = append(args, int64(*params.MonthlyCostLimit))
		argN++
	}

	updateQuery := "UPDATE org_memberships SET " + strings.Join(setClauses, ", ") +
		" WHERE id = " + p(argN)
	args = append(args, id)

	selectQuery := "SELECT " + orgMembershipSelectColumns +
		" FROM org_memberships WHERE id = " + p(1)

	var m *OrgMembership
	err := d.WithTx(ctx, func(q Querier) error {
		result, execErr := q.ExecContext(ctx, updateQuery, args...)
		if execErr != nil {
			return translateError(execErr)
		}

		n, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return fmt.Errorf("rows affected: %w", rowsErr)
		}
		if n == 0 {
			return ErrNotFound
		}

		row := q.QueryRowContext(ctx, selectQuery, id)
		var scanErr error
		m, scanErr = scanOrgMembership(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("UpdateOrgMembership %s: %w", id, err)
	}
	return m, nil
}

// DeleteOrgMembership permanently removes an org membership by its ID.
// It returns ErrNotFound if no matching record exists.
func (d *DB) DeleteOrgMembership(ctx context.Context, id string) error {
	p := d.dialect.Placeholder
	query := "DELETE FROM org_memberships WHERE id = " + p(1)

	result, err := d.sql.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("DeleteOrgMembership %s: %w", id, translateError(err))
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("DeleteOrgMembership %s rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("DeleteOrgMembership %s: %w", id, ErrNotFound)
	}

	return nil
}

// scanOrgMembership scans a single org_memberships row returned by QueryRowContext.
func scanOrgMembership(row *sql.Row) (*OrgMembership, error) {
	var m OrgMembership
	err := row.Scan(
		&m.ID, &m.OrgID, &m.UserID, &m.Role, &m.CreatedAt,
		&m.DailyTokenLimit, &m.MonthlyTokenLimit, &m.RequestsPerMinute, &m.RequestsPerDay,
		&m.DailyCostLimit, &m.MonthlyCostLimit,
	)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// orgMembershipScopeConditions returns the WHERE conditions that decide which
// memberships a list call sees. It is shared by ListOrgMemberships and
// SummarizeOrgMemberships so the list and its summary can never diverge.
func orgMembershipScopeConditions(q *queryArgs, orgID string) []string {
	return []string{"org_id = " + q.bind(orgID)}
}

// OrgMembershipsSummary holds aggregate figures over all memberships of an org.
type OrgMembershipsSummary struct {
	// Total is the number of memberships in the org.
	Total int
	// Admins is the number of memberships with an admin role (org_admin or system_admin).
	Admins int
}

// SummarizeOrgMemberships aggregates the memberships that ListOrgMemberships
// would return for the same orgID across all pages.
func (d *DB) SummarizeOrgMemberships(ctx context.Context, orgID string) (*OrgMembershipsSummary, error) {
	q := d.newQueryArgs()
	query := "SELECT COUNT(*), " +
		"COALESCE(SUM(CASE WHEN role IN ('org_admin', 'system_admin') THEN 1 ELSE 0 END), 0)" +
		" FROM org_memberships WHERE " + strings.Join(orgMembershipScopeConditions(q, orgID), " AND ")

	var s OrgMembershipsSummary
	if err := d.sql.QueryRowContext(ctx, query, q.args...).Scan(&s.Total, &s.Admins); err != nil {
		return nil, fmt.Errorf("SummarizeOrgMemberships: %w", err)
	}
	return &s, nil
}
