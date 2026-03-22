package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// teamMembershipSelectColumns is the ordered column list used in all team_memberships
// SELECT queries. It must match the scan order in scanTeamMembership.
const teamMembershipSelectColumns = "id, team_id, user_id, role, created_at"

// TeamMembership represents a single team_memberships record in the database.
type TeamMembership struct {
	ID        string
	TeamID    string
	UserID    string
	Role      string
	CreatedAt string
}

// CreateTeamMembershipParams holds the input for creating a team membership.
type CreateTeamMembershipParams struct {
	TeamID string
	UserID string
	Role   string
}

// UpdateTeamMembershipParams holds optional fields for updating a team membership.
// A nil pointer means the field is not changed.
type UpdateTeamMembershipParams struct {
	Role *string
}

// CreateTeamMembership inserts a new team membership and returns the persisted record.
// It returns ErrConflict if the (team_id, user_id) pair already exists.
func (d *DB) CreateTeamMembership(ctx context.Context, params CreateTeamMembershipParams) (*TeamMembership, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("create team membership: generate id: %w", err)
	}

	p := d.dialect.Placeholder
	insertQuery := "INSERT INTO team_memberships (id, team_id, user_id, role, created_at) " +
		"VALUES (" + p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", CURRENT_TIMESTAMP)"

	selectQuery := "SELECT " + teamMembershipSelectColumns +
		" FROM team_memberships WHERE id = " + p(1)

	var m *TeamMembership
	err = d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx, insertQuery,
			id.String(),
			params.TeamID,
			params.UserID,
			params.Role,
		)
		if execErr != nil {
			return translateError(execErr)
		}

		row := q.QueryRowContext(ctx, selectQuery, id.String())
		var scanErr error
		m, scanErr = scanTeamMembership(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("create team membership: %w", err)
	}
	return m, nil
}

// GetTeamMembership retrieves a team membership by its ID.
// It returns ErrNotFound if the record does not exist.
func (d *DB) GetTeamMembership(ctx context.Context, id string) (*TeamMembership, error) {
	query := "SELECT " + teamMembershipSelectColumns +
		" FROM team_memberships WHERE id = " + d.dialect.Placeholder(1)

	row := d.sql.QueryRowContext(ctx, query, id)
	m, err := scanTeamMembership(row)
	if err != nil {
		return nil, fmt.Errorf("GetTeamMembership %s: %w", id, translateError(err))
	}
	return m, nil
}

// ListTeamMemberships returns a page of memberships for the given team, ordered by ID
// ascending. cursor is an exclusive lower bound on ID for keyset pagination; pass ""
// to start from the beginning. limit controls the maximum number of records returned.
func (d *DB) ListTeamMemberships(ctx context.Context, teamID string, cursor string, limit int) ([]TeamMembership, error) {
	p := d.dialect.Placeholder
	argN := 1
	var conditions []string
	var args []any

	conditions = append(conditions, "team_id = "+p(argN))
	args = append(args, teamID)
	argN++

	if cursor != "" {
		conditions = append(conditions, "id > "+p(argN))
		args = append(args, cursor)
		argN++
	}

	query := "SELECT " + teamMembershipSelectColumns + " FROM team_memberships" +
		" WHERE " + strings.Join(conditions, " AND ") +
		" ORDER BY id ASC LIMIT " + p(argN)
	args = append(args, limit)

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ListTeamMemberships query: %w", err)
	}
	defer rows.Close()

	var memberships []TeamMembership
	for rows.Next() {
		var m TeamMembership
		if err := rows.Scan(&m.ID, &m.TeamID, &m.UserID, &m.Role, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("ListTeamMemberships scan: %w", err)
		}
		memberships = append(memberships, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListTeamMemberships rows: %w", err)
	}

	return memberships, nil
}

// UpdateTeamMembership applies a partial update to a team membership.
// Only non-nil fields in params are written. If all fields are nil the record
// is returned unchanged without issuing an UPDATE.
// It returns ErrNotFound if the record does not exist.
func (d *DB) UpdateTeamMembership(ctx context.Context, id string, params UpdateTeamMembershipParams) (*TeamMembership, error) {
	if params.Role == nil {
		return d.GetTeamMembership(ctx, id)
	}

	p := d.dialect.Placeholder
	argN := 1
	var setClauses []string
	var args []any

	setClauses = append(setClauses, "role = "+p(argN))
	args = append(args, *params.Role)
	argN++

	updateQuery := "UPDATE team_memberships SET " + strings.Join(setClauses, ", ") +
		" WHERE id = " + p(argN)
	args = append(args, id)

	selectQuery := "SELECT " + teamMembershipSelectColumns +
		" FROM team_memberships WHERE id = " + p(1)

	var m *TeamMembership
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
		m, scanErr = scanTeamMembership(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("UpdateTeamMembership %s: %w", id, err)
	}
	return m, nil
}

// DeleteTeamMembership permanently removes a team membership by its ID.
// It returns ErrNotFound if no matching record exists.
func (d *DB) DeleteTeamMembership(ctx context.Context, id string) error {
	p := d.dialect.Placeholder
	query := "DELETE FROM team_memberships WHERE id = " + p(1)

	result, err := d.sql.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("DeleteTeamMembership %s: %w", id, translateError(err))
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("DeleteTeamMembership %s rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("DeleteTeamMembership %s: %w", id, ErrNotFound)
	}

	return nil
}

// scanTeamMembership scans a single team_memberships row returned by QueryRowContext.
func scanTeamMembership(row *sql.Row) (*TeamMembership, error) {
	var m TeamMembership
	err := row.Scan(&m.ID, &m.TeamID, &m.UserID, &m.Role, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}
