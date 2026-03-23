package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// apiKeySelectColumns is the ordered column list used in all api_keys SELECT queries.
// It must match the scan order in scanAPIKey exactly.
// key_hash is included for cache population; it must never be exposed in API responses.
const apiKeySelectColumns = "id, key_hash, key_hint, key_type, name, " +
	"org_id, team_id, user_id, service_account_id, " +
	"daily_token_limit, monthly_token_limit, requests_per_minute, requests_per_day, " +
	"expires_at, last_used_at, created_by, created_at, updated_at, deleted_at"

// APIKey represents an API key record in the database.
// KeyHash is included for cache keying and must never be included in API responses.
type APIKey struct {
	ID                string
	KeyHash           string
	KeyHint           string
	KeyType           string
	Name              string
	OrgID             string
	TeamID            *string
	UserID            *string
	ServiceAccountID  *string
	DailyTokenLimit   int64
	MonthlyTokenLimit int64
	RequestsPerMinute int
	RequestsPerDay    int
	ExpiresAt         *string
	LastUsedAt        *string
	CreatedBy         string
	CreatedAt         string
	UpdatedAt         string
	DeletedAt         *string
}

// CreateAPIKeyParams holds the input for creating an API key.
type CreateAPIKeyParams struct {
	// KeyHash is the HMAC-SHA256 hash of the raw key. Never store the raw key.
	KeyHash           string
	KeyHint           string
	KeyType           string
	Name              string
	OrgID             string
	TeamID            *string
	UserID            *string
	ServiceAccountID  *string
	DailyTokenLimit   int64
	MonthlyTokenLimit int64
	RequestsPerMinute int
	RequestsPerDay    int
	ExpiresAt         *string
	CreatedBy         string
}

// UpdateAPIKeyParams holds optional fields for updating an API key.
// A nil pointer means the field is not changed.
type UpdateAPIKeyParams struct {
	Name              *string
	DailyTokenLimit   *int64
	MonthlyTokenLimit *int64
	RequestsPerMinute *int
	RequestsPerDay    *int
	ExpiresAt         *string
}

// CreateAPIKey inserts a new API key and returns the persisted record.
func (d *DB) CreateAPIKey(ctx context.Context, params CreateAPIKeyParams) (*APIKey, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("create api key: generate id: %w", err)
	}

	p := d.dialect.Placeholder
	insertQuery := "INSERT INTO api_keys " +
		"(id, key_hash, key_hint, key_type, name, " +
		"org_id, team_id, user_id, service_account_id, " +
		"daily_token_limit, monthly_token_limit, requests_per_minute, requests_per_day, " +
		"expires_at, created_by, created_at, updated_at) " +
		"VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " + p(5) + ", " +
		p(6) + ", " + p(7) + ", " + p(8) + ", " + p(9) + ", " +
		p(10) + ", " + p(11) + ", " + p(12) + ", " + p(13) + ", " +
		p(14) + ", " + p(15) + ", " +
		"CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"

	selectQuery := "SELECT " + apiKeySelectColumns +
		" FROM api_keys WHERE id = " + p(1) + " AND deleted_at IS NULL"

	var key *APIKey
	err = d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx, insertQuery,
			id.String(),
			params.KeyHash,
			params.KeyHint,
			params.KeyType,
			params.Name,
			params.OrgID,
			params.TeamID,
			params.UserID,
			params.ServiceAccountID,
			params.DailyTokenLimit,
			params.MonthlyTokenLimit,
			params.RequestsPerMinute,
			params.RequestsPerDay,
			params.ExpiresAt,
			params.CreatedBy,
		)
		if execErr != nil {
			return translateError(execErr)
		}

		row := q.QueryRowContext(ctx, selectQuery, id.String())
		var scanErr error
		key, scanErr = scanAPIKey(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("create api key: %w", err)
	}
	return key, nil
}

// GetAPIKey retrieves an active API key by its ID.
// It returns ErrNotFound if the key does not exist or has been soft-deleted.
func (d *DB) GetAPIKey(ctx context.Context, id string) (*APIKey, error) {
	query := "SELECT " + apiKeySelectColumns +
		" FROM api_keys WHERE id = " + d.dialect.Placeholder(1) + " AND deleted_at IS NULL"

	row := d.sql.QueryRowContext(ctx, query, id)
	key, err := scanAPIKey(row)
	if err != nil {
		return nil, fmt.Errorf("get api key %s: %w", id, translateError(err))
	}
	return key, nil
}

// ListAPIKeys returns a page of API keys for the given org, ordered by ID ascending.
// cursor is an exclusive lower bound on ID for keyset pagination; pass "" to start from the beginning.
// limit controls the maximum number of records returned.
// includeDeleted controls whether soft-deleted keys are included.
// userID and teamID are optional equality filters; an empty string means no filter is applied.
func (d *DB) ListAPIKeys(ctx context.Context, orgID, userID, teamID, cursor string, limit int, includeDeleted bool) ([]APIKey, error) {
	p := d.dialect.Placeholder
	argN := 1
	var conditions []string
	var args []any

	conditions = append(conditions, "org_id = "+p(argN))
	args = append(args, orgID)
	argN++

	// Session keys are internal (login tokens) — never expose them in the API list.
	conditions = append(conditions, "key_type != 'session_key'")

	if !includeDeleted {
		conditions = append(conditions, "deleted_at IS NULL")
	}
	if userID != "" {
		conditions = append(conditions, "user_id = "+p(argN))
		args = append(args, userID)
		argN++
	}
	if teamID != "" {
		conditions = append(conditions, "team_id = "+p(argN))
		args = append(args, teamID)
		argN++
	}
	if cursor != "" {
		conditions = append(conditions, "id > "+p(argN))
		args = append(args, cursor)
		argN++
	}

	query := "SELECT " + apiKeySelectColumns + " FROM api_keys" +
		" WHERE " + strings.Join(conditions, " AND ") +
		" ORDER BY id ASC LIMIT " + p(argN)
	args = append(args, limit)

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list api keys query: %w", err)
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(
			&k.ID, &k.KeyHash, &k.KeyHint, &k.KeyType, &k.Name,
			&k.OrgID, &k.TeamID, &k.UserID, &k.ServiceAccountID,
			&k.DailyTokenLimit, &k.MonthlyTokenLimit,
			&k.RequestsPerMinute, &k.RequestsPerDay,
			&k.ExpiresAt, &k.LastUsedAt,
			&k.CreatedBy, &k.CreatedAt, &k.UpdatedAt, &k.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("list api keys scan: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list api keys rows: %w", err)
	}

	return keys, nil
}

// UpdateAPIKey applies a partial update to an active API key.
// Only non-nil fields in params are written. If all fields are nil the record
// is returned unchanged without issuing an UPDATE.
// It returns ErrNotFound if the key does not exist or has been soft-deleted.
func (d *DB) UpdateAPIKey(ctx context.Context, id string, params UpdateAPIKeyParams) (*APIKey, error) {
	p := d.dialect.Placeholder
	argN := 1
	var setClauses []string
	var args []any

	if params.Name != nil {
		setClauses = append(setClauses, "name = "+p(argN))
		args = append(args, *params.Name)
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
	if params.ExpiresAt != nil {
		setClauses = append(setClauses, "expires_at = "+p(argN))
		args = append(args, *params.ExpiresAt)
		argN++
	}

	if len(setClauses) == 0 {
		return d.GetAPIKey(ctx, id)
	}

	setClauses = append(setClauses, "updated_at = CURRENT_TIMESTAMP")

	updateQuery := "UPDATE api_keys SET " + strings.Join(setClauses, ", ") +
		" WHERE id = " + p(argN) + " AND deleted_at IS NULL"
	args = append(args, id)

	selectQuery := "SELECT " + apiKeySelectColumns +
		" FROM api_keys WHERE id = " + p(1) + " AND deleted_at IS NULL"

	var key *APIKey
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
		key, scanErr = scanAPIKey(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("update api key %s: %w", id, err)
	}
	return key, nil
}

// DeleteAPIKey soft-deletes an active API key by setting deleted_at.
// It returns ErrNotFound if the key does not exist or is already deleted.
// RevokeUserSessions hard-deletes all session keys for a user.
// Called during login to ensure only one session exists per user.
// Session keys are ephemeral — no audit trail needed, hard delete
// prevents the api_keys table from filling up with dead sessions.
func (d *DB) RevokeUserSessions(ctx context.Context, userID string) error {
	p := d.dialect.Placeholder
	query := "DELETE FROM api_keys WHERE user_id = " + p(1) + " AND key_type = " + p(2)

	_, err := d.sql.ExecContext(ctx, query, userID, "session_key")
	if err != nil {
		return fmt.Errorf("revoke user sessions %s: %w", userID, translateError(err))
	}
	return nil
}

func (d *DB) DeleteAPIKey(ctx context.Context, id string) error {
	p := d.dialect.Placeholder
	query := "UPDATE api_keys SET deleted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP " +
		"WHERE id = " + p(1) + " AND deleted_at IS NULL"

	result, err := d.sql.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("delete api key %s: %w", id, translateError(err))
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete api key %s rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("delete api key %s: %w", id, ErrNotFound)
	}

	return nil
}

// RotateKeyTxResult holds both records produced by RotateKeyTx.
type RotateKeyTxResult struct {
	NewKey *APIKey
	OldKey *APIKey
}

// RotateKeyTx atomically inserts a new API key and updates the old key's expiry
// in a single transaction. newParams describes the replacement key; oldID is the
// ID of the key being rotated; oldExpiresAt is the grace-period deadline to write
// onto the old row. Both records are returned on success.
func (d *DB) RotateKeyTx(ctx context.Context, oldID string, oldExpiresAt string, newParams CreateAPIKeyParams) (*RotateKeyTxResult, error) {
	newID, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("rotate key tx: generate id: %w", err)
	}

	p := d.dialect.Placeholder

	insertQuery := "INSERT INTO api_keys " +
		"(id, key_hash, key_hint, key_type, name, " +
		"org_id, team_id, user_id, service_account_id, " +
		"daily_token_limit, monthly_token_limit, requests_per_minute, requests_per_day, " +
		"expires_at, created_by, created_at, updated_at) " +
		"VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " + p(5) + ", " +
		p(6) + ", " + p(7) + ", " + p(8) + ", " + p(9) + ", " +
		p(10) + ", " + p(11) + ", " + p(12) + ", " + p(13) + ", " +
		p(14) + ", " + p(15) + ", " +
		"CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"

	updateQuery := "UPDATE api_keys SET expires_at = " + p(1) + ", updated_at = CURRENT_TIMESTAMP" +
		" WHERE id = " + p(2) + " AND deleted_at IS NULL"

	selectQuery := "SELECT " + apiKeySelectColumns +
		" FROM api_keys WHERE id = " + p(1)

	var result RotateKeyTxResult
	txErr := d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx, insertQuery,
			newID.String(),
			newParams.KeyHash,
			newParams.KeyHint,
			newParams.KeyType,
			newParams.Name,
			newParams.OrgID,
			newParams.TeamID,
			newParams.UserID,
			newParams.ServiceAccountID,
			newParams.DailyTokenLimit,
			newParams.MonthlyTokenLimit,
			newParams.RequestsPerMinute,
			newParams.RequestsPerDay,
			newParams.ExpiresAt,
			newParams.CreatedBy,
		)
		if execErr != nil {
			return fmt.Errorf("insert new key: %w", translateError(execErr))
		}

		updateResult, execErr := q.ExecContext(ctx, updateQuery, oldExpiresAt, oldID)
		if execErr != nil {
			return fmt.Errorf("update old key expiry: %w", translateError(execErr))
		}
		n, rowsErr := updateResult.RowsAffected()
		if rowsErr != nil {
			return fmt.Errorf("update old key rows affected: %w", rowsErr)
		}
		if n == 0 {
			return fmt.Errorf("update old key expiry: %w", ErrNotFound)
		}

		newRow := q.QueryRowContext(ctx, selectQuery, newID.String())
		var scanErr error
		result.NewKey, scanErr = scanAPIKey(newRow)
		if scanErr != nil {
			return fmt.Errorf("scan new key: %w", scanErr)
		}

		oldRow := q.QueryRowContext(ctx, selectQuery, oldID)
		result.OldKey, scanErr = scanAPIKey(oldRow)
		if scanErr != nil {
			return fmt.Errorf("scan old key: %w", scanErr)
		}

		return nil
	})
	if txErr != nil {
		return nil, fmt.Errorf("rotate key tx: %w", txErr)
	}
	return &result, nil
}

// GetUserOrgRole resolves the effective RBAC role for a user in an organization.
// It checks users.is_system_admin first (returns "system_admin" if true),
// then falls back to org_memberships.role. Returns ErrNotFound if the user
// has no membership in the org and is not a system admin.
func (d *DB) GetUserOrgRole(ctx context.Context, userID string, orgID string) (string, error) {
	p := d.dialect.Placeholder

	var isAdmin int
	err := d.sql.QueryRowContext(ctx,
		"SELECT is_system_admin FROM users WHERE id = "+p(1)+" AND deleted_at IS NULL",
		userID).Scan(&isAdmin)
	if err != nil {
		return "", fmt.Errorf("get user org role user %s: %w", userID, translateError(err))
	}
	if isAdmin == 1 {
		return "system_admin", nil
	}

	var role string
	err = d.sql.QueryRowContext(ctx,
		"SELECT role FROM org_memberships WHERE user_id = "+p(1)+" AND org_id = "+p(2),
		userID, orgID).Scan(&role)
	if err != nil {
		return "", fmt.Errorf("get user org role user %s org %s: %w", userID, orgID, translateError(err))
	}
	return role, nil
}

// IsTeamMember reports whether a user is a member of the given team.
// It returns false (not an error) if the user has no membership row.
func (d *DB) IsTeamMember(ctx context.Context, userID string, teamID string) (bool, error) {
	p := d.dialect.Placeholder
	query := "SELECT COUNT(*) FROM team_memberships WHERE user_id = " +
		p(1) + " AND team_id = " + p(2)
	var count int
	if err := d.sql.QueryRowContext(ctx, query, userID, teamID).Scan(&count); err != nil {
		return false, fmt.Errorf("is team member: %w", err)
	}
	return count > 0, nil
}

// scanAPIKey scans a single API key row returned by QueryRowContext.
func scanAPIKey(row *sql.Row) (*APIKey, error) {
	var k APIKey
	err := row.Scan(
		&k.ID, &k.KeyHash, &k.KeyHint, &k.KeyType, &k.Name,
		&k.OrgID, &k.TeamID, &k.UserID, &k.ServiceAccountID,
		&k.DailyTokenLimit, &k.MonthlyTokenLimit,
		&k.RequestsPerMinute, &k.RequestsPerDay,
		&k.ExpiresAt, &k.LastUsedAt,
		&k.CreatedBy, &k.CreatedAt, &k.UpdatedAt, &k.DeletedAt,
	)
	if err != nil {
		return nil, err
	}
	return &k, nil
}
