package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// modelAliasSelectColumns is the ordered column list used in all model_aliases SELECT queries.
// It must match the scan order in scanModelAlias exactly.
const modelAliasSelectColumns = "id, alias, model_name, scope_type, org_id, team_id, created_by, created_at, updated_at"

// ModelAlias represents a model alias record in the database.
// Aliases map a short name to a canonical model name within an org or team scope.
type ModelAlias struct {
	ID        string
	Alias     string
	ModelName string
	ScopeType string  // "org" or "team"
	OrgID     string
	TeamID    *string
	CreatedBy string
	CreatedAt string
	UpdatedAt string
}

// CreateModelAliasParams holds the input for creating a model alias.
type CreateModelAliasParams struct {
	Alias     string
	ModelName string
	ScopeType string
	OrgID     string
	TeamID    *string
	CreatedBy string
}

// CreateModelAlias inserts a new model alias and returns the persisted record.
// It returns ErrConflict if an alias with the same name already exists within the same scope.
func (d *DB) CreateModelAlias(ctx context.Context, params CreateModelAliasParams) (*ModelAlias, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("create model alias: generate id: %w", err)
	}

	p := d.dialect.Placeholder
	insertQuery := "INSERT INTO model_aliases " +
		"(id, alias, model_name, scope_type, org_id, team_id, created_by, created_at, updated_at) " +
		"VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " + p(5) + ", " + p(6) + ", " + p(7) + ", " +
		"CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"

	selectQuery := "SELECT " + modelAliasSelectColumns +
		" FROM model_aliases WHERE id = " + p(1)

	var alias *ModelAlias
	err = d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx, insertQuery,
			id.String(),
			params.Alias,
			params.ModelName,
			params.ScopeType,
			params.OrgID,
			params.TeamID,
			params.CreatedBy,
		)
		if execErr != nil {
			return translateError(execErr)
		}

		row := q.QueryRowContext(ctx, selectQuery, id.String())
		var scanErr error
		alias, scanErr = scanModelAlias(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("create model alias: %w", err)
	}
	return alias, nil
}

// ListModelAliases returns all aliases for the given scope, ordered by alias name.
// scopeType must be "org" or "team"; scopeID is the corresponding org or team ID.
func (d *DB) ListModelAliases(ctx context.Context, scopeType, scopeID string) ([]ModelAlias, error) {
	p := d.dialect.Placeholder
	var query string
	var args []any

	switch scopeType {
	case "org":
		query = "SELECT " + modelAliasSelectColumns +
			" FROM model_aliases WHERE scope_type = 'org' AND org_id = " + p(1) +
			" ORDER BY alias"
		args = []any{scopeID}
	case "team":
		query = "SELECT " + modelAliasSelectColumns +
			" FROM model_aliases WHERE scope_type = 'team' AND team_id = " + p(1) +
			" ORDER BY alias"
		args = []any{scopeID}
	default:
		return nil, fmt.Errorf("list model aliases: unsupported scope type %q", scopeType)
	}

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list model aliases %s %s query: %w", scopeType, scopeID, err)
	}
	defer rows.Close()

	var aliases []ModelAlias
	for rows.Next() {
		a, scanErr := scanModelAlias(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list model aliases %s %s scan: %w", scopeType, scopeID, scanErr)
		}
		aliases = append(aliases, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list model aliases %s %s rows: %w", scopeType, scopeID, err)
	}

	return aliases, nil
}

// DeleteModelAlias hard-deletes a model alias by its ID, scoped to the given
// owner. scopeType must be "org" or "team"; scopeID is the corresponding org or
// team ID. The WHERE clause enforces ownership so a caller cannot delete an
// alias that belongs to a different scope. It returns ErrNotFound if no alias
// matching both id and scope exists.
func (d *DB) DeleteModelAlias(ctx context.Context, id, scopeType, scopeID string) error {
	p := d.dialect.Placeholder
	var query string
	var args []any

	switch scopeType {
	case "org":
		query = "DELETE FROM model_aliases WHERE id = " + p(1) +
			" AND scope_type = 'org' AND org_id = " + p(2)
		args = []any{id, scopeID}
	case "team":
		query = "DELETE FROM model_aliases WHERE id = " + p(1) +
			" AND scope_type = 'team' AND team_id = " + p(2)
		args = []any{id, scopeID}
	default:
		return fmt.Errorf("delete model alias: unsupported scope type %q", scopeType)
	}

	result, err := d.sql.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("delete model alias %s: %w", id, translateError(err))
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete model alias %s rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("delete model alias %s: %w", id, ErrNotFound)
	}
	return nil
}

// LoadAllModelAliases returns all model aliases grouped into nested maps for cache population.
// orgAliases maps orgID → alias → canonical model name.
// teamAliases maps teamID → alias → canonical model name.
// It issues two queries (one per scope type) and is intended for use at startup and
// after each admin mutation that modifies the alias table.
func (d *DB) LoadAllModelAliases(ctx context.Context) (
	orgAliases map[string]map[string]string,
	teamAliases map[string]map[string]string,
	err error,
) {
	orgAliases, err = loadAliasMap(ctx, d,
		"SELECT org_id, alias, model_name FROM model_aliases WHERE scope_type = 'org'")
	if err != nil {
		return nil, nil, fmt.Errorf("load all model aliases (orgs): %w", err)
	}

	teamAliases, err = loadAliasMap(ctx, d,
		"SELECT team_id, alias, model_name FROM model_aliases WHERE scope_type = 'team'")
	if err != nil {
		return nil, nil, fmt.Errorf("load all model aliases (teams): %w", err)
	}

	return orgAliases, teamAliases, nil
}

// loadAliasMap executes a three-column query (scope_id, alias, model_name) and builds
// a nested map from scope ID to alias to canonical model name.
func loadAliasMap(ctx context.Context, d *DB, query string) (map[string]map[string]string, error) {
	rows, err := d.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]map[string]string)
	for rows.Next() {
		var scopeID, alias, modelName string
		if err := rows.Scan(&scopeID, &alias, &modelName); err != nil {
			return nil, err
		}
		if result[scopeID] == nil {
			result[scopeID] = make(map[string]string)
		}
		result[scopeID][alias] = modelName
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// scanModelAlias scans a single model_aliases row. The scanner may be a *sql.Row
// (from QueryRowContext) or *sql.Rows (from QueryContext); both satisfy the interface.
func scanModelAlias(scanner interface{ Scan(...any) error }) (*ModelAlias, error) {
	var a ModelAlias
	err := scanner.Scan(
		&a.ID, &a.Alias, &a.ModelName, &a.ScopeType, &a.OrgID, &a.TeamID,
		&a.CreatedBy, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &a, nil
}
