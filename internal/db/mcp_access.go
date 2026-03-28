package db

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
)

// GetOrgMCPAccess returns the allowed MCP server IDs for an org, ordered by server_id.
// An empty slice means "not configured" — no MCP servers are allowed at the org level
// (explicit-allow policy).
func (d *DB) GetOrgMCPAccess(ctx context.Context, orgID string) ([]string, error) {
	query := "SELECT server_id FROM org_mcp_access WHERE org_id = " + d.dialect.Placeholder(1) + " ORDER BY server_id"

	rows, err := d.sql.QueryContext(ctx, query, orgID)
	if err != nil {
		return nil, fmt.Errorf("get org mcp access %s: %w", orgID, err)
	}
	defer rows.Close()

	var serverIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("get org mcp access %s scan: %w", orgID, err)
		}
		serverIDs = append(serverIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get org mcp access %s rows: %w", orgID, err)
	}

	return serverIDs, nil
}

// SetOrgMCPAccess atomically replaces the MCP server allowlist for an org.
// An empty serverIDs slice clears the list, meaning no MCP servers are accessible.
func (d *DB) SetOrgMCPAccess(ctx context.Context, orgID string, serverIDs []string) error {
	p := d.dialect.Placeholder
	deleteQuery := "DELETE FROM org_mcp_access WHERE org_id = " + p(1)
	insertQuery := "INSERT INTO org_mcp_access (id, org_id, server_id) VALUES (" + p(1) + ", " + p(2) + ", " + p(3) + ")"

	err := d.WithTx(ctx, func(q Querier) error {
		if _, err := q.ExecContext(ctx, deleteQuery, orgID); err != nil {
			return fmt.Errorf("delete org mcp access: %w", err)
		}

		for _, serverID := range serverIDs {
			id, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("generate id: %w", err)
			}
			if _, err := q.ExecContext(ctx, insertQuery, id.String(), orgID, serverID); err != nil {
				return fmt.Errorf("insert org mcp access %q: %w", serverID, err)
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("set org mcp access %s: %w", orgID, err)
	}

	return nil
}

// GetTeamMCPAccess returns the allowed MCP server IDs for a team, ordered by server_id.
// An empty slice means "not configured" — all org-allowed servers are accessible at the team level.
func (d *DB) GetTeamMCPAccess(ctx context.Context, teamID string) ([]string, error) {
	query := "SELECT server_id FROM team_mcp_access WHERE team_id = " + d.dialect.Placeholder(1) + " ORDER BY server_id"

	rows, err := d.sql.QueryContext(ctx, query, teamID)
	if err != nil {
		return nil, fmt.Errorf("get team mcp access %s: %w", teamID, err)
	}
	defer rows.Close()

	var serverIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("get team mcp access %s scan: %w", teamID, err)
		}
		serverIDs = append(serverIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get team mcp access %s rows: %w", teamID, err)
	}

	return serverIDs, nil
}

// SetTeamMCPAccess atomically replaces the MCP server allowlist for a team.
// An empty serverIDs slice clears the list, meaning all org-allowed servers are accessible.
func (d *DB) SetTeamMCPAccess(ctx context.Context, teamID string, serverIDs []string) error {
	p := d.dialect.Placeholder
	deleteQuery := "DELETE FROM team_mcp_access WHERE team_id = " + p(1)
	insertQuery := "INSERT INTO team_mcp_access (id, team_id, server_id) VALUES (" + p(1) + ", " + p(2) + ", " + p(3) + ")"

	err := d.WithTx(ctx, func(q Querier) error {
		if _, err := q.ExecContext(ctx, deleteQuery, teamID); err != nil {
			return fmt.Errorf("delete team mcp access: %w", err)
		}

		for _, serverID := range serverIDs {
			id, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("generate id: %w", err)
			}
			if _, err := q.ExecContext(ctx, insertQuery, id.String(), teamID, serverID); err != nil {
				return fmt.Errorf("insert team mcp access %q: %w", serverID, err)
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("set team mcp access %s: %w", teamID, err)
	}

	return nil
}

// GetKeyMCPAccess returns the allowed MCP server IDs for an API key, ordered by server_id.
// An empty slice means "not configured" — all team-allowed servers are accessible at the key level.
func (d *DB) GetKeyMCPAccess(ctx context.Context, keyID string) ([]string, error) {
	query := "SELECT server_id FROM key_mcp_access WHERE key_id = " + d.dialect.Placeholder(1) + " ORDER BY server_id"

	rows, err := d.sql.QueryContext(ctx, query, keyID)
	if err != nil {
		return nil, fmt.Errorf("get key mcp access %s: %w", keyID, err)
	}
	defer rows.Close()

	var serverIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("get key mcp access %s scan: %w", keyID, err)
		}
		serverIDs = append(serverIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get key mcp access %s rows: %w", keyID, err)
	}

	return serverIDs, nil
}

// SetKeyMCPAccess atomically replaces the MCP server allowlist for an API key.
// An empty serverIDs slice clears the list, meaning all team-allowed servers are accessible.
func (d *DB) SetKeyMCPAccess(ctx context.Context, keyID string, serverIDs []string) error {
	p := d.dialect.Placeholder
	deleteQuery := "DELETE FROM key_mcp_access WHERE key_id = " + p(1)
	insertQuery := "INSERT INTO key_mcp_access (id, key_id, server_id) VALUES (" + p(1) + ", " + p(2) + ", " + p(3) + ")"

	err := d.WithTx(ctx, func(q Querier) error {
		if _, err := q.ExecContext(ctx, deleteQuery, keyID); err != nil {
			return fmt.Errorf("delete key mcp access: %w", err)
		}

		for _, serverID := range serverIDs {
			id, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("generate id: %w", err)
			}
			if _, err := q.ExecContext(ctx, insertQuery, id.String(), keyID, serverID); err != nil {
				return fmt.Errorf("insert key mcp access %q: %w", serverID, err)
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("set key mcp access %s: %w", keyID, err)
	}

	return nil
}

// CheckMCPAccess reports whether serverID is accessible for the given org, team, and key.
// An empty allowlist at any level means "not configured" — all servers pass at that level.
// The most-restrictive-wins rule is applied: org → team → key.
// A non-empty teamID is required for team-level enforcement; pass "" to skip that level.
func (d *DB) CheckMCPAccess(ctx context.Context, orgID, teamID, keyID, serverID string) (bool, error) {
	orgAccess, err := d.GetOrgMCPAccess(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("check mcp access: %w", err)
	}
	if len(orgAccess) > 0 && !slices.Contains(orgAccess, serverID) {
		return false, nil
	}

	if teamID != "" {
		teamAccess, err := d.GetTeamMCPAccess(ctx, teamID)
		if err != nil {
			return false, fmt.Errorf("check mcp access: %w", err)
		}
		if len(teamAccess) > 0 && !slices.Contains(teamAccess, serverID) {
			return false, nil
		}
	}

	keyAccess, err := d.GetKeyMCPAccess(ctx, keyID)
	if err != nil {
		return false, fmt.Errorf("check mcp access: %w", err)
	}
	if len(keyAccess) > 0 && !slices.Contains(keyAccess, serverID) {
		return false, nil
	}

	return true, nil
}

// LoadAllMCPAccess returns all MCP access entries grouped by scope.
// It issues three queries (one per scope table) and returns maps from entity ID
// to the list of allowed server IDs. Entries with no rows are absent from the
// map (not an empty slice). Used to populate the in-memory access cache at
// startup and after each admin mutation.
func (d *DB) LoadAllMCPAccess(ctx context.Context) (
	orgAccess map[string][]string,
	teamAccess map[string][]string,
	keyAccess map[string][]string,
	err error,
) {
	orgAccess, err = loadAccessMap(ctx, d, "SELECT org_id, server_id FROM org_mcp_access")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load all mcp access (orgs): %w", err)
	}

	teamAccess, err = loadAccessMap(ctx, d, "SELECT team_id, server_id FROM team_mcp_access")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load all mcp access (teams): %w", err)
	}

	keyAccess, err = loadAccessMap(ctx, d, "SELECT key_id, server_id FROM key_mcp_access")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load all mcp access (keys): %w", err)
	}

	return orgAccess, teamAccess, keyAccess, nil
}
