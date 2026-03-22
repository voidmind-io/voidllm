package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// serviceAccountSelectColumns is the ordered column list used in all service account SELECT queries.
// It must match the scan order in scanServiceAccount.
const serviceAccountSelectColumns = "id, name, org_id, team_id, created_by, created_at, updated_at, deleted_at"

// ServiceAccount represents a service account record in the database.
type ServiceAccount struct {
	ID        string
	Name      string
	OrgID     string
	TeamID    *string
	CreatedBy string
	CreatedAt string
	UpdatedAt string
	DeletedAt *string
}

// CreateServiceAccountParams holds the input for creating a service account.
type CreateServiceAccountParams struct {
	Name      string
	OrgID     string
	TeamID    *string
	CreatedBy string
}

// UpdateServiceAccountParams holds optional fields for updating a service account.
// A nil pointer means the field is not changed.
type UpdateServiceAccountParams struct {
	Name *string
}

// CreateServiceAccount inserts a new service account and returns the persisted record.
func (d *DB) CreateServiceAccount(ctx context.Context, params CreateServiceAccountParams) (*ServiceAccount, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("create service account: generate id: %w", err)
	}

	p := d.dialect.Placeholder
	insertQuery := "INSERT INTO service_accounts " +
		"(id, name, org_id, team_id, created_by, created_at, updated_at) " +
		"VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " + p(5) + ", " +
		"CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"

	selectQuery := "SELECT " + serviceAccountSelectColumns +
		" FROM service_accounts WHERE id = " + p(1) + " AND deleted_at IS NULL"

	var sa *ServiceAccount
	err = d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx, insertQuery,
			id.String(),
			params.Name,
			params.OrgID,
			params.TeamID,
			params.CreatedBy,
		)
		if execErr != nil {
			return translateError(execErr)
		}

		row := q.QueryRowContext(ctx, selectQuery, id.String())
		var scanErr error
		sa, scanErr = scanServiceAccount(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("create service account: %w", err)
	}
	return sa, nil
}

// GetServiceAccount retrieves an active service account by its ID.
// It returns ErrNotFound if the service account does not exist or has been soft-deleted.
func (d *DB) GetServiceAccount(ctx context.Context, id string) (*ServiceAccount, error) {
	query := "SELECT " + serviceAccountSelectColumns +
		" FROM service_accounts WHERE id = " + d.dialect.Placeholder(1) + " AND deleted_at IS NULL"

	row := d.sql.QueryRowContext(ctx, query, id)
	sa, err := scanServiceAccount(row)
	if err != nil {
		return nil, fmt.Errorf("GetServiceAccount %s: %w", id, translateError(err))
	}
	return sa, nil
}

// ListServiceAccounts returns a page of service accounts for the given org ordered by ID ascending.
// cursor is an exclusive lower bound on ID for keyset pagination; pass "" to start from the beginning.
// createdBy restricts results to service accounts created by that user ID; pass "" for no filter.
// limit controls the maximum number of records returned.
// includeDeleted controls whether soft-deleted service accounts are included.
func (d *DB) ListServiceAccounts(ctx context.Context, orgID, createdBy, cursor string, limit int, includeDeleted bool) ([]ServiceAccount, error) {
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
	if createdBy != "" {
		conditions = append(conditions, "created_by = "+p(argN))
		args = append(args, createdBy)
		argN++
	}
	if cursor != "" {
		conditions = append(conditions, "id > "+p(argN))
		args = append(args, cursor)
		argN++
	}

	query := "SELECT " + serviceAccountSelectColumns + " FROM service_accounts"
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY id ASC LIMIT " + p(argN)
	args = append(args, limit)

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ListServiceAccounts query: %w", err)
	}
	defer rows.Close()

	var accounts []ServiceAccount
	for rows.Next() {
		var sa ServiceAccount
		if err := rows.Scan(
			&sa.ID, &sa.Name, &sa.OrgID, &sa.TeamID,
			&sa.CreatedBy, &sa.CreatedAt, &sa.UpdatedAt, &sa.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListServiceAccounts scan: %w", err)
		}
		accounts = append(accounts, sa)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListServiceAccounts rows: %w", err)
	}

	return accounts, nil
}

// UpdateServiceAccount applies a partial update to an active service account.
// Only non-nil fields in params are written. If all fields are nil the record
// is returned unchanged without issuing an UPDATE.
// It returns ErrNotFound if the service account does not exist or has been soft-deleted.
func (d *DB) UpdateServiceAccount(ctx context.Context, id string, params UpdateServiceAccountParams) (*ServiceAccount, error) {
	p := d.dialect.Placeholder
	argN := 1
	var setClauses []string
	var args []any

	if params.Name != nil {
		setClauses = append(setClauses, "name = "+p(argN))
		args = append(args, *params.Name)
		argN++
	}

	if len(setClauses) == 0 {
		return d.GetServiceAccount(ctx, id)
	}

	setClauses = append(setClauses, "updated_at = CURRENT_TIMESTAMP")

	updateQuery := "UPDATE service_accounts SET " + strings.Join(setClauses, ", ") +
		" WHERE id = " + p(argN) + " AND deleted_at IS NULL"
	args = append(args, id)

	selectQuery := "SELECT " + serviceAccountSelectColumns +
		" FROM service_accounts WHERE id = " + p(1) + " AND deleted_at IS NULL"

	var sa *ServiceAccount
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
		sa, scanErr = scanServiceAccount(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("UpdateServiceAccount %s: %w", id, err)
	}
	return sa, nil
}

// UpdateServiceAccountWithCounts applies a partial update to an active service account
// and returns the updated record with its active key count, all within a single transaction.
// Only non-nil fields in params are written. If all fields are nil the record is returned
// unchanged without issuing an UPDATE.
// It returns ErrNotFound if the service account does not exist or has been soft-deleted.
func (d *DB) UpdateServiceAccountWithCounts(ctx context.Context, id string, params UpdateServiceAccountParams) (*ServiceAccountWithCounts, error) {
	p := d.dialect.Placeholder
	argN := 1
	var setClauses []string
	var args []any

	if params.Name != nil {
		setClauses = append(setClauses, "name = "+p(argN))
		args = append(args, *params.Name)
		argN++
	}

	if len(setClauses) == 0 {
		return d.GetServiceAccountWithCounts(ctx, id)
	}

	setClauses = append(setClauses, "updated_at = CURRENT_TIMESTAMP")

	updateQuery := "UPDATE service_accounts SET " + strings.Join(setClauses, ", ") +
		" WHERE id = " + p(argN) + " AND deleted_at IS NULL"
	args = append(args, id)

	now := time.Now().UTC().Format(time.RFC3339)
	// p(1)=expires_at threshold, p(2)=service account id for the WithCounts SELECT.
	selectQuery := fmt.Sprintf(saWithCountsBase, p(1)) +
		" WHERE sa.id = " + p(2) + " AND sa.deleted_at IS NULL" +
		" GROUP BY sa.id"

	var sa *ServiceAccountWithCounts
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

		row := q.QueryRowContext(ctx, selectQuery, now, id)
		var scanErr error
		sa, scanErr = scanServiceAccountWithCounts(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("UpdateServiceAccountWithCounts %s: %w", id, err)
	}
	return sa, nil
}

// DeleteServiceAccount soft-deletes an active service account by setting deleted_at.
// It returns ErrNotFound if the service account does not exist or is already deleted.
func (d *DB) DeleteServiceAccount(ctx context.Context, id string) error {
	p := d.dialect.Placeholder
	query := "UPDATE service_accounts SET deleted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP " +
		"WHERE id = " + p(1) + " AND deleted_at IS NULL"

	result, err := d.sql.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("DeleteServiceAccount %s: %w", id, translateError(err))
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("DeleteServiceAccount %s rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("DeleteServiceAccount %s: %w", id, ErrNotFound)
	}

	return nil
}

// ServiceAccountWithCounts extends ServiceAccount with an active key count.
type ServiceAccountWithCounts struct {
	ServiceAccount
	KeyCount int
}

// saWithCountsBase is the SELECT expression used in all *WithCounts SA queries.
// It joins api_keys via LEFT JOIN and aggregates the active key count.
// The %s token is replaced with the placeholder for the expires_at threshold.
const saWithCountsBase = "SELECT sa.id, sa.name, sa.org_id, sa.team_id, " +
	"sa.created_by, sa.created_at, sa.updated_at, sa.deleted_at, " +
	"COUNT(DISTINCT CASE WHEN ak.deleted_at IS NULL " +
	"AND (ak.expires_at IS NULL OR ak.expires_at > %s) " +
	"THEN ak.id END) AS key_count " +
	"FROM service_accounts sa " +
	"LEFT JOIN api_keys ak ON ak.service_account_id = sa.id"

// scanServiceAccountWithCounts scans a single row from a *WithCounts query into a ServiceAccountWithCounts.
func scanServiceAccountWithCounts(rows interface {
	Scan(...any) error
}) (*ServiceAccountWithCounts, error) {
	var sa ServiceAccountWithCounts
	err := rows.Scan(
		&sa.ID, &sa.Name, &sa.OrgID, &sa.TeamID,
		&sa.CreatedBy, &sa.CreatedAt, &sa.UpdatedAt, &sa.DeletedAt,
		&sa.KeyCount,
	)
	if err != nil {
		return nil, err
	}
	return &sa, nil
}

// ListServiceAccountsWithCounts returns a page of service accounts for the given
// org with active key counts computed in a single query via LEFT JOIN aggregation.
// cursor is an exclusive lower bound on ID for keyset pagination; pass "" to start
// from the beginning. createdBy restricts results to service accounts created by
// that user ID; pass "" for no filter. limit controls the maximum number of records
// returned. includeDeleted controls whether soft-deleted service accounts are included.
func (d *DB) ListServiceAccountsWithCounts(ctx context.Context, orgID, createdBy, cursor string, limit int, includeDeleted bool) ([]ServiceAccountWithCounts, error) {
	p := d.dialect.Placeholder
	argN := 1
	now := time.Now().UTC().Format(time.RFC3339)

	// p(1) is always the expires_at threshold used inside the CASE expression.
	args := []any{now}
	argN++ // argN=2

	var conditions []string
	conditions = append(conditions, "sa.org_id = "+p(argN))
	args = append(args, orgID)
	argN++

	if !includeDeleted {
		conditions = append(conditions, "sa.deleted_at IS NULL")
	}
	if createdBy != "" {
		conditions = append(conditions, "sa.created_by = "+p(argN))
		args = append(args, createdBy)
		argN++
	}
	if cursor != "" {
		conditions = append(conditions, "sa.id > "+p(argN))
		args = append(args, cursor)
		argN++
	}

	query := fmt.Sprintf(saWithCountsBase, p(1))
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " GROUP BY sa.id ORDER BY sa.id ASC LIMIT " + p(argN)
	args = append(args, limit)

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ListServiceAccountsWithCounts query: %w", err)
	}
	defer rows.Close()

	var accounts []ServiceAccountWithCounts
	for rows.Next() {
		sa, scanErr := scanServiceAccountWithCounts(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("ListServiceAccountsWithCounts scan: %w", scanErr)
		}
		accounts = append(accounts, *sa)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListServiceAccountsWithCounts rows: %w", err)
	}

	return accounts, nil
}

// GetServiceAccountWithCounts retrieves an active service account by its ID along
// with its active key count computed in a single query via LEFT JOIN aggregation.
// It returns ErrNotFound if the service account does not exist or has been soft-deleted.
func (d *DB) GetServiceAccountWithCounts(ctx context.Context, id string) (*ServiceAccountWithCounts, error) {
	p := d.dialect.Placeholder
	now := time.Now().UTC().Format(time.RFC3339)

	// p(1)=expires_at threshold, p(2)=service account id.
	query := fmt.Sprintf(saWithCountsBase, p(1)) +
		" WHERE sa.id = " + p(2) + " AND sa.deleted_at IS NULL" +
		" GROUP BY sa.id"

	row := d.sql.QueryRowContext(ctx, query, now, id)
	sa, err := scanServiceAccountWithCounts(row)
	if err != nil {
		return nil, fmt.Errorf("GetServiceAccountWithCounts %s: %w", id, translateError(err))
	}
	return sa, nil
}

// scanServiceAccount scans a single service account row returned by QueryRowContext.
func scanServiceAccount(row *sql.Row) (*ServiceAccount, error) {
	var sa ServiceAccount
	err := row.Scan(
		&sa.ID, &sa.Name, &sa.OrgID, &sa.TeamID,
		&sa.CreatedBy, &sa.CreatedAt, &sa.UpdatedAt, &sa.DeletedAt,
	)
	if err != nil {
		return nil, err
	}
	return &sa, nil
}
