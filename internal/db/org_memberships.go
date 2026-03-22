package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// orgMembershipSelectColumns is the ordered column list used in all org_memberships
// SELECT queries. It must match the scan order in scanOrgMembership.
const orgMembershipSelectColumns = "id, org_id, user_id, role, created_at"

// OrgMembership represents a single org_memberships record in the database.
type OrgMembership struct {
	ID        string
	OrgID     string
	UserID    string
	Role      string
	CreatedAt string
}

// CreateOrgMembershipParams holds the input for creating an org membership.
type CreateOrgMembershipParams struct {
	OrgID  string
	UserID string
	Role   string
}

// UpdateOrgMembershipParams holds optional fields for updating an org membership.
// A nil pointer means the field is not changed.
type UpdateOrgMembershipParams struct {
	Role *string
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
	p := d.dialect.Placeholder
	argN := 1
	var conditions []string
	var args []any

	conditions = append(conditions, "org_id = "+p(argN))
	args = append(args, orgID)
	argN++

	if cursor != "" {
		conditions = append(conditions, "id > "+p(argN))
		args = append(args, cursor)
		argN++
	}

	query := "SELECT " + orgMembershipSelectColumns + " FROM org_memberships" +
		" WHERE " + strings.Join(conditions, " AND ") +
		" ORDER BY id ASC LIMIT " + p(argN)
	args = append(args, limit)

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ListOrgMemberships query: %w", err)
	}
	defer rows.Close()

	var memberships []OrgMembership
	for rows.Next() {
		var m OrgMembership
		if err := rows.Scan(&m.ID, &m.OrgID, &m.UserID, &m.Role, &m.CreatedAt); err != nil {
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
	if params.Role == nil {
		return d.GetOrgMembership(ctx, id)
	}

	p := d.dialect.Placeholder
	argN := 1
	var setClauses []string
	var args []any

	setClauses = append(setClauses, "role = "+p(argN))
	args = append(args, *params.Role)
	argN++

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
	err := row.Scan(&m.ID, &m.OrgID, &m.UserID, &m.Role, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}
