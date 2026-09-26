package db

import (
	"context"
	"database/sql"
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
	// ServiceAccountTeamID is the owning service account's service_accounts.team_id,
	// resolved via LEFT JOIN. This is the source of truth for whether a sa_key is
	// team-bound or org-level — k.team_id is never set on sa_key rows. Nil when the
	// key is not a service-account key, or the service account row does not exist,
	// or the service account is org-scoped (team_id NULL).
	ServiceAccountTeamID *string
	// ServiceAccountActive is true when the owning service account row exists
	// (sa.id IS NOT NULL) and has not been soft-deleted (sa.deleted_at IS NULL).
	// Meaningless for non-SA keys. A sa_key whose service account is missing or
	// soft-deleted must not be trusted for role resolution.
	ServiceAccountActive bool

	// UserActive is true when the owning user row exists and has not been
	// soft-deleted (u.id IS NOT NULL, resolved via the LEFT JOIN users ...
	// AND u.deleted_at IS NULL clause). Meaningless for keys with no UserID
	// (team_key, sa_key). A user_key or session_key whose owning user has
	// been soft-deleted must never be trusted to authenticate, regardless of
	// what its org membership role says.
	UserActive bool
	// MembershipExists is true when the owning user has a surviving
	// org_memberships row for this key's org (m.id IS NOT NULL). Meaningless
	// for keys with no UserID. Every user belongs to exactly one org, so a
	// user_key or session_key with no membership row means the user was
	// removed from the org the key was minted in; such a key must not be
	// trusted to authenticate as the least-privilege RoleMember default —
	// see auth.Cacheable.
	MembershipExists bool

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

	// User-level rate and token limits resolved via LEFT JOIN on the owning
	// user's org membership. Zero when there is no membership row (e.g.
	// service-account keys) or the membership has no limit set.
	UserDailyTokenLimit   int64
	UserMonthlyTokenLimit int64
	UserRequestsPerMinute int
	UserRequestsPerDay    int

	// IsSystemAdmin is 1 when the owning user has users.is_system_admin set.
	IsSystemAdmin int
	// MembershipRole is the org_memberships.role for the owning user, or empty
	// when no membership row exists (e.g. service-account keys).
	MembershipRole string
}

// keySelectFromJoin is the shared SELECT column list and FROM/JOIN clause used
// by both LoadAllActiveKeys and LoadActiveKey. It resolves org, team,
// per-user (org membership), and owning-service-account metadata alongside
// the key row itself so that a single query populates a complete cache entry.
// The service_accounts JOIN is intentionally unconditional on sa.deleted_at
// (soft-delete is not filtered in the ON clause) so callers can distinguish
// "no service account row" from "soft-deleted service account row" via
// KeyRecord.ServiceAccountActive rather than have both cases collapse into
// NULL. The users and org_memberships JOINs are similarly unconditional on
// existence so callers can distinguish "user soft-deleted" (KeyRecord.UserActive)
// from "user has no membership in this key's org" (KeyRecord.MembershipExists)
// rather than have both collapse into the same NULL-derived defaults. Callers
// append their own WHERE clause; the scan order in both callers must match
// this column order exactly.
const keySelectFromJoin = `
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
    COALESCE(m.daily_token_limit, 0), COALESCE(m.monthly_token_limit, 0),
    COALESCE(m.requests_per_minute, 0), COALESCE(m.requests_per_day, 0),
    COALESCE(u.is_system_admin, 0),
    COALESCE(m.role, ''),
    sa.team_id,
    CASE WHEN sa.id IS NOT NULL AND sa.deleted_at IS NULL THEN 1 ELSE 0 END,
    CASE WHEN u.id IS NOT NULL THEN 1 ELSE 0 END,
    CASE WHEN m.id IS NOT NULL THEN 1 ELSE 0 END
FROM api_keys k
JOIN organizations o ON o.id = k.org_id AND o.deleted_at IS NULL
LEFT JOIN teams t ON t.id = k.team_id AND t.deleted_at IS NULL
LEFT JOIN users u ON u.id = k.user_id AND u.deleted_at IS NULL
LEFT JOIN org_memberships m ON m.user_id = k.user_id AND m.org_id = k.org_id
LEFT JOIN service_accounts sa ON sa.id = k.service_account_id
`

// scanKeyRecord scans a single row produced by keySelectFromJoin into a
// KeyRecord. It returns an error for the caller to collect and log rather than
// aborting the whole load, so a single corrupt row does not take down the
// entire cache population.
func scanKeyRecord(row interface {
	Scan(dest ...any) error
}) (KeyRecord, error) {
	var (
		r                    KeyRecord
		expiresAtRaw         *string
		serviceAccountActive int
		userActive           int
		membershipExists     int
	)

	if err := row.Scan(
		&r.ID, &r.KeyHash, &r.KeyType, &r.Name,
		&r.OrgID, &r.TeamID, &r.UserID, &r.ServiceAccountID,
		&r.DailyTokenLimit, &r.MonthlyTokenLimit,
		&r.RequestsPerMinute, &r.RequestsPerDay,
		&expiresAtRaw,
		&r.OrgDailyTokenLimit, &r.OrgMonthlyTokenLimit,
		&r.OrgRequestsPerMinute, &r.OrgRequestsPerDay,
		&r.TeamDailyTokenLimit, &r.TeamMonthlyTokenLimit,
		&r.TeamRequestsPerMinute, &r.TeamRequestsPerDay,
		&r.UserDailyTokenLimit, &r.UserMonthlyTokenLimit,
		&r.UserRequestsPerMinute, &r.UserRequestsPerDay,
		&r.IsSystemAdmin, &r.MembershipRole,
		&r.ServiceAccountTeamID, &serviceAccountActive,
		&userActive, &membershipExists,
	); err != nil {
		return KeyRecord{}, fmt.Errorf("scan row: %w", err)
	}
	r.ServiceAccountActive = serviceAccountActive == 1
	r.UserActive = userActive == 1
	r.MembershipExists = membershipExists == 1

	if expiresAtRaw != nil {
		t, err := time.Parse(time.RFC3339, *expiresAtRaw)
		if err != nil {
			return KeyRecord{}, fmt.Errorf("parse expires_at %q: %w", *expiresAtRaw, err)
		}
		r.ExpiresAt = &t
	}

	return r, nil
}

// LoadAllActiveKeys returns all non-deleted, non-expired API keys with their
// associated org, team, user, and per-user membership metadata for populating
// the in-memory key cache. Results are ordered by k.id ascending. Rows that
// fail to scan (due to data corruption or an unparseable expires_at value) are
// skipped; the errors for each skipped row are collected and returned so
// callers can log them individually. Each call issues a single 6-table JOIN
// against the database; results are not cached by this method.
func (d *DB) LoadAllActiveKeys(ctx context.Context) ([]KeyRecord, []error, error) {
	// The query accepts one parameter: the current UTC time in RFC3339 format,
	// used to filter out keys whose expires_at has already passed. String
	// comparison is correct here because both SQLite and PostgreSQL store
	// expires_at as RFC3339 text, and ISO-8601 strings sort lexicographically.
	// Placeholder is dialect-specific: ? for SQLite, $1 for PostgreSQL.
	q := keySelectFromJoin + fmt.Sprintf(`
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
		r, err := scanKeyRecord(rows)
		if err != nil {
			skipErrors = append(skipErrors, err)
			continue
		}
		records = append(records, r)
	}

	if err := rows.Err(); err != nil {
		return nil, skipErrors, fmt.Errorf("load all active keys: rows: %w", err)
	}

	return records, skipErrors, nil
}

// LoadActiveKey returns the single non-deleted, non-expired API key identified
// by keyID, with the same org, team, user, and per-user membership metadata as
// LoadAllActiveKeys. It is used to populate the key cache immediately after a
// key is created or rotated, so newly issued keys are subject to their full
// set of limits from the first request instead of waiting for the next
// periodic cache reload. Returns ErrNotFound if the key does not exist, is
// soft-deleted, or has already expired.
func (d *DB) LoadActiveKey(ctx context.Context, keyID string) (*KeyRecord, error) {
	p := d.dialect.Placeholder
	q := keySelectFromJoin + fmt.Sprintf(`
WHERE k.deleted_at IS NULL
  AND k.id = %s
  AND (k.expires_at IS NULL OR k.expires_at > %s)`, p(1), p(2))

	now := time.Now().UTC().Format(time.RFC3339)
	row := d.sql.QueryRowContext(ctx, q, keyID, now)
	r, err := scanKeyRecord(row)
	if err != nil {
		return nil, fmt.Errorf("load active key %s: %w", keyID, translateError(err))
	}
	return &r, nil
}

// scanKeyHashes reads a single key_hash column from every row and returns the
// collected values. It is shared by ListActiveKeyHashesByUser,
// ListActiveKeyHashesByUserInOrg, and ListActiveKeyHashesByServiceAccount.
func scanKeyHashes(rows *sql.Rows) ([]string, error) {
	var hashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("scan key hash: %w", err)
		}
		hashes = append(hashes, hash)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return hashes, nil
}

// ListActiveKeyHashesByUser returns the key_hash of every non-deleted API key
// owned by userID across every organization, including session keys. It is
// used to evict every cached key belonging to a user from the in-memory key
// cache immediately after the user is soft-deleted (see DeleteUser), so those
// keys stop authenticating on this instance without waiting for the next
// periodic cache reload.
func (d *DB) ListActiveKeyHashesByUser(ctx context.Context, userID string) ([]string, error) {
	p := d.dialect.Placeholder
	query := "SELECT key_hash FROM api_keys WHERE user_id = " + p(1) + " AND deleted_at IS NULL"

	rows, err := d.sql.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("list active key hashes by user %s: query: %w", userID, err)
	}
	defer rows.Close()

	hashes, err := scanKeyHashes(rows)
	if err != nil {
		return nil, fmt.Errorf("list active key hashes by user %s: %w", userID, err)
	}
	return hashes, nil
}

// ListActiveKeyHashesByUserInOrg returns the key_hash of every non-deleted API
// key owned by userID scoped to orgID, including session keys. It is used to
// evict every cached key belonging to a user in a single org immediately
// after that user's org membership is removed (see DeleteOrgMembership),
// since api_keys rows for other organizations the user may belong to must
// stay untouched.
func (d *DB) ListActiveKeyHashesByUserInOrg(ctx context.Context, userID, orgID string) ([]string, error) {
	p := d.dialect.Placeholder
	query := "SELECT key_hash FROM api_keys WHERE user_id = " + p(1) +
		" AND org_id = " + p(2) + " AND deleted_at IS NULL"

	rows, err := d.sql.QueryContext(ctx, query, userID, orgID)
	if err != nil {
		return nil, fmt.Errorf("list active key hashes by user %s in org %s: query: %w", userID, orgID, err)
	}
	defer rows.Close()

	hashes, err := scanKeyHashes(rows)
	if err != nil {
		return nil, fmt.Errorf("list active key hashes by user %s in org %s: %w", userID, orgID, err)
	}
	return hashes, nil
}

// ListActiveKeyHashesByServiceAccount returns the key_hash of every
// non-deleted sa_key owned by saID. Soft-deleting a service account
// (DeleteServiceAccount) only sets service_accounts.deleted_at — the owning
// api_keys rows are left untouched — so this query still returns the
// service account's keys whether it is called before or after that
// soft-delete.
func (d *DB) ListActiveKeyHashesByServiceAccount(ctx context.Context, saID string) ([]string, error) {
	p := d.dialect.Placeholder
	query := "SELECT key_hash FROM api_keys WHERE service_account_id = " + p(1) + " AND deleted_at IS NULL"

	rows, err := d.sql.QueryContext(ctx, query, saID)
	if err != nil {
		return nil, fmt.Errorf("list active key hashes by service account %s: query: %w", saID, err)
	}
	defer rows.Close()

	hashes, err := scanKeyHashes(rows)
	if err != nil {
		return nil, fmt.Errorf("list active key hashes by service account %s: %w", saID, err)
	}
	return hashes, nil
}
