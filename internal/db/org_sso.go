package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// orgSSOSelectColumns is the ordered column list used in all org_sso_config SELECT queries.
// It must match the scan order in scanOrgSSOConfig exactly.
const orgSSOSelectColumns = "id, org_id, enabled, issuer, client_id, client_secret_enc, " +
	"redirect_url, scopes, allowed_domains, auto_provision, default_role, " +
	"group_sync, group_claim, created_at, updated_at"

// OrgSSOConfig represents a per-org OIDC/SSO configuration record in the database.
type OrgSSOConfig struct {
	// ID is the UUIDv7 primary key.
	ID string
	// OrgID is the owning organization's ID.
	OrgID string
	// Enabled controls whether SSO login is active for this org.
	Enabled bool
	// Issuer is the OIDC provider's issuer URL.
	Issuer string
	// ClientID is the OAuth2 client identifier.
	ClientID string
	// ClientSecretEnc is the AES-256-GCM ciphertext of the client secret (base64 encoded).
	ClientSecretEnc string
	// RedirectURL is the absolute callback URL registered with the identity provider.
	RedirectURL string
	// Scopes is the list of OAuth2 scopes to request.
	Scopes []string
	// AllowedDomains restricts login to these email domains. Empty means any domain.
	AllowedDomains []string
	// AutoProvision controls whether unrecognized users are created on first login.
	AutoProvision bool
	// DefaultRole is the RBAC role assigned to auto-provisioned users.
	DefaultRole string
	// GroupSync enables team membership synchronization from the group claim.
	GroupSync bool
	// GroupClaim is the ID token claim key containing the user's group list.
	GroupClaim string
	// CreatedAt is the UTC timestamp of record creation.
	CreatedAt string
	// UpdatedAt is the UTC timestamp of the last modification.
	UpdatedAt string
}

// UpsertOrgSSOParams holds the input for creating or updating an org SSO config.
// ClientSecretEnc must already be encrypted by the caller before passing to UpsertOrgSSOConfig.
// Scopes and AllowedDomains are JSON-encoded strings (e.g. `["openid","email"]`).
type UpsertOrgSSOParams struct {
	// Enabled controls whether SSO is active.
	Enabled bool
	// Issuer is the OIDC provider's issuer URL.
	Issuer string
	// ClientID is the OAuth2 client identifier.
	ClientID string
	// ClientSecretEnc is the AES-256-GCM ciphertext of the client secret (base64 encoded).
	// The caller is responsible for encrypting the raw secret before setting this field.
	ClientSecretEnc string
	// RedirectURL is the absolute callback URL.
	RedirectURL string
	// Scopes is a JSON-encoded array of OAuth2 scopes (e.g. `["openid","email","profile"]`).
	Scopes string
	// AllowedDomains is a JSON-encoded array of allowed email domains (e.g. `["example.com"]`).
	AllowedDomains string
	// AutoProvision controls whether unrecognized users are created on first login.
	AutoProvision bool
	// DefaultRole is the RBAC role assigned to auto-provisioned users.
	DefaultRole string
	// GroupSync enables team membership synchronization from the group claim.
	GroupSync bool
	// GroupClaim is the ID token claim key containing the user's group list.
	GroupClaim string
}

// GetOrgSSOConfig retrieves the SSO configuration for the given org.
// It returns ErrNotFound if no config has been created for the org.
func (d *DB) GetOrgSSOConfig(ctx context.Context, orgID string) (*OrgSSOConfig, error) {
	query := "SELECT " + orgSSOSelectColumns +
		" FROM org_sso_config WHERE org_id = " + d.dialect.Placeholder(1)

	row := d.sql.QueryRowContext(ctx, query, orgID)
	cfg, err := scanOrgSSOConfig(row)
	if err != nil {
		return nil, fmt.Errorf("get org sso config %s: %w", orgID, translateError(err))
	}
	return cfg, nil
}

// UpsertOrgSSOConfig inserts or updates the SSO configuration for the given org.
// If a config already exists for the org it is updated in place; otherwise a new
// record is created with a generated UUIDv7 ID.
// It returns the persisted record after the write.
func (d *DB) UpsertOrgSSOConfig(ctx context.Context, orgID string, params UpsertOrgSSOParams) (*OrgSSOConfig, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("upsert org sso config: generate id: %w", err)
	}

	p := d.dialect.Placeholder

	upsertQuery := "INSERT INTO org_sso_config " +
		"(id, org_id, enabled, issuer, client_id, client_secret_enc, " +
		"redirect_url, scopes, allowed_domains, auto_provision, default_role, " +
		"group_sync, group_claim, created_at, updated_at) " +
		"VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " + p(5) + ", " +
		p(6) + ", " + p(7) + ", " + p(8) + ", " + p(9) + ", " +
		p(10) + ", " + p(11) + ", " + p(12) + ", " + p(13) + ", " +
		"CURRENT_TIMESTAMP, CURRENT_TIMESTAMP) " +
		"ON CONFLICT(org_id) DO UPDATE SET " +
		"enabled = " + p(14) + ", " +
		"issuer = " + p(15) + ", " +
		"client_id = " + p(16) + ", " +
		"client_secret_enc = " + p(17) + ", " +
		"redirect_url = " + p(18) + ", " +
		"scopes = " + p(19) + ", " +
		"allowed_domains = " + p(20) + ", " +
		"auto_provision = " + p(21) + ", " +
		"default_role = " + p(22) + ", " +
		"group_sync = " + p(23) + ", " +
		"group_claim = " + p(24) + ", " +
		"updated_at = CURRENT_TIMESTAMP"

	selectQuery := "SELECT " + orgSSOSelectColumns +
		" FROM org_sso_config WHERE org_id = " + p(1)

	enabledInt := boolToInt(params.Enabled)
	autoProvInt := boolToInt(params.AutoProvision)
	groupSyncInt := boolToInt(params.GroupSync)

	var cfg *OrgSSOConfig
	err = d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx, upsertQuery,
			// INSERT values (1–13)
			id.String(),
			orgID,
			enabledInt,
			params.Issuer,
			params.ClientID,
			params.ClientSecretEnc,
			params.RedirectURL,
			params.Scopes,
			params.AllowedDomains,
			autoProvInt,
			params.DefaultRole,
			groupSyncInt,
			params.GroupClaim,
			// ON CONFLICT UPDATE values (14–24)
			enabledInt,
			params.Issuer,
			params.ClientID,
			params.ClientSecretEnc,
			params.RedirectURL,
			params.Scopes,
			params.AllowedDomains,
			autoProvInt,
			params.DefaultRole,
			groupSyncInt,
			params.GroupClaim,
		)
		if execErr != nil {
			return translateError(execErr)
		}

		row := q.QueryRowContext(ctx, selectQuery, orgID)
		var scanErr error
		cfg, scanErr = scanOrgSSOConfig(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("upsert org sso config %s: %w", orgID, err)
	}
	return cfg, nil
}

// DeleteOrgSSOConfig permanently removes the SSO configuration for the given org.
// It returns ErrNotFound if no config exists for the org.
func (d *DB) DeleteOrgSSOConfig(ctx context.Context, orgID string) error {
	p := d.dialect.Placeholder
	query := "DELETE FROM org_sso_config WHERE org_id = " + p(1)

	result, err := d.sql.ExecContext(ctx, query, orgID)
	if err != nil {
		return fmt.Errorf("delete org sso config %s: %w", orgID, translateError(err))
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete org sso config %s rows affected: %w", orgID, err)
	}
	if n == 0 {
		return fmt.Errorf("delete org sso config %s: %w", orgID, ErrNotFound)
	}

	return nil
}

// scanOrgSSOConfig scans a single org_sso_config row. The scanner may be a
// *sql.Row (from QueryRowContext) or *sql.Rows (from QueryContext); both
// satisfy the interface. The scopes and allowed_domains columns are stored as
// JSON strings and are unmarshalled into []string slices before returning.
func scanOrgSSOConfig(scanner interface{ Scan(...any) error }) (*OrgSSOConfig, error) {
	var cfg OrgSSOConfig
	var enabledInt, autoProvInt, groupSyncInt int
	var scopesJSON, allowedDomainsJSON string

	err := scanner.Scan(
		&cfg.ID, &cfg.OrgID,
		&enabledInt,
		&cfg.Issuer, &cfg.ClientID, &cfg.ClientSecretEnc,
		&cfg.RedirectURL,
		&scopesJSON, &allowedDomainsJSON,
		&autoProvInt,
		&cfg.DefaultRole,
		&groupSyncInt, &cfg.GroupClaim,
		&cfg.CreatedAt, &cfg.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	cfg.Enabled = enabledInt == 1
	cfg.AutoProvision = autoProvInt == 1
	cfg.GroupSync = groupSyncInt == 1

	if err := jsonx.Unmarshal([]byte(scopesJSON), &cfg.Scopes); err != nil {
		return nil, fmt.Errorf("unmarshal scopes: %w", err)
	}
	if err := jsonx.Unmarshal([]byte(allowedDomainsJSON), &cfg.AllowedDomains); err != nil {
		return nil, fmt.Errorf("unmarshal allowed_domains: %w", err)
	}

	return &cfg, nil
}

// boolToInt converts a bool to the integer representation used in SQLite CHECK
// constraints (0 or 1). PostgreSQL BOOLEAN columns also accept integer literals.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
