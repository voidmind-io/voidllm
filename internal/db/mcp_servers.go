package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// mcpServerSelectColumns is the ordered column list used in all mcp_servers SELECT queries.
// It must match the scan order in scanMCPServer exactly.
const mcpServerSelectColumns = "id, name, alias, url, auth_type, auth_header, " +
	"auth_token_enc, org_id, team_id, is_active, created_by, created_at, updated_at, deleted_at"

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
	CreatedAt    string
	UpdatedAt    string
	DeletedAt    *string
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
}

// CreateMCPServer inserts a new MCP server record and returns the persisted row.
// It returns ErrConflict if a server with the same (org_id, team_id, alias) combination already exists.
func (d *DB) CreateMCPServer(ctx context.Context, params CreateMCPServerParams) (*MCPServer, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("create mcp server: generate id: %w", err)
	}

	p := d.dialect.Placeholder
	insertQuery := "INSERT INTO mcp_servers " +
		"(id, name, alias, url, auth_type, auth_header, auth_token_enc, " +
		"org_id, team_id, is_active, created_by, created_at, updated_at) " +
		"VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " + p(5) + ", " +
		p(6) + ", " + p(7) + ", " +
		p(8) + ", " + p(9) + ", " +
		"1, " + p(10) + ", " +
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

// scanMCPServer scans a single MCP server row. The scanner may be a *sql.Row
// (from QueryRowContext) or *sql.Rows (from QueryContext); both satisfy the interface.
func scanMCPServer(scanner interface{ Scan(...any) error }) (*MCPServer, error) {
	var s MCPServer
	var isActiveInt int
	err := scanner.Scan(
		&s.ID, &s.Name, &s.Alias, &s.URL, &s.AuthType, &s.AuthHeader,
		&s.AuthTokenEnc, &s.OrgID, &s.TeamID, &isActiveInt, &s.CreatedBy,
		&s.CreatedAt, &s.UpdatedAt, &s.DeletedAt,
	)
	if err != nil {
		return nil, err
	}
	s.IsActive = isActiveInt == 1
	return &s, nil
}
