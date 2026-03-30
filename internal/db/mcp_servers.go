package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/pkg/crypto"
)

// mcpServerSelectColumns is the ordered column list used in all mcp_servers SELECT queries.
// It must match the scan order in scanMCPServer exactly.
const mcpServerSelectColumns = "id, name, alias, url, auth_type, auth_header, " +
	"auth_token_enc, org_id, team_id, is_active, created_by, source, code_mode_enabled, created_at, updated_at, deleted_at"

// MCPServer represents an external MCP server record in the database.
type MCPServer struct {
	ID           string
	Name         string
	Alias        string
	URL          string
	AuthType     string  // "none", "bearer", or "header"
	AuthHeader   string  // header name used when AuthType is "header"
	AuthTokenEnc *string // AES-256-GCM encrypted token; nil when AuthType is "none"
	OrgID        *string // nil for global servers
	TeamID       *string // nil for global or org-scoped servers
	IsActive     bool
	CreatedBy    *string
	// Source indicates how this server was registered: "api" for Admin API-created
	// servers, "yaml" for config-file-sourced servers, or "builtin" for the
	// built-in VoidLLM management server. Defaults to "api".
	Source string
	// CodeModeEnabled controls whether this server's tools are available in
	// Code Mode sandboxed execution. Default true.
	CodeModeEnabled bool
	CreatedAt       string
	UpdatedAt       string
	DeletedAt       *string
}

// CreateMCPServerParams holds the input for creating an MCP server record.
type CreateMCPServerParams struct {
	Name         string
	Alias        string
	URL          string
	AuthType     string
	AuthHeader   string
	AuthTokenEnc *string
	OrgID        *string // nil for global servers
	TeamID       *string // nil for global or org-scoped servers
	CreatedBy    string
	// Source is "yaml" for config-file-sourced servers or "api" for Admin
	// API-created servers. Defaults to "api" when empty.
	Source string
	// CodeModeEnabled controls whether this server's tools are available in
	// Code Mode sandboxed execution. Defaults to true when nil.
	CodeModeEnabled *bool
}

// UpdateMCPServerParams holds optional fields for updating an MCP server.
// A nil pointer means the field is not changed.
type UpdateMCPServerParams struct {
	Name         *string
	Alias        *string
	URL          *string
	AuthType     *string
	AuthHeader   *string
	AuthTokenEnc *string
	// IsActive, when non-nil, sets the is_active flag. Use the dedicated
	// ActivateMCPServer / DeactivateMCPServer helpers where possible.
	IsActive *bool
	// CodeModeEnabled, when non-nil, sets the code_mode_enabled flag.
	CodeModeEnabled *bool
}

// CreateMCPServer inserts a new MCP server record and returns the persisted row.
// It returns ErrConflict if a server with the same (org_id, team_id, alias) combination already exists.
func (d *DB) CreateMCPServer(ctx context.Context, params CreateMCPServerParams) (*MCPServer, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("create mcp server: generate id: %w", err)
	}

	source := params.Source
	if source == "" {
		source = "api"
	}

	codeModeEnabled := 1
	if params.CodeModeEnabled != nil && !*params.CodeModeEnabled {
		codeModeEnabled = 0
	}

	p := d.dialect.Placeholder
	insertQuery := "INSERT INTO mcp_servers " +
		"(id, name, alias, url, auth_type, auth_header, auth_token_enc, " +
		"org_id, team_id, is_active, created_by, source, code_mode_enabled, created_at, updated_at) " +
		"VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " + p(5) + ", " +
		p(6) + ", " + p(7) + ", " +
		p(8) + ", " + p(9) + ", " +
		"1, " + p(10) + ", " + p(11) + ", " + p(12) + ", " +
		"CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"

	selectQuery := "SELECT " + mcpServerSelectColumns +
		" FROM mcp_servers WHERE id = " + p(1) + " AND deleted_at IS NULL"

	var createdBy any
	if params.CreatedBy != "" {
		createdBy = params.CreatedBy
	}

	var server *MCPServer
	err = d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx, insertQuery,
			id.String(),
			params.Name,
			params.Alias,
			params.URL,
			params.AuthType,
			params.AuthHeader,
			params.AuthTokenEnc,
			params.OrgID,
			params.TeamID,
			createdBy,
			source,
			codeModeEnabled,
		)
		if execErr != nil {
			return translateError(execErr)
		}

		row := q.QueryRowContext(ctx, selectQuery, id.String())
		var scanErr error
		server, scanErr = scanMCPServer(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("create mcp server: %w", err)
	}
	return server, nil
}

// GetMCPServer retrieves an active MCP server by its ID.
// It returns ErrNotFound if the server does not exist or has been soft-deleted.
func (d *DB) GetMCPServer(ctx context.Context, id string) (*MCPServer, error) {
	query := "SELECT " + mcpServerSelectColumns +
		" FROM mcp_servers WHERE id = " + d.dialect.Placeholder(1) + " AND deleted_at IS NULL"

	row := d.sql.QueryRowContext(ctx, query, id)
	server, err := scanMCPServer(row)
	if err != nil {
		return nil, fmt.Errorf("get mcp server %s: %w", id, translateError(err))
	}
	return server, nil
}

// GetMCPServerByAlias retrieves a global active MCP server (org_id IS NULL,
// team_id IS NULL) by its alias. It returns ErrNotFound if no such server
// exists, has been soft-deleted, or is inactive.
//
// Use GetMCPServerByAliasScoped for scope-aware resolution in the proxy path.
func (d *DB) GetMCPServerByAlias(ctx context.Context, alias string) (*MCPServer, error) {
	p := d.dialect.Placeholder
	query := "SELECT " + mcpServerSelectColumns +
		" FROM mcp_servers WHERE alias = " + p(1) +
		" AND is_active = 1 AND deleted_at IS NULL" +
		" AND org_id IS NULL AND team_id IS NULL"

	row := d.sql.QueryRowContext(ctx, query, alias)
	server, err := scanMCPServer(row)
	if err != nil {
		return nil, fmt.Errorf("get mcp server by alias %q: %w", alias, translateError(err))
	}
	return server, nil
}

// GetMCPServerByAliasScoped resolves an active MCP server by alias using
// scope priority: team-scoped (highest) → org-scoped → global (lowest).
// orgID and teamID may each be empty string to indicate absence of that scope.
// It returns ErrNotFound if no matching server exists.
func (d *DB) GetMCPServerByAliasScoped(ctx context.Context, alias, orgID, teamID string) (*MCPServer, error) {
	p := d.dialect.Placeholder
	query := "SELECT " + mcpServerSelectColumns +
		" FROM mcp_servers" +
		" WHERE alias = " + p(1) + " AND deleted_at IS NULL AND is_active = 1" +
		" AND (" +
		"(team_id = " + p(2) + " AND org_id = " + p(3) + ")" +
		" OR (team_id IS NULL AND org_id = " + p(4) + ")" +
		" OR (team_id IS NULL AND org_id IS NULL)" +
		")" +
		" ORDER BY" +
		" CASE WHEN team_id IS NOT NULL THEN 1" +
		"      WHEN org_id IS NOT NULL THEN 2" +
		"      ELSE 3" +
		" END" +
		" LIMIT 1"

	// Use empty string args for nullable IDs; the SQL comparisons handle IS NULL
	// separately, so non-empty teamID/orgID only match their respective clauses.
	var teamArg, orgArg any
	if teamID != "" {
		teamArg = teamID
	}
	if orgID != "" {
		orgArg = orgID
	}

	row := d.sql.QueryRowContext(ctx, query, alias, teamArg, orgArg, orgArg)
	server, err := scanMCPServer(row)
	if err != nil {
		return nil, fmt.Errorf("get mcp server by alias scoped %q: %w", alias, translateError(err))
	}
	return server, nil
}

// ListMCPServers returns all active, non-deleted global MCP servers
// (org_id IS NULL, team_id IS NULL) ordered by alias ascending.
// Intended for system_admin use only.
func (d *DB) ListMCPServers(ctx context.Context) ([]MCPServer, error) {
	query := "SELECT " + mcpServerSelectColumns +
		" FROM mcp_servers" +
		" WHERE is_active = 1 AND deleted_at IS NULL" +
		" AND org_id IS NULL AND team_id IS NULL" +
		" ORDER BY alias ASC"

	rows, err := d.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list mcp servers query: %w", err)
	}
	defer rows.Close()

	var servers []MCPServer
	for rows.Next() {
		s, scanErr := scanMCPServer(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list mcp servers scan: %w", scanErr)
		}
		servers = append(servers, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list mcp servers rows: %w", err)
	}

	return servers, nil
}

// ListMCPServersByOrg returns all active, non-deleted MCP servers visible to
// the given org: org-scoped servers for that org plus all global servers.
// Results are ordered by alias ascending.
func (d *DB) ListMCPServersByOrg(ctx context.Context, orgID string) ([]MCPServer, error) {
	p := d.dialect.Placeholder
	query := "SELECT " + mcpServerSelectColumns +
		" FROM mcp_servers" +
		" WHERE deleted_at IS NULL AND is_active = 1" +
		" AND (org_id = " + p(1) + " OR org_id IS NULL)" +
		" AND team_id IS NULL" +
		" ORDER BY alias ASC"

	rows, err := d.sql.QueryContext(ctx, query, orgID)
	if err != nil {
		return nil, fmt.Errorf("list mcp servers by org query: %w", err)
	}
	defer rows.Close()

	var servers []MCPServer
	for rows.Next() {
		s, scanErr := scanMCPServer(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list mcp servers by org scan: %w", scanErr)
		}
		servers = append(servers, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list mcp servers by org rows: %w", err)
	}

	return servers, nil
}

// ListMCPServersByTeam returns all active, non-deleted MCP servers visible to
// the given team: team-scoped, org-scoped (for the team's org), and global.
// Results are ordered by alias ascending.
func (d *DB) ListMCPServersByTeam(ctx context.Context, teamID, orgID string) ([]MCPServer, error) {
	p := d.dialect.Placeholder
	query := "SELECT " + mcpServerSelectColumns +
		" FROM mcp_servers" +
		" WHERE deleted_at IS NULL AND is_active = 1" +
		" AND (" +
		"(team_id = " + p(1) + " AND org_id = " + p(2) + ")" +
		" OR (team_id IS NULL AND org_id = " + p(3) + ")" +
		" OR (team_id IS NULL AND org_id IS NULL)" +
		")" +
		" ORDER BY alias ASC"

	rows, err := d.sql.QueryContext(ctx, query, teamID, orgID, orgID)
	if err != nil {
		return nil, fmt.Errorf("list mcp servers by team query: %w", err)
	}
	defer rows.Close()

	var servers []MCPServer
	for rows.Next() {
		s, scanErr := scanMCPServer(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list mcp servers by team scan: %w", scanErr)
		}
		servers = append(servers, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list mcp servers by team rows: %w", err)
	}

	return servers, nil
}

// UpdateMCPServer applies a partial update to an active MCP server.
// Only non-nil fields in params are written. If all fields are nil the record
// is returned unchanged without issuing an UPDATE.
// It returns ErrNotFound if the server does not exist or has been soft-deleted,
// and ErrConflict if the new alias collides with an existing server in the same scope.
func (d *DB) UpdateMCPServer(ctx context.Context, id string, params UpdateMCPServerParams) (*MCPServer, error) {
	p := d.dialect.Placeholder
	argN := 1
	var setClauses []string
	var args []any

	if params.Name != nil {
		setClauses = append(setClauses, "name = "+p(argN))
		args = append(args, *params.Name)
		argN++
	}
	if params.Alias != nil {
		setClauses = append(setClauses, "alias = "+p(argN))
		args = append(args, *params.Alias)
		argN++
	}
	if params.URL != nil {
		setClauses = append(setClauses, "url = "+p(argN))
		args = append(args, *params.URL)
		argN++
	}
	if params.AuthType != nil {
		setClauses = append(setClauses, "auth_type = "+p(argN))
		args = append(args, *params.AuthType)
		argN++
	}
	if params.AuthHeader != nil {
		setClauses = append(setClauses, "auth_header = "+p(argN))
		args = append(args, *params.AuthHeader)
		argN++
	}
	if params.AuthTokenEnc != nil {
		setClauses = append(setClauses, "auth_token_enc = "+p(argN))
		args = append(args, *params.AuthTokenEnc)
		argN++
	}
	if params.IsActive != nil {
		val := 0
		if *params.IsActive {
			val = 1
		}
		setClauses = append(setClauses, "is_active = "+p(argN))
		args = append(args, val)
		argN++
	}
	if params.CodeModeEnabled != nil {
		val := 0
		if *params.CodeModeEnabled {
			val = 1
		}
		setClauses = append(setClauses, "code_mode_enabled = "+p(argN))
		args = append(args, val)
		argN++
	}

	if len(setClauses) == 0 {
		return d.GetMCPServer(ctx, id)
	}

	setClauses = append(setClauses, "updated_at = CURRENT_TIMESTAMP")

	updateQuery := "UPDATE mcp_servers SET " + strings.Join(setClauses, ", ") +
		" WHERE id = " + p(argN) + " AND deleted_at IS NULL"
	args = append(args, id)

	selectQuery := "SELECT " + mcpServerSelectColumns +
		" FROM mcp_servers WHERE id = " + p(1) + " AND deleted_at IS NULL"

	var server *MCPServer
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
		server, scanErr = scanMCPServer(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("update mcp server %s: %w", id, err)
	}
	return server, nil
}

// DeleteMCPServer soft-deletes an active MCP server by setting deleted_at.
// It returns ErrNotFound if the server does not exist or is already deleted.
func (d *DB) DeleteMCPServer(ctx context.Context, id string) error {
	p := d.dialect.Placeholder
	query := "UPDATE mcp_servers SET deleted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP " +
		"WHERE id = " + p(1) + " AND deleted_at IS NULL"

	result, err := d.sql.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("delete mcp server %s: %w", id, translateError(err))
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete mcp server %s rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("delete mcp server %s: %w", id, ErrNotFound)
	}

	return nil
}

// LoadAllActiveMCPServers returns all active, non-deleted MCP servers ordered
// by alias. Used for bulk-loading the in-memory server cache at startup and
// on periodic refresh.
func (d *DB) LoadAllActiveMCPServers(ctx context.Context) ([]MCPServer, error) {
	query := "SELECT " + mcpServerSelectColumns +
		" FROM mcp_servers WHERE is_active = 1 AND deleted_at IS NULL ORDER BY alias"

	rows, err := d.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("load all active mcp servers query: %w", err)
	}
	defer rows.Close()

	var servers []MCPServer
	for rows.Next() {
		s, scanErr := scanMCPServer(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("load all active mcp servers scan: %w", scanErr)
		}
		servers = append(servers, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load all active mcp servers rows: %w", err)
	}

	return servers, nil
}

// scanMCPServer scans a single MCP server row. The scanner may be a *sql.Row
// (from QueryRowContext) or *sql.Rows (from QueryContext); both satisfy the interface.
func scanMCPServer(scanner interface{ Scan(...any) error }) (*MCPServer, error) {
	var s MCPServer
	var isActiveInt int
	var codeModeEnabledInt int
	err := scanner.Scan(
		&s.ID, &s.Name, &s.Alias, &s.URL, &s.AuthType, &s.AuthHeader,
		&s.AuthTokenEnc, &s.OrgID, &s.TeamID, &isActiveInt, &s.CreatedBy,
		&s.Source, &codeModeEnabledInt, &s.CreatedAt, &s.UpdatedAt, &s.DeletedAt,
	)
	if err != nil {
		return nil, err
	}
	s.IsActive = isActiveInt == 1
	s.CodeModeEnabled = codeModeEnabledInt == 1
	return &s, nil
}

// mcpServerAAD returns the additional authenticated data used when encrypting
// and decrypting MCP server auth tokens. The AAD binds the ciphertext to the
// specific server row so that a ciphertext from one row cannot be replayed
// against a different row.
func mcpServerAAD(serverID string) []byte {
	return []byte("mcp_server:" + serverID)
}

// EnsureBuiltinMCPServer creates or returns the built-in VoidLLM management
// MCP server record. It is idempotent — safe to call on every startup.
// If a server with alias "voidllm" already exists (any source), it is left
// untouched and returned as-is.
func (d *DB) EnsureBuiltinMCPServer(ctx context.Context) (*MCPServer, error) {
	existing, err := d.GetMCPServerByAlias(ctx, "voidllm")
	if err == nil {
		return existing, nil // already exists and is active
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("ensure builtin mcp server: %w", err)
	}

	// Not found (inactive or never created). Try to create.
	created, createErr := d.CreateMCPServer(ctx, CreateMCPServerParams{
		Name:     "VoidLLM",
		Alias:    "voidllm",
		URL:      "",
		AuthType: "none",
		Source:   "builtin",
	})
	if createErr == nil {
		return created, nil
	}

	// ErrConflict means the row exists but is inactive or soft-deleted.
	// Re-activate it.
	if errors.Is(createErr, ErrConflict) {
		p := d.dialect.Placeholder
		reactivateQuery := "UPDATE mcp_servers SET is_active = 1, deleted_at = NULL, updated_at = CURRENT_TIMESTAMP" +
			" WHERE alias = " + p(1) + " AND org_id IS NULL AND team_id IS NULL"
		if _, execErr := d.sql.ExecContext(ctx, reactivateQuery, "voidllm"); execErr != nil {
			return nil, fmt.Errorf("ensure builtin mcp server: reactivate: %w", execErr)
		}
		// Re-fetch the now-active row.
		return d.GetMCPServerByAlias(ctx, "voidllm")
	}

	return nil, fmt.Errorf("ensure builtin mcp server: %w", createErr)
}

// SyncYAMLMCPServers upserts YAML-configured global MCP servers into the database.
//
// For each server in the provided slice:
//   - If an existing global server with the same alias has source="api", it is
//     left untouched; API-created servers take precedence over YAML configuration.
//   - If no matching server exists, a new record is created with source="yaml".
//   - If a matching server exists with source="yaml", it is updated to reflect
//     the current YAML values.
//
// When a server entry carries an auth token it is encrypted with AES-256-GCM
// using the server's database ID as additional authenticated data (AAD). For
// newly created servers the token is written in a separate UPDATE after the
// INSERT returns the generated ID.
//
// encKey must be a 32-byte AES-256 key (see crypto.ParseKey).
func (d *DB) SyncYAMLMCPServers(ctx context.Context, servers []config.MCPServerConfig, encKey []byte) error {
	for _, s := range servers {
		authType := s.AuthType
		if authType == "" {
			authType = "none"
		}

		existing, err := d.GetMCPServerByAlias(ctx, s.Alias)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("sync yaml mcp servers: check %s: %w", s.Alias, err)
		}

		if errors.Is(err, ErrNotFound) {
			// Server is not in the DB — create it with source="yaml".
			created, createErr := d.CreateMCPServer(ctx, CreateMCPServerParams{
				Name:       s.Name,
				Alias:      s.Alias,
				URL:        s.URL,
				AuthType:   authType,
				AuthHeader: s.AuthHeader,
				Source:     "yaml",
			})
			if createErr != nil {
				return fmt.Errorf("sync yaml mcp servers: create %s: %w", s.Alias, createErr)
			}

			// Encrypt the auth token now that we have the server ID to use as AAD,
			// then store it in a follow-up UPDATE.
			if s.AuthToken != "" {
				enc, encErr := crypto.EncryptString(s.AuthToken, encKey, mcpServerAAD(created.ID))
				if encErr != nil {
					return fmt.Errorf("sync yaml mcp servers: encrypt token for %s: %w", s.Alias, encErr)
				}
				if _, updateErr := d.UpdateMCPServer(ctx, created.ID, UpdateMCPServerParams{
					AuthTokenEnc: &enc,
				}); updateErr != nil {
					return fmt.Errorf("sync yaml mcp servers: set token for %s: %w", s.Alias, updateErr)
				}
			}
			continue
		}

		// Server exists in DB — skip if it was created via the Admin API.
		if existing.Source != "yaml" {
			continue
		}

		// source="yaml" — update with the current YAML values.
		name := s.Name
		url := s.URL
		authTypeVal := authType
		authHeader := s.AuthHeader

		updateParams := UpdateMCPServerParams{
			Name:       &name,
			URL:        &url,
			AuthType:   &authTypeVal,
			AuthHeader: &authHeader,
		}

		if s.AuthToken != "" {
			enc, encErr := crypto.EncryptString(s.AuthToken, encKey, mcpServerAAD(existing.ID))
			if encErr != nil {
				return fmt.Errorf("sync yaml mcp servers: encrypt token for %s: %w", s.Alias, encErr)
			}
			updateParams.AuthTokenEnc = &enc
		}

		if _, updateErr := d.UpdateMCPServer(ctx, existing.ID, updateParams); updateErr != nil {
			return fmt.Errorf("sync yaml mcp servers: update %s: %w", s.Alias, updateErr)
		}
	}
	return nil
}
