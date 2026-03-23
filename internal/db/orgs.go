package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// orgSelectColumns is the ordered column list used in all org SELECT queries.
// It must match the scan order in scanOrg.
const orgSelectColumns = "id, name, slug, timezone, daily_token_limit, monthly_token_limit, " +
	"requests_per_minute, requests_per_day, created_at, updated_at, deleted_at"

// Org represents an organization record in the database.
type Org struct {
	ID                string
	Name              string
	Slug              string
	Timezone          *string
	DailyTokenLimit   int64
	MonthlyTokenLimit int64
	RequestsPerMinute int
	RequestsPerDay    int
	CreatedAt         string
	UpdatedAt         string
	DeletedAt         *string
}

// CreateOrgParams holds the input for creating an organization.
type CreateOrgParams struct {
	Name              string
	Slug              string
	Timezone          *string
	DailyTokenLimit   int64
	MonthlyTokenLimit int64
	RequestsPerMinute int
	RequestsPerDay    int
}

// UpdateOrgParams holds optional fields for updating an organization.
// A nil pointer means the field is not changed.
type UpdateOrgParams struct {
	Name              *string
	Slug              *string
	Timezone          *string
	DailyTokenLimit   *int64
	MonthlyTokenLimit *int64
	RequestsPerMinute *int
	RequestsPerDay    *int
}

// CreateOrg inserts a new organization and returns the persisted record.
// It returns ErrConflict if the slug is already taken.
func (d *DB) CreateOrg(ctx context.Context, params CreateOrgParams) (*Org, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("create org: generate id: %w", err)
	}

	p := d.dialect.Placeholder
	insertQuery := "INSERT INTO organizations " +
		"(id, name, slug, timezone, daily_token_limit, monthly_token_limit, requests_per_minute, requests_per_day, created_at, updated_at) " +
		"VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " +
		p(5) + ", " + p(6) + ", " + p(7) + ", " + p(8) + ", " +
		"CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"

	selectQuery := "SELECT " + orgSelectColumns +
		" FROM organizations WHERE id = " + p(1) + " AND deleted_at IS NULL"

	var org *Org
	err = d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx, insertQuery,
			id.String(),
			params.Name,
			params.Slug,
			params.Timezone,
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
		org, scanErr = scanOrg(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("create org: %w", err)
	}
	return org, nil
}

// GetOrg retrieves an active organization by its ID.
// It returns ErrNotFound if the organization does not exist or has been soft-deleted.
func (d *DB) GetOrg(ctx context.Context, id string) (*Org, error) {
	query := "SELECT " + orgSelectColumns +
		" FROM organizations WHERE id = " + d.dialect.Placeholder(1) + " AND deleted_at IS NULL"

	row := d.sql.QueryRowContext(ctx, query, id)
	org, err := scanOrg(row)
	if err != nil {
		return nil, fmt.Errorf("GetOrg %s: %w", id, translateError(err))
	}
	return org, nil
}

// GetOrgBySlug retrieves an active organization by its URL-safe slug.
// It returns ErrNotFound if no matching active organization exists.
func (d *DB) GetOrgBySlug(ctx context.Context, slug string) (*Org, error) {
	query := "SELECT " + orgSelectColumns +
		" FROM organizations WHERE slug = " + d.dialect.Placeholder(1) + " AND deleted_at IS NULL"

	row := d.sql.QueryRowContext(ctx, query, slug)
	org, err := scanOrg(row)
	if err != nil {
		return nil, fmt.Errorf("GetOrgBySlug %q: %w", slug, translateError(err))
	}
	return org, nil
}

// ListOrgs returns a page of organizations ordered by ID ascending.
// cursor is an exclusive lower bound on ID for keyset pagination; pass "" to start from the beginning.
// limit controls the maximum number of records returned.
// includeDeleted controls whether soft-deleted organizations are included.
func (d *DB) ListOrgs(ctx context.Context, cursor string, limit int, includeDeleted bool) ([]Org, error) {
	p := d.dialect.Placeholder
	argN := 1
	var conditions []string
	var args []any

	if !includeDeleted {
		conditions = append(conditions, "deleted_at IS NULL")
	}
	if cursor != "" {
		conditions = append(conditions, "id > "+p(argN))
		args = append(args, cursor)
		argN++
	}

	query := "SELECT " + orgSelectColumns + " FROM organizations"
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY id ASC LIMIT " + p(argN)
	args = append(args, limit)

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ListOrgs query: %w", err)
	}
	defer rows.Close()

	var orgs []Org
	for rows.Next() {
		var o Org
		if err := rows.Scan(
			&o.ID, &o.Name, &o.Slug, &o.Timezone,
			&o.DailyTokenLimit, &o.MonthlyTokenLimit,
			&o.RequestsPerMinute, &o.RequestsPerDay,
			&o.CreatedAt, &o.UpdatedAt, &o.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListOrgs scan: %w", err)
		}
		orgs = append(orgs, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListOrgs rows: %w", err)
	}

	return orgs, nil
}

// UpdateOrg applies a partial update to an active organization.
// Only non-nil fields in params are written. If all fields are nil the record
// is returned unchanged without issuing an UPDATE.
// It returns ErrNotFound if the organization does not exist or has been soft-deleted,
// and ErrConflict if the new slug collides with an existing one.
func (d *DB) UpdateOrg(ctx context.Context, id string, params UpdateOrgParams) (*Org, error) {
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
	if params.Timezone != nil {
		setClauses = append(setClauses, "timezone = "+p(argN))
		args = append(args, *params.Timezone)
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
		return d.GetOrg(ctx, id)
	}

	setClauses = append(setClauses, "updated_at = CURRENT_TIMESTAMP")

	updateQuery := "UPDATE organizations SET " + strings.Join(setClauses, ", ") +
		" WHERE id = " + p(argN) + " AND deleted_at IS NULL"
	args = append(args, id)

	selectQuery := "SELECT " + orgSelectColumns +
		" FROM organizations WHERE id = " + p(1) + " AND deleted_at IS NULL"

	var org *Org
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
		org, scanErr = scanOrg(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("UpdateOrg %s: %w", id, err)
	}
	return org, nil
}

// DeleteOrg soft-deletes an active organization by setting deleted_at.
// It returns ErrNotFound if the organization does not exist or is already deleted.
func (d *DB) DeleteOrg(ctx context.Context, id string) error {
	p := d.dialect.Placeholder
	query := "UPDATE organizations SET deleted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP " +
		"WHERE id = " + p(1) + " AND deleted_at IS NULL"

	result, err := d.sql.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("DeleteOrg %s: %w", id, translateError(err))
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("DeleteOrg %s rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("DeleteOrg %s: %w", id, ErrNotFound)
	}

	return nil
}

// scanOrg scans a single organization row returned by QueryRowContext.
func scanOrg(row *sql.Row) (*Org, error) {
	var o Org
	err := row.Scan(
		&o.ID, &o.Name, &o.Slug, &o.Timezone,
		&o.DailyTokenLimit, &o.MonthlyTokenLimit,
		&o.RequestsPerMinute, &o.RequestsPerDay,
		&o.CreatedAt, &o.UpdatedAt, &o.DeletedAt,
	)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// OrgWithCounts extends Org with computed member and team counts.
type OrgWithCounts struct {
	Org
	MemberCount int
	TeamCount   int
}

// orgWithCountsBase is the SELECT expression used in all *WithCounts org queries.
// Counts are computed via correlated subqueries. Placeholder positions are
// dialect-specific: p(1)=cursor (optional), p(N)=limit for ListOrgsWithCounts;
// or p(1)=org id for GetOrgWithCounts.
const orgWithCountsBase = "SELECT o.id, o.name, o.slug, o.timezone, " +
	"o.daily_token_limit, o.monthly_token_limit, " +
	"o.requests_per_minute, o.requests_per_day, " +
	"o.created_at, o.updated_at, o.deleted_at, " +
	"(SELECT COUNT(*) FROM org_memberships WHERE org_id = o.id) AS member_count, " +
	"(SELECT COUNT(*) FROM teams WHERE org_id = o.id AND deleted_at IS NULL) AS team_count " +
	"FROM organizations o"

// scanOrgWithCounts scans a single row from a *WithCounts query into an OrgWithCounts.
func scanOrgWithCounts(rows interface {
	Scan(...any) error
}) (*OrgWithCounts, error) {
	var o OrgWithCounts
	err := rows.Scan(
		&o.ID, &o.Name, &o.Slug, &o.Timezone,
		&o.DailyTokenLimit, &o.MonthlyTokenLimit,
		&o.RequestsPerMinute, &o.RequestsPerDay,
		&o.CreatedAt, &o.UpdatedAt, &o.DeletedAt,
		&o.MemberCount, &o.TeamCount,
	)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// ListOrgsWithCounts returns a page of organizations ordered by ID ascending,
// each augmented with its total member count and active team count computed via
// correlated subqueries. cursor is an exclusive lower bound on ID for keyset
// pagination; pass "" to start from the beginning. limit controls the maximum
// number of records returned. includeDeleted controls whether soft-deleted
// organizations are included.
func (d *DB) ListOrgsWithCounts(ctx context.Context, cursor string, limit int, includeDeleted bool) ([]OrgWithCounts, error) {
	p := d.dialect.Placeholder
	argN := 1
	var conditions []string
	var args []any

	if !includeDeleted {
		conditions = append(conditions, "o.deleted_at IS NULL")
	}
	if cursor != "" {
		conditions = append(conditions, "o.id > "+p(argN))
		args = append(args, cursor)
		argN++
	}

	query := orgWithCountsBase
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY o.id ASC LIMIT " + p(argN)
	args = append(args, limit)

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ListOrgsWithCounts query: %w", err)
	}
	defer rows.Close()

	var orgs []OrgWithCounts
	for rows.Next() {
		o, scanErr := scanOrgWithCounts(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("ListOrgsWithCounts scan: %w", scanErr)
		}
		orgs = append(orgs, *o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListOrgsWithCounts rows: %w", err)
	}

	return orgs, nil
}

// GetOrgWithCounts retrieves an active organization by its ID along with its
// total member count and active team count computed via correlated subqueries.
// It returns ErrNotFound if the organization does not exist or has been soft-deleted.
func (d *DB) GetOrgWithCounts(ctx context.Context, id string) (*OrgWithCounts, error) {
	p := d.dialect.Placeholder

	// p(1)=org id.
	query := orgWithCountsBase + " WHERE o.id = " + p(1) + " AND o.deleted_at IS NULL"

	row := d.sql.QueryRowContext(ctx, query, id)
	o, err := scanOrgWithCounts(row)
	if err != nil {
		return nil, fmt.Errorf("GetOrgWithCounts %s: %w", id, translateError(err))
	}
	return o, nil
}
