package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// teamSelectColumns is the ordered column list used in all team SELECT queries.
// It must match the scan order in scanTeam.
const teamSelectColumns = "id, org_id, name, slug, daily_token_limit, monthly_token_limit, " +
	"requests_per_minute, requests_per_day, created_at, updated_at, deleted_at"

// Team represents a team record in the database.
type Team struct {
	ID                string
	OrgID             string
	Name              string
	Slug              string
	DailyTokenLimit   int64
	MonthlyTokenLimit int64
	RequestsPerMinute int
	RequestsPerDay    int
	CreatedAt         string
	UpdatedAt         string
	DeletedAt         *string
}

// CreateTeamParams holds the input for creating a team.
type CreateTeamParams struct {
	OrgID             string
	Name              string
	Slug              string
	DailyTokenLimit   int64
	MonthlyTokenLimit int64
	RequestsPerMinute int
	RequestsPerDay    int
}

// UpdateTeamParams holds optional fields for updating a team.
// A nil pointer means the field is not changed.
type UpdateTeamParams struct {
	Name              *string
	Slug              *string
	DailyTokenLimit   *int64
	MonthlyTokenLimit *int64
	RequestsPerMinute *int
	RequestsPerDay    *int
}

// CreateTeam inserts a new team and returns the persisted record.
// It returns ErrConflict if the (org_id, slug) pair is already taken.
func (d *DB) CreateTeam(ctx context.Context, params CreateTeamParams) (*Team, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("create team: generate id: %w", err)
	}

	p := d.dialect.Placeholder
	insertQuery := "INSERT INTO teams " +
		"(id, org_id, name, slug, daily_token_limit, monthly_token_limit, requests_per_minute, requests_per_day, created_at, updated_at) " +
		"VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " +
		p(5) + ", " + p(6) + ", " + p(7) + ", " + p(8) + ", " +
		"CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"

	selectQuery := "SELECT " + teamSelectColumns +
		" FROM teams WHERE id = " + p(1) + " AND deleted_at IS NULL"

	var team *Team
	err = d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx, insertQuery,
			id.String(),
			params.OrgID,
			params.Name,
			params.Slug,
			params.DailyTokenLimit,
			params.MonthlyTokenLimit,
			params.RequestsPerMinute,
			params.RequestsPerDay,
		)
		if execErr != nil {
			return translateError(execErr)
		}

		row := q.QueryRowContext(ctx, selectQuery, id.String())
		var scanErr error
		team, scanErr = scanTeam(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("create team: %w", err)
	}
	return team, nil
}

// GetTeam retrieves an active team by its ID.
// It returns ErrNotFound if the team does not exist or has been soft-deleted.
func (d *DB) GetTeam(ctx context.Context, id string) (*Team, error) {
	query := "SELECT " + teamSelectColumns +
		" FROM teams WHERE id = " + d.dialect.Placeholder(1) + " AND deleted_at IS NULL"

	row := d.sql.QueryRowContext(ctx, query, id)
	team, err := scanTeam(row)
	if err != nil {
		return nil, fmt.Errorf("GetTeam %s: %w", id, translateError(err))
	}
	return team, nil
}

// GetTeamByName retrieves an active team within an organization by its display
// name. It returns ErrNotFound if no matching active team exists.
func (d *DB) GetTeamByName(ctx context.Context, orgID, name string) (*Team, error) {
	query := "SELECT " + teamSelectColumns +
		" FROM teams WHERE org_id = " + d.dialect.Placeholder(1) +
		" AND name = " + d.dialect.Placeholder(2) +
		" AND deleted_at IS NULL"

	row := d.sql.QueryRowContext(ctx, query, orgID, name)
	team, err := scanTeam(row)
	if err != nil {
		return nil, fmt.Errorf("GetTeamByName: %w", translateError(err))
	}
	return team, nil
}

// ListTeams returns a page of teams for the given org ordered by ID ascending.
// cursor is an exclusive lower bound on ID for keyset pagination; pass "" to start from the beginning.
// limit controls the maximum number of records returned.
// includeDeleted controls whether soft-deleted teams are included.
func (d *DB) ListTeams(ctx context.Context, orgID string, cursor string, limit int, includeDeleted bool) ([]Team, error) {
	p := d.dialect.Placeholder
	argN := 1
	var conditions []string
	var args []any

	conditions = append(conditions, "org_id = "+p(argN))
	args = append(args, orgID)
	argN++

	if !includeDeleted {
		conditions = append(conditions, "deleted_at IS NULL")
	}
	if cursor != "" {
		conditions = append(conditions, "id > "+p(argN))
		args = append(args, cursor)
		argN++
	}

	query := "SELECT " + teamSelectColumns + " FROM teams"
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY id ASC LIMIT " + p(argN)
	args = append(args, limit)

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ListTeams query: %w", err)
	}
	defer rows.Close()

	var teams []Team
	for rows.Next() {
		var t Team
		if err := rows.Scan(
			&t.ID, &t.OrgID, &t.Name, &t.Slug,
			&t.DailyTokenLimit, &t.MonthlyTokenLimit,
			&t.RequestsPerMinute, &t.RequestsPerDay,
			&t.CreatedAt, &t.UpdatedAt, &t.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListTeams scan: %w", err)
		}
		teams = append(teams, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListTeams rows: %w", err)
	}

	return teams, nil
}

// UpdateTeam applies a partial update to an active team.
// Only non-nil fields in params are written. If all fields are nil the record
// is returned unchanged without issuing an UPDATE.
// It returns ErrNotFound if the team does not exist or has been soft-deleted,
// and ErrConflict if the new slug collides with an existing one in the same org.
func (d *DB) UpdateTeam(ctx context.Context, id string, params UpdateTeamParams) (*Team, error) {
	p := d.dialect.Placeholder
	argN := 1
	var setClauses []string
	var args []any

	if params.Name != nil {
		setClauses = append(setClauses, "name = "+p(argN))
		args = append(args, *params.Name)
		argN++
	}
	if params.Slug != nil {
		setClauses = append(setClauses, "slug = "+p(argN))
		args = append(args, *params.Slug)
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

	if len(setClauses) == 0 {
		return d.GetTeam(ctx, id)
	}

	setClauses = append(setClauses, "updated_at = CURRENT_TIMESTAMP")

	updateQuery := "UPDATE teams SET " + strings.Join(setClauses, ", ") +
		" WHERE id = " + p(argN) + " AND deleted_at IS NULL"
	args = append(args, id)

	selectQuery := "SELECT " + teamSelectColumns +
		" FROM teams WHERE id = " + p(1) + " AND deleted_at IS NULL"

	var team *Team
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
		team, scanErr = scanTeam(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("UpdateTeam %s: %w", id, err)
	}
	return team, nil
}

// DeleteTeam soft-deletes an active team by setting deleted_at.
// It returns ErrNotFound if the team does not exist or is already deleted.
func (d *DB) DeleteTeam(ctx context.Context, id string) error {
	p := d.dialect.Placeholder
	query := "UPDATE teams SET deleted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP " +
		"WHERE id = " + p(1) + " AND deleted_at IS NULL"

	result, err := d.sql.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("DeleteTeam %s: %w", id, translateError(err))
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("DeleteTeam %s rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("DeleteTeam %s: %w", id, ErrNotFound)
	}

	return nil
}

// TeamWithCounts extends Team with computed member and key counts.
type TeamWithCounts struct {
	Team
	MemberCount int
	KeyCount    int
}

// teamWithCountsSelectExpr is the SELECT expression used in all *WithCounts queries.
// It joins team_memberships and api_keys via LEFT JOIN and aggregates counts.
// Placeholder positions are relative: p(1)=expires_at threshold, p(2)=org_id,
// then optional p(3)=cursor, and p(N)=limit for ListTeamsWithCounts; or
// p(1)=expires_at threshold, p(2)=team_id for GetTeamWithCounts.
const teamWithCountsBase = "SELECT t.id, t.org_id, t.name, t.slug, " +
	"t.daily_token_limit, t.monthly_token_limit, " +
	"t.requests_per_minute, t.requests_per_day, " +
	"t.created_at, t.updated_at, t.deleted_at, " +
	"COUNT(DISTINCT tm.user_id) AS member_count, " +
	"COUNT(DISTINCT CASE WHEN ak.deleted_at IS NULL " +
	"AND (ak.expires_at IS NULL OR ak.expires_at > %s) " +
	"THEN ak.id END) AS key_count " +
	"FROM teams t " +
	"LEFT JOIN team_memberships tm ON tm.team_id = t.id " +
	"LEFT JOIN api_keys ak ON ak.team_id = t.id"

// scanTeamWithCounts scans a single row from a *WithCounts query into a TeamWithCounts.
func scanTeamWithCounts(rows interface {
	Scan(...any) error
}) (*TeamWithCounts, error) {
	var t TeamWithCounts
	err := rows.Scan(
		&t.ID, &t.OrgID, &t.Name, &t.Slug,
		&t.DailyTokenLimit, &t.MonthlyTokenLimit,
		&t.RequestsPerMinute, &t.RequestsPerDay,
		&t.CreatedAt, &t.UpdatedAt, &t.DeletedAt,
		&t.MemberCount, &t.KeyCount,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListTeamsWithCounts returns paginated teams for the given org with member and
// active key counts computed in a single query via LEFT JOIN aggregation.
// cursor is an exclusive lower bound on ID for keyset pagination; pass "" to start
// from the beginning. limit controls the maximum number of records returned.
// includeDeleted controls whether soft-deleted teams are included.
func (d *DB) ListTeamsWithCounts(ctx context.Context, orgID string, cursor string, limit int, includeDeleted bool) ([]TeamWithCounts, error) {
	p := d.dialect.Placeholder
	argN := 1
	now := time.Now().UTC().Format(time.RFC3339)

	// p(1) is always the expires_at threshold used inside the CASE expression.
	args := []any{now}
	argN++ // argN=2

	var conditions []string
	conditions = append(conditions, "t.org_id = "+p(argN))
	args = append(args, orgID)
	argN++

	if !includeDeleted {
		conditions = append(conditions, "t.deleted_at IS NULL")
	}
	if cursor != "" {
		conditions = append(conditions, "t.id > "+p(argN))
		args = append(args, cursor)
		argN++
	}

	query := fmt.Sprintf(teamWithCountsBase, p(1))
	query += " WHERE " + strings.Join(conditions, " AND ")
	query += " GROUP BY t.id ORDER BY t.id ASC LIMIT " + p(argN)
	args = append(args, limit)

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ListTeamsWithCounts query: %w", err)
	}
	defer rows.Close()

	var teams []TeamWithCounts
	for rows.Next() {
		t, scanErr := scanTeamWithCounts(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("ListTeamsWithCounts scan: %w", scanErr)
		}
		teams = append(teams, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListTeamsWithCounts rows: %w", err)
	}

	return teams, nil
}

// GetTeamWithCounts retrieves an active team by its ID along with its member and
// active key counts computed in a single query via LEFT JOIN aggregation.
// It returns ErrNotFound if the team does not exist or has been soft-deleted.
func (d *DB) GetTeamWithCounts(ctx context.Context, id string) (*TeamWithCounts, error) {
	p := d.dialect.Placeholder
	now := time.Now().UTC().Format(time.RFC3339)

	// p(1)=expires_at threshold, p(2)=team id.
	query := fmt.Sprintf(teamWithCountsBase, p(1)) +
		" WHERE t.id = " + p(2) + " AND t.deleted_at IS NULL" +
		" GROUP BY t.id"

	row := d.sql.QueryRowContext(ctx, query, now, id)
	t, err := scanTeamWithCounts(row)
	if err != nil {
		return nil, fmt.Errorf("GetTeamWithCounts %s: %w", id, translateError(err))
	}
	return t, nil
}

// ListUserTeams returns all active teams within an org that the given user is a
// member of, joined via team_memberships. Results are ordered by team ID ascending.
// It is used to enforce team-scoped visibility for team_admin callers.
func (d *DB) ListUserTeams(ctx context.Context, orgID, userID string) ([]TeamWithCounts, error) {
	p := d.dialect.Placeholder
	now := time.Now().UTC().Format(time.RFC3339)

	// p(1)=expires_at threshold, p(2)=org_id, p(3)=user_id.
	query := fmt.Sprintf(teamWithCountsBase, p(1)) +
		" INNER JOIN team_memberships tm2 ON tm2.team_id = t.id AND tm2.user_id = " + p(3) +
		" WHERE t.org_id = " + p(2) + " AND t.deleted_at IS NULL" +
		" GROUP BY t.id ORDER BY t.id ASC"

	rows, err := d.sql.QueryContext(ctx, query, now, orgID, userID)
	if err != nil {
		return nil, fmt.Errorf("ListUserTeams query: %w", err)
	}
	defer rows.Close()

	var teams []TeamWithCounts
	for rows.Next() {
		t, scanErr := scanTeamWithCounts(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("ListUserTeams scan: %w", scanErr)
		}
		teams = append(teams, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListUserTeams rows: %w", err)
	}

	return teams, nil
}

// scanTeam scans a single team row returned by QueryRowContext.
func scanTeam(row *sql.Row) (*Team, error) {
	var t Team
	err := row.Scan(
		&t.ID, &t.OrgID, &t.Name, &t.Slug,
		&t.DailyTokenLimit, &t.MonthlyTokenLimit,
		&t.RequestsPerMinute, &t.RequestsPerDay,
		&t.CreatedAt, &t.UpdatedAt, &t.DeletedAt,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
