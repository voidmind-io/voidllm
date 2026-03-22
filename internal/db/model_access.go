package db

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
)

// GetOrgModelAccess returns the allowed model names for an org, ordered alphabetically.
// An empty slice means "not configured" — all models are allowed at the org level.
func (d *DB) GetOrgModelAccess(ctx context.Context, orgID string) ([]string, error) {
	query := "SELECT model_name FROM org_model_access WHERE org_id = " + d.dialect.Placeholder(1) + " ORDER BY model_name"

	rows, err := d.sql.QueryContext(ctx, query, orgID)
	if err != nil {
		return nil, fmt.Errorf("get org model access %s: %w", orgID, err)
	}
	defer rows.Close()

	var models []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("get org model access %s scan: %w", orgID, err)
		}
		models = append(models, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get org model access %s rows: %w", orgID, err)
	}

	return models, nil
}

// GetTeamModelAccess returns the allowed model names for a team, ordered alphabetically.
// An empty slice means "not configured" — all org-allowed models are allowed at the team level.
func (d *DB) GetTeamModelAccess(ctx context.Context, teamID string) ([]string, error) {
	query := "SELECT model_name FROM team_model_access WHERE team_id = " + d.dialect.Placeholder(1) + " ORDER BY model_name"

	rows, err := d.sql.QueryContext(ctx, query, teamID)
	if err != nil {
		return nil, fmt.Errorf("get team model access %s: %w", teamID, err)
	}
	defer rows.Close()

	var models []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("get team model access %s scan: %w", teamID, err)
		}
		models = append(models, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get team model access %s rows: %w", teamID, err)
	}

	return models, nil
}

// GetKeyModelAccess returns the allowed model names for an API key, ordered alphabetically.
// An empty slice means "not configured" — all team-allowed models are allowed at the key level.
func (d *DB) GetKeyModelAccess(ctx context.Context, keyID string) ([]string, error) {
	query := "SELECT model_name FROM key_model_access WHERE key_id = " + d.dialect.Placeholder(1) + " ORDER BY model_name"

	rows, err := d.sql.QueryContext(ctx, query, keyID)
	if err != nil {
		return nil, fmt.Errorf("get key model access %s: %w", keyID, err)
	}
	defer rows.Close()

	var models []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("get key model access %s scan: %w", keyID, err)
		}
		models = append(models, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get key model access %s rows: %w", keyID, err)
	}

	return models, nil
}

// SetOrgModelAccess atomically replaces the model allowlist for an org.
// An empty models slice clears the list, meaning all models are allowed.
func (d *DB) SetOrgModelAccess(ctx context.Context, orgID string, models []string) error {
	p := d.dialect.Placeholder
	deleteQuery := "DELETE FROM org_model_access WHERE org_id = " + p(1)
	insertQuery := "INSERT INTO org_model_access (id, org_id, model_name) VALUES (" + p(1) + ", " + p(2) + ", " + p(3) + ")"

	err := d.WithTx(ctx, func(q Querier) error {
		if _, err := q.ExecContext(ctx, deleteQuery, orgID); err != nil {
			return fmt.Errorf("delete org model access: %w", err)
		}

		for _, model := range models {
			id, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("generate id: %w", err)
			}
			if _, err := q.ExecContext(ctx, insertQuery, id.String(), orgID, model); err != nil {
				return fmt.Errorf("insert org model access %q: %w", model, err)
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("set org model access %s: %w", orgID, err)
	}

	return nil
}

// SetTeamModelAccess atomically replaces the model allowlist for a team.
// An empty models slice clears the list, meaning all org-allowed models are allowed.
func (d *DB) SetTeamModelAccess(ctx context.Context, teamID string, models []string) error {
	p := d.dialect.Placeholder
	deleteQuery := "DELETE FROM team_model_access WHERE team_id = " + p(1)
	insertQuery := "INSERT INTO team_model_access (id, team_id, model_name) VALUES (" + p(1) + ", " + p(2) + ", " + p(3) + ")"

	err := d.WithTx(ctx, func(q Querier) error {
		if _, err := q.ExecContext(ctx, deleteQuery, teamID); err != nil {
			return fmt.Errorf("delete team model access: %w", err)
		}

		for _, model := range models {
			id, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("generate id: %w", err)
			}
			if _, err := q.ExecContext(ctx, insertQuery, id.String(), teamID, model); err != nil {
				return fmt.Errorf("insert team model access %q: %w", model, err)
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("set team model access %s: %w", teamID, err)
	}

	return nil
}

// SetKeyModelAccess atomically replaces the model allowlist for an API key.
// An empty models slice clears the list, meaning all team-allowed models are allowed.
func (d *DB) SetKeyModelAccess(ctx context.Context, keyID string, models []string) error {
	p := d.dialect.Placeholder
	deleteQuery := "DELETE FROM key_model_access WHERE key_id = " + p(1)
	insertQuery := "INSERT INTO key_model_access (id, key_id, model_name) VALUES (" + p(1) + ", " + p(2) + ", " + p(3) + ")"

	err := d.WithTx(ctx, func(q Querier) error {
		if _, err := q.ExecContext(ctx, deleteQuery, keyID); err != nil {
			return fmt.Errorf("delete key model access: %w", err)
		}

		for _, model := range models {
			id, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("generate id: %w", err)
			}
			if _, err := q.ExecContext(ctx, insertQuery, id.String(), keyID, model); err != nil {
				return fmt.Errorf("insert key model access %q: %w", model, err)
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("set key model access %s: %w", keyID, err)
	}

	return nil
}

// CheckModelAccess reports whether modelName is accessible for the given org, team, and key.
// An empty allowlist at any level means "not configured" — all models pass at that level.
// The most-restrictive-wins rule is applied: org → team → key.
// A non-empty teamID is required for team-level enforcement; pass "" to skip that level.
func (d *DB) CheckModelAccess(ctx context.Context, orgID, teamID, keyID, modelName string) (bool, error) {
	orgModels, err := d.GetOrgModelAccess(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("check model access: %w", err)
	}
	if len(orgModels) > 0 && !slices.Contains(orgModels, modelName) {
		return false, nil
	}

	if teamID != "" {
		teamModels, err := d.GetTeamModelAccess(ctx, teamID)
		if err != nil {
			return false, fmt.Errorf("check model access: %w", err)
		}
		if len(teamModels) > 0 && !slices.Contains(teamModels, modelName) {
			return false, nil
		}
	}

	keyModels, err := d.GetKeyModelAccess(ctx, keyID)
	if err != nil {
		return false, fmt.Errorf("check model access: %w", err)
	}
	if len(keyModels) > 0 && !slices.Contains(keyModels, modelName) {
		return false, nil
	}

	return true, nil
}

// LoadAllModelAccess returns all model access entries grouped by scope.
// It issues three queries (one per scope table) and returns maps from entity ID
// to the list of allowed model names. Entries with no rows are absent from the
// map (not an empty slice). Used to populate the in-memory access cache at
// startup and after each admin mutation.
func (d *DB) LoadAllModelAccess(ctx context.Context) (
	orgAccess map[string][]string,
	teamAccess map[string][]string,
	keyAccess map[string][]string,
	err error,
) {
	orgAccess, err = loadAccessMap(ctx, d, "SELECT org_id, model_name FROM org_model_access")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load all model access (orgs): %w", err)
	}

	teamAccess, err = loadAccessMap(ctx, d, "SELECT team_id, model_name FROM team_model_access")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load all model access (teams): %w", err)
	}

	keyAccess, err = loadAccessMap(ctx, d, "SELECT key_id, model_name FROM key_model_access")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load all model access (keys): %w", err)
	}

	return orgAccess, teamAccess, keyAccess, nil
}

// loadAccessMap executes a two-column query (id, model_name) and builds a map
// from entity ID to the accumulated list of model names.
func loadAccessMap(ctx context.Context, d *DB, query string) (map[string][]string, error) {
	rows, err := d.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string][]string)
	for rows.Next() {
		var id, modelName string
		if err := rows.Scan(&id, &modelName); err != nil {
			return nil, err
		}
		result[id] = append(result[id], modelName)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
