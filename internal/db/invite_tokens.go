package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// inviteTokenSelectColumns is the ordered column list used in all invite_tokens
// SELECT queries. It must match the scan order in scanInviteToken exactly.
const inviteTokenSelectColumns = "id, token_hash, token_hint, org_id, email, role, " +
	"expires_at, redeemed_at, created_by, created_at"

// InviteToken represents an invite_tokens record in the database.
type InviteToken struct {
	ID         string
	TokenHash  string
	TokenHint  string
	OrgID      string
	Email      string
	Role       string
	ExpiresAt  string
	RedeemedAt *string
	CreatedBy  string
	CreatedAt  string
}

// CreateInviteTokenParams holds the input for creating an invite token.
type CreateInviteTokenParams struct {
	// TokenHash is the HMAC-SHA256 hash of the raw token. Never store the raw token.
	TokenHash string
	TokenHint string
	OrgID     string
	Email     string
	Role      string
	ExpiresAt string
	CreatedBy string
}

// CreateInviteToken inserts a new invite token and returns the persisted record.
// It returns ErrConflict if an active (unredeemed) invite already exists for
// the same org and email address.
func (d *DB) CreateInviteToken(ctx context.Context, params CreateInviteTokenParams) (*InviteToken, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("create invite token: generate id: %w", err)
	}

	p := d.dialect.Placeholder
	insertQuery := "INSERT INTO invite_tokens " +
		"(id, token_hash, token_hint, org_id, email, role, expires_at, created_by, created_at) " +
		"VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " +
		p(5) + ", " + p(6) + ", " + p(7) + ", " + p(8) + ", CURRENT_TIMESTAMP)"

	selectQuery := "SELECT " + inviteTokenSelectColumns +
		" FROM invite_tokens WHERE id = " + p(1)

	var token *InviteToken
	err = d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx, insertQuery,
			id.String(),
			params.TokenHash,
			params.TokenHint,
			params.OrgID,
			params.Email,
			params.Role,
			params.ExpiresAt,
			params.CreatedBy,
		)
		if execErr != nil {
			return translateError(execErr)
		}

		row := q.QueryRowContext(ctx, selectQuery, id.String())
		var scanErr error
		token, scanErr = scanInviteToken(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("create invite token: %w", err)
	}
	return token, nil
}

// GetInviteTokenByHash retrieves an invite token by its HMAC hash.
// It returns ErrNotFound if no token with that hash exists.
func (d *DB) GetInviteTokenByHash(ctx context.Context, hash string) (*InviteToken, error) {
	query := "SELECT " + inviteTokenSelectColumns +
		" FROM invite_tokens WHERE token_hash = " + d.dialect.Placeholder(1)

	row := d.sql.QueryRowContext(ctx, query, hash)
	token, err := scanInviteToken(row)
	if err != nil {
		return nil, fmt.Errorf("get invite token by hash: %w", translateError(err))
	}
	return token, nil
}

// ListInviteTokens returns a page of invite tokens for the given org, ordered
// by ID ascending. cursor is an exclusive lower bound on ID for keyset
// pagination; pass "" to start from the beginning. limit controls the maximum
// number of records returned.
func (d *DB) ListInviteTokens(ctx context.Context, orgID string, cursor string, limit int) ([]InviteToken, error) {
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

	query := "SELECT " + inviteTokenSelectColumns + " FROM invite_tokens" +
		" WHERE " + strings.Join(conditions, " AND ") +
		" ORDER BY id ASC LIMIT " + p(argN)
	args = append(args, limit)

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list invite tokens query: %w", err)
	}
	defer rows.Close()

	var tokens []InviteToken
	for rows.Next() {
		var t InviteToken
		if err := rows.Scan(
			&t.ID, &t.TokenHash, &t.TokenHint, &t.OrgID, &t.Email, &t.Role,
			&t.ExpiresAt, &t.RedeemedAt, &t.CreatedBy, &t.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("list invite tokens scan: %w", err)
		}
		tokens = append(tokens, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list invite tokens rows: %w", err)
	}

	return tokens, nil
}

// RevokeInviteToken hard-deletes an invite token by its ID, scoped to the
// given organization. It returns ErrNotFound if no matching record exists
// within that organization, preventing cross-org access.
func (d *DB) RevokeInviteToken(ctx context.Context, id string, orgID string) error {
	p := d.dialect.Placeholder
	query := "DELETE FROM invite_tokens WHERE id = " + p(1) + " AND org_id = " + p(2)

	result, err := d.sql.ExecContext(ctx, query, id, orgID)
	if err != nil {
		return fmt.Errorf("revoke invite token %s: %w", id, translateError(err))
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke invite token %s rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("revoke invite token %s: %w", id, ErrNotFound)
	}

	return nil
}

// RevokeInviteTokensByEmail hard-deletes all unredeemed invite tokens for the
// given email address within an organization. It is used to ensure only one
// active invite exists per email per org before issuing a new one.
// No error is returned if no matching rows exist.
func (d *DB) RevokeInviteTokensByEmail(ctx context.Context, orgID string, email string) error {
	p := d.dialect.Placeholder
	query := "DELETE FROM invite_tokens WHERE org_id = " + p(1) +
		" AND email = " + p(2) + " AND redeemed_at IS NULL"

	_, err := d.sql.ExecContext(ctx, query, orgID, email)
	if err != nil {
		return fmt.Errorf("revoke invite tokens for org %s: %w", orgID, translateError(err))
	}

	return nil
}

// RedeemInviteToken marks an invite token as used by setting redeemed_at to
// the current timestamp. It is a no-op (returns ErrNotFound) if the token has
// already been redeemed or does not exist.
func (d *DB) RedeemInviteToken(ctx context.Context, id string) error {
	p := d.dialect.Placeholder
	query := "UPDATE invite_tokens SET redeemed_at = CURRENT_TIMESTAMP " +
		"WHERE id = " + p(1) + " AND redeemed_at IS NULL"

	result, err := d.sql.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("redeem invite token %s: %w", id, translateError(err))
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("redeem invite token %s rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("redeem invite token %s: %w", id, ErrNotFound)
	}

	return nil
}

// scanInviteToken scans a single invite_tokens row returned by QueryRowContext.
func scanInviteToken(row *sql.Row) (*InviteToken, error) {
	var t InviteToken
	err := row.Scan(
		&t.ID, &t.TokenHash, &t.TokenHint, &t.OrgID, &t.Email, &t.Role,
		&t.ExpiresAt, &t.RedeemedAt, &t.CreatedBy, &t.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
