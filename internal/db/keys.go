package db

import (
	"context"
	"fmt"
	"time"
)

// KeyRecord holds all columns returned by LoadAllActiveKeys. It carries the
// raw database values needed to populate the in-memory key cache including
// the org, team, and user metadata that are resolved via JOIN at load time.
// The KeyHash field is intentionally included here because the cache is keyed
// on hash; callers must never expose it in API responses.
type KeyRecord struct {
	// ID is the UUIDv7 primary key of the api_keys row.
	ID string
	// KeyHash is the HMAC-SHA256 hash of the raw key used as the cache key.
	KeyHash string
	// KeyType is one of the keygen package constants (user_key, team_key, sa_key, session_key).
	KeyType string
	// Name is the human-readable label assigned to the key.
	Name string
	// OrgID is the organization this key belongs to.
	OrgID string
	// TeamID is set only for team-scoped and team service-account keys.
	TeamID *string
	// UserID is set only for user and session keys.
	UserID *string
	// ServiceAccountID is set only for service-account keys.
	ServiceAccountID *string

	// Key-level rate and token limits.
	DailyTokenLimit   int64
	MonthlyTokenLimit int64
	RequestsPerMinute int
	RequestsPerDay    int

	// ExpiresAt is the optional key expiry, stored as RFC3339 in the DB.
	ExpiresAt *time.Time

	// Org-level rate and token limits resolved via JOIN.
	OrgDailyTokenLimit   int64
	OrgMonthlyTokenLimit int64
	OrgRequestsPerMinute int
	OrgRequestsPerDay    int

	// Team-level rate and token limits resolved via LEFT JOIN. Zero when no team.
	TeamDailyTokenLimit   int64
	TeamMonthlyTokenLimit int64
	TeamRequestsPerMinute int
	TeamRequestsPerDay    int

	// IsSystemAdmin is 1 when the owning user has users.is_system_admin set.
	IsSystemAdmin int
	// MembershipRole is the org_memberships.role for the owning user, or empty
	// when no membership row exists (e.g. service-account keys).
	MembershipRole string
}

// LoadAllActiveKeys returns all non-deleted, non-expired API keys with their
// associated org, team, and user metadata for populating the in-memory key
// cache. Results are ordered by k.id ascending. Rows that fail to scan (due to
// data corruption or an unparseable expires_at value) are skipped; the errors
// for each skipped row are collected and returned so callers can log them
// individually. Each call issues a single 5-table JOIN against the database;
// results are not cached by this method.
func (d *DB) LoadAllActiveKeys(ctx context.Context) ([]KeyRecord, []error, error) {
	// The query accepts one parameter: the current UTC time in RFC3339 format,
	// used to filter out keys whose expires_at has already passed. String
	// comparison is correct here because both SQLite and PostgreSQL store
	// expires_at as RFC3339 text, and ISO-8601 strings sort lexicographically.
	// Placeholder is dialect-specific: ? for SQLite, $1 for PostgreSQL.
	q := fmt.Sprintf(`
SELECT
    k.id, k.key_hash, k.key_type, k.name,
    k.org_id, k.team_id, k.user_id, k.service_account_id,
    k.daily_token_limit, k.monthly_token_limit,
    k.requests_per_minute, k.requests_per_day,
    k.expires_at,
    o.daily_token_limit, o.monthly_token_limit,
    o.requests_per_minute, o.requests_per_day,
    COALESCE(t.daily_token_limit, 0), COALESCE(t.monthly_token_limit, 0),
    COALESCE(t.requests_per_minute, 0), COALESCE(t.requests_per_day, 0),
    COALESCE(u.is_system_admin, 0),
    COALESCE(m.role, '')
FROM api_keys k
JOIN organizations o ON o.id = k.org_id AND o.deleted_at IS NULL
LEFT JOIN teams t ON t.id = k.team_id AND t.deleted_at IS NULL
LEFT JOIN users u ON u.id = k.user_id AND u.deleted_at IS NULL
LEFT JOIN org_memberships m ON m.user_id = k.user_id AND m.org_id = k.org_id
WHERE k.deleted_at IS NULL
  AND (k.expires_at IS NULL OR k.expires_at > %s)
ORDER BY k.id ASC`, d.dialect.Placeholder(1))

	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := d.sql.QueryContext(ctx, q, now)
	if err != nil {
		return nil, nil, fmt.Errorf("load all active keys: query: %w", err)
	}
	defer rows.Close()

	var records []KeyRecord
	var skipErrors []error

	for rows.Next() {
		var (
			r            KeyRecord
			expiresAtRaw *string
		)

		if err := rows.Scan(
			&r.ID, &r.KeyHash, &r.KeyType, &r.Name,
			&r.OrgID, &r.TeamID, &r.UserID, &r.ServiceAccountID,
			&r.DailyTokenLimit, &r.MonthlyTokenLimit,
			&r.RequestsPerMinute, &r.RequestsPerDay,
			&expiresAtRaw,
			&r.OrgDailyTokenLimit, &r.OrgMonthlyTokenLimit,
			&r.OrgRequestsPerMinute, &r.OrgRequestsPerDay,
			&r.TeamDailyTokenLimit, &r.TeamMonthlyTokenLimit,
			&r.TeamRequestsPerMinute, &r.TeamRequestsPerDay,
			&r.IsSystemAdmin, &r.MembershipRole,
		); err != nil {
			skipErrors = append(skipErrors, fmt.Errorf("scan row: %w", err))
			continue
		}

		if expiresAtRaw != nil {
			t, err := time.Parse(time.RFC3339, *expiresAtRaw)
			if err != nil {
				skipErrors = append(skipErrors, fmt.Errorf("parse expires_at %q: %w", *expiresAtRaw, err))
				continue
			}
			r.ExpiresAt = &t
		}

		records = append(records, r)
	}

	if err := rows.Err(); err != nil {
		return nil, skipErrors, fmt.Errorf("load all active keys: rows: %w", err)
	}

	return records, skipErrors, nil
}
