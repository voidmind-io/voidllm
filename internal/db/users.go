package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// userSelectColumns is the ordered column list used in all user SELECT queries.
// It must match the scan order in scanUser.
// password_hash and external_id are intentionally excluded — they are never
// returned to the API layer.
const userSelectColumns = "id, email, display_name, auth_provider, is_system_admin, " +
	"created_at, updated_at, deleted_at"

// User represents a user record in the database.
// PasswordHash and ExternalID are not included; they are internal fields
// that must never appear in API responses.
type User struct {
	ID            string
	Email         string
	DisplayName   string
	AuthProvider  string
	IsSystemAdmin bool
	CreatedAt     string
	UpdatedAt     string
	DeletedAt     *string
}

// CreateUserParams holds the input for creating a user.
// If AuthProvider is empty it defaults to "local".
// PasswordHash must already be a bcrypt digest when provided.
type CreateUserParams struct {
	Email         string
	DisplayName   string
	PasswordHash  *string
	AuthProvider  string
	ExternalID    *string
	IsSystemAdmin bool
}

// UpdateUserParams holds optional fields for updating a user.
// A nil pointer means the field is not changed.
// PasswordHash, when non-nil, must already be a bcrypt digest.
type UpdateUserParams struct {
	Email         *string
	DisplayName   *string
	PasswordHash  *string
	IsSystemAdmin *bool
}

// CreateUser inserts a new user and returns the persisted record.
// It returns ErrConflict if the email is already taken.
// If params.AuthProvider is empty, "local" is used.
func (d *DB) CreateUser(ctx context.Context, params CreateUserParams) (*User, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("create user: generate id: %w", err)
	}

	authProvider := params.AuthProvider
	if authProvider == "" {
		authProvider = "local"
	}

	p := d.dialect.Placeholder
	insertQuery := "INSERT INTO users " +
		"(id, email, display_name, password_hash, auth_provider, external_id, is_system_admin, created_at, updated_at) " +
		"VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " +
		p(5) + ", " + p(6) + ", " + p(7) + ", " +
		"CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"

	selectQuery := "SELECT " + userSelectColumns +
		" FROM users WHERE id = " + p(1) + " AND deleted_at IS NULL"

	isSystemAdminInt := 0
	if params.IsSystemAdmin {
		isSystemAdminInt = 1
	}

	var user *User
	err = d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx, insertQuery,
			id.String(),
			params.Email,
			params.DisplayName,
			params.PasswordHash,
			authProvider,
			params.ExternalID,
			isSystemAdminInt,
		)
		if execErr != nil {
			return translateError(execErr)
		}

		row := q.QueryRowContext(ctx, selectQuery, id.String())
		var scanErr error
		user, scanErr = scanUser(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return user, nil
}

// GetUser retrieves an active user by their ID.
// It returns ErrNotFound if the user does not exist or has been soft-deleted.
func (d *DB) GetUser(ctx context.Context, id string) (*User, error) {
	query := "SELECT " + userSelectColumns +
		" FROM users WHERE id = " + d.dialect.Placeholder(1) + " AND deleted_at IS NULL"

	row := d.sql.QueryRowContext(ctx, query, id)
	user, err := scanUser(row)
	if err != nil {
		return nil, fmt.Errorf("GetUser %s: %w", id, translateError(err))
	}
	return user, nil
}

// GetUserByExternalID retrieves an active user by their identity provider and
// external subject identifier. provider is the auth_provider value (e.g. "oidc")
// and externalID is the subject claim from the ID token.
// It returns ErrNotFound if no matching active user exists.
func (d *DB) GetUserByExternalID(ctx context.Context, provider, externalID string) (*User, error) {
	query := "SELECT " + userSelectColumns +
		" FROM users WHERE auth_provider = " + d.dialect.Placeholder(1) +
		" AND external_id = " + d.dialect.Placeholder(2) +
		" AND deleted_at IS NULL"

	row := d.sql.QueryRowContext(ctx, query, provider, externalID)
	user, err := scanUser(row)
	if err != nil {
		return nil, fmt.Errorf("GetUserByExternalID: %w", translateError(err))
	}
	return user, nil
}

// GetUserByEmail retrieves an active user by their email address.
// It returns ErrNotFound if no active user with that email exists.
func (d *DB) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	query := "SELECT " + userSelectColumns +
		" FROM users WHERE email = " + d.dialect.Placeholder(1) + " AND deleted_at IS NULL"

	row := d.sql.QueryRowContext(ctx, query, email)
	user, err := scanUser(row)
	if err != nil {
		return nil, fmt.Errorf("GetUserByEmail: %w", translateError(err))
	}
	return user, nil
}

// ListUsers returns a page of users ordered by ID ascending.
// cursor is an exclusive lower bound on ID for keyset pagination; pass "" to start from the beginning.
// limit controls the maximum number of records returned.
// includeDeleted controls whether soft-deleted users are included.
func (d *DB) ListUsers(ctx context.Context, cursor string, limit int, includeDeleted bool) ([]User, error) {
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

	query := "SELECT " + userSelectColumns + " FROM users"
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY id ASC LIMIT " + p(argN)
	args = append(args, limit)

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ListUsers query: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		var isSystemAdminInt int
		if err := rows.Scan(
			&u.ID, &u.Email, &u.DisplayName, &u.AuthProvider,
			&isSystemAdminInt,
			&u.CreatedAt, &u.UpdatedAt, &u.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListUsers scan: %w", err)
		}
		u.IsSystemAdmin = isSystemAdminInt != 0
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListUsers rows: %w", err)
	}

	return users, nil
}

// UpdateUser applies a partial update to an active user.
// Only non-nil fields in params are written. If all fields are nil the record
// is returned unchanged without issuing an UPDATE.
// It returns ErrNotFound if the user does not exist or has been soft-deleted,
// and ErrConflict if the new email collides with an existing one.
func (d *DB) UpdateUser(ctx context.Context, id string, params UpdateUserParams) (*User, error) {
	p := d.dialect.Placeholder
	argN := 1
	var setClauses []string
	var args []any

	if params.Email != nil {
		setClauses = append(setClauses, "email = "+p(argN))
		args = append(args, *params.Email)
		argN++
	}
	if params.DisplayName != nil {
		setClauses = append(setClauses, "display_name = "+p(argN))
		args = append(args, *params.DisplayName)
		argN++
	}
	if params.PasswordHash != nil {
		setClauses = append(setClauses, "password_hash = "+p(argN))
		args = append(args, *params.PasswordHash)
		argN++
	}
	if params.IsSystemAdmin != nil {
		isAdminInt := 0
		if *params.IsSystemAdmin {
			isAdminInt = 1
		}
		setClauses = append(setClauses, "is_system_admin = "+p(argN))
		args = append(args, isAdminInt)
		argN++
	}

	if len(setClauses) == 0 {
		return d.GetUser(ctx, id)
	}

	setClauses = append(setClauses, "updated_at = CURRENT_TIMESTAMP")

	updateQuery := "UPDATE users SET " + strings.Join(setClauses, ", ") +
		" WHERE id = " + p(argN) + " AND deleted_at IS NULL"
	args = append(args, id)

	selectQuery := "SELECT " + userSelectColumns +
		" FROM users WHERE id = " + p(1) + " AND deleted_at IS NULL"

	var user *User
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
		user, scanErr = scanUser(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("UpdateUser %s: %w", id, err)
	}
	return user, nil
}

// DeleteUser soft-deletes an active user by setting deleted_at.
// It returns ErrNotFound if the user does not exist or is already deleted.
func (d *DB) DeleteUser(ctx context.Context, id string) error {
	p := d.dialect.Placeholder
	query := "UPDATE users SET deleted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP " +
		"WHERE id = " + p(1) + " AND deleted_at IS NULL"

	result, err := d.sql.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("DeleteUser %s: %w", id, translateError(err))
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("DeleteUser %s rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("DeleteUser %s: %w", id, ErrNotFound)
	}

	return nil
}

// GetUserPasswordHash retrieves the user ID and bcrypt password hash for login
// verification. Returns ErrNotFound if the user does not exist or is deleted.
// Returns ErrNoPassword if password_hash is NULL, indicating an SSO-only account.
func (d *DB) GetUserPasswordHash(ctx context.Context, email string) (string, string, error) {
	query := "SELECT id, password_hash FROM users WHERE email = " +
		d.dialect.Placeholder(1) + " AND deleted_at IS NULL"

	var id string
	var passwordHash *string
	err := d.sql.QueryRowContext(ctx, query, email).Scan(&id, &passwordHash)
	if err != nil {
		return "", "", fmt.Errorf("GetUserPasswordHash: %w", translateError(err))
	}
	if passwordHash == nil {
		return "", "", ErrNoPassword
	}
	return id, *passwordHash, nil
}

// ResolveUserRole determines the effective RBAC role and organization for a user.
// If the user is a system admin, it returns RoleSystemAdmin with the first org (if any).
// Otherwise, it returns the role from the user's first org membership.
// Returns ErrNotFound if the user has no org membership and is not a system admin.
func (d *DB) ResolveUserRole(ctx context.Context, userID string) (role string, orgID string, err error) {
	p := d.dialect.Placeholder

	var isAdmin int
	err = d.sql.QueryRowContext(ctx,
		"SELECT is_system_admin FROM users WHERE id = "+p(1)+" AND deleted_at IS NULL",
		userID,
	).Scan(&isAdmin)
	if err != nil {
		return "", "", fmt.Errorf("ResolveUserRole get admin flag: %w", translateError(err))
	}

	if isAdmin == 1 {
		// Best-effort: pick the first org this admin belongs to, but do not
		// fail if they have none (system admins are not required to be members).
		var firstOrg string
		scanErr := d.sql.QueryRowContext(ctx,
			"SELECT org_id FROM org_memberships WHERE user_id = "+p(1)+" LIMIT 1",
			userID,
		).Scan(&firstOrg)
		if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
			return "", "", fmt.Errorf("ResolveUserRole get admin org: %w", scanErr)
		}
		return "system_admin", firstOrg, nil
	}

	var memberOrgID, memberRole string
	scanErr := d.sql.QueryRowContext(ctx,
		"SELECT org_id, role FROM org_memberships WHERE user_id = "+p(1)+" LIMIT 1",
		userID,
	).Scan(&memberOrgID, &memberRole)
	if scanErr != nil {
		return "", "", fmt.Errorf("ResolveUserRole get membership: %w", translateError(scanErr))
	}
	return memberRole, memberOrgID, nil
}

// scanUser scans a single user row returned by QueryRowContext.
// It handles the is_system_admin INTEGER→bool conversion for SQLite compatibility.
func scanUser(row *sql.Row) (*User, error) {
	var u User
	var isSystemAdminInt int
	err := row.Scan(
		&u.ID, &u.Email, &u.DisplayName, &u.AuthProvider,
		&isSystemAdminInt,
		&u.CreatedAt, &u.UpdatedAt, &u.DeletedAt,
	)
	if err != nil {
		return nil, err
	}
	u.IsSystemAdmin = isSystemAdminInt != 0
	return &u, nil
}
