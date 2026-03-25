package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// deploymentSelectColumns is the ordered column list used in all model_deployments
// SELECT queries. It must match the scan order in scanDeployment exactly.
const deploymentSelectColumns = "id, model_id, name, provider, base_url, api_key_encrypted, " +
	"azure_deployment, azure_api_version, weight, priority, is_active, " +
	"created_at, updated_at, deleted_at"

// Deployment represents a row in the model_deployments table.
// Each deployment is a concrete upstream endpoint associated with a model.
// Multiple deployments on the same model enable load balancing strategies.
type Deployment struct {
	ID              string
	ModelID         string
	Name            string
	Provider        string
	BaseURL         string
	APIKeyEncrypted *string
	AzureDeployment string
	AzureAPIVersion string
	Weight          int
	Priority        int
	IsActive        bool
	CreatedAt       string
	UpdatedAt       string
	DeletedAt       *string
}

// CreateDeploymentParams holds the input for creating a deployment.
type CreateDeploymentParams struct {
	ModelID         string
	Name            string
	Provider        string
	BaseURL         string
	APIKeyEncrypted *string
	AzureDeployment string
	AzureAPIVersion string
	// Weight is the relative probability used for weighted routing. Must be >= 1.
	Weight int
	// Priority is the routing preference for the priority strategy; lower value
	// means higher priority. 0 is the highest priority.
	Priority int
}

// UpdateDeploymentParams holds optional fields for a partial deployment update.
// A nil pointer means the field is not changed.
type UpdateDeploymentParams struct {
	Name            *string
	Provider        *string
	BaseURL         *string
	APIKeyEncrypted *string
	AzureDeployment *string
	AzureAPIVersion *string
	// Weight, when non-nil, replaces the stored routing weight.
	Weight *int
	// Priority, when non-nil, replaces the stored routing priority.
	Priority *int
}

// CreateDeployment inserts a new deployment and returns the persisted record.
// It returns ErrConflict if a deployment with the same (model_id, name) pair already exists.
func (d *DB) CreateDeployment(ctx context.Context, params CreateDeploymentParams) (*Deployment, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("create deployment: generate id: %w", err)
	}

	weight := params.Weight
	if weight < 1 {
		weight = 1
	}

	p := d.dialect.Placeholder
	insertQuery := "INSERT INTO model_deployments " +
		"(id, model_id, name, provider, base_url, api_key_encrypted, " +
		"azure_deployment, azure_api_version, weight, priority, is_active, " +
		"created_at, updated_at) " +
		"VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " + p(5) + ", " + p(6) + ", " +
		p(7) + ", " + p(8) + ", " + p(9) + ", " + p(10) + ", " +
		"1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"

	selectQuery := "SELECT " + deploymentSelectColumns +
		" FROM model_deployments WHERE id = " + p(1) + " AND deleted_at IS NULL"

	var dep *Deployment
	err = d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx, insertQuery,
			id.String(),
			params.ModelID,
			params.Name,
			params.Provider,
			params.BaseURL,
			params.APIKeyEncrypted,
			params.AzureDeployment,
			params.AzureAPIVersion,
			weight,
			params.Priority,
		)
		if execErr != nil {
			return translateError(execErr)
		}

		row := q.QueryRowContext(ctx, selectQuery, id.String())
		var scanErr error
		dep, scanErr = scanDeployment(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("create deployment: %w", err)
	}
	return dep, nil
}

// GetDeployment retrieves an active deployment by its ID.
// It returns ErrNotFound if the deployment does not exist or has been soft-deleted.
func (d *DB) GetDeployment(ctx context.Context, id string) (*Deployment, error) {
	query := "SELECT " + deploymentSelectColumns +
		" FROM model_deployments WHERE id = " + d.dialect.Placeholder(1) + " AND deleted_at IS NULL"

	row := d.sql.QueryRowContext(ctx, query, id)
	dep, err := scanDeployment(row)
	if err != nil {
		return nil, fmt.Errorf("get deployment %s: %w", id, translateError(err))
	}
	return dep, nil
}

// ListDeployments returns all non-deleted deployments for the given model,
// ordered by priority ascending (lowest value first). Both active and inactive
// deployments are included. Use ListActiveDeployments for load-balancer paths.
func (d *DB) ListDeployments(ctx context.Context, modelID string) ([]Deployment, error) {
	query := "SELECT " + deploymentSelectColumns +
		" FROM model_deployments" +
		" WHERE model_id = " + d.dialect.Placeholder(1) + " AND deleted_at IS NULL" +
		" ORDER BY priority ASC, id ASC"

	rows, err := d.sql.QueryContext(ctx, query, modelID)
	if err != nil {
		return nil, fmt.Errorf("list deployments query: %w", err)
	}
	defer rows.Close()

	var deps []Deployment
	for rows.Next() {
		dep, scanErr := scanDeployment(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list deployments scan: %w", scanErr)
		}
		deps = append(deps, *dep)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list deployments rows: %w", err)
	}

	return deps, nil
}

// ListActiveDeployments returns all active (is_active=1), non-deleted deployments
// for the given model, ordered by priority ascending. This is the path used by
// the load balancer on every proxy request.
func (d *DB) ListActiveDeployments(ctx context.Context, modelID string) ([]Deployment, error) {
	query := "SELECT " + deploymentSelectColumns +
		" FROM model_deployments" +
		" WHERE model_id = " + d.dialect.Placeholder(1) +
		" AND is_active = 1 AND deleted_at IS NULL" +
		" ORDER BY priority ASC, id ASC"

	rows, err := d.sql.QueryContext(ctx, query, modelID)
	if err != nil {
		return nil, fmt.Errorf("list active deployments query: %w", err)
	}
	defer rows.Close()

	var deps []Deployment
	for rows.Next() {
		dep, scanErr := scanDeployment(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list active deployments scan: %w", scanErr)
		}
		deps = append(deps, *dep)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list active deployments rows: %w", err)
	}

	return deps, nil
}

// UpdateDeployment applies a partial update to an active deployment.
// Only non-nil fields in params are written. If all fields are nil the record
// is returned unchanged without issuing an UPDATE.
// It returns ErrNotFound if the deployment does not exist or has been soft-deleted,
// and ErrConflict if the new name collides with an existing deployment on the same model.
func (d *DB) UpdateDeployment(ctx context.Context, id string, params UpdateDeploymentParams) (*Deployment, error) {
	p := d.dialect.Placeholder
	argN := 1
	var setClauses []string
	var args []any

	if params.Name != nil {
		setClauses = append(setClauses, "name = "+p(argN))
		args = append(args, *params.Name)
		argN++
	}
	if params.Provider != nil {
		setClauses = append(setClauses, "provider = "+p(argN))
		args = append(args, *params.Provider)
		argN++
	}
	if params.BaseURL != nil {
		setClauses = append(setClauses, "base_url = "+p(argN))
		args = append(args, *params.BaseURL)
		argN++
	}
	if params.APIKeyEncrypted != nil {
		setClauses = append(setClauses, "api_key_encrypted = "+p(argN))
		args = append(args, *params.APIKeyEncrypted)
		argN++
	}
	if params.AzureDeployment != nil {
		setClauses = append(setClauses, "azure_deployment = "+p(argN))
		args = append(args, *params.AzureDeployment)
		argN++
	}
	if params.AzureAPIVersion != nil {
		setClauses = append(setClauses, "azure_api_version = "+p(argN))
		args = append(args, *params.AzureAPIVersion)
		argN++
	}
	if params.Weight != nil {
		setClauses = append(setClauses, "weight = "+p(argN))
		args = append(args, *params.Weight)
		argN++
	}
	if params.Priority != nil {
		setClauses = append(setClauses, "priority = "+p(argN))
		args = append(args, *params.Priority)
		argN++
	}

	if len(setClauses) == 0 {
		return d.GetDeployment(ctx, id)
	}

	setClauses = append(setClauses, "updated_at = CURRENT_TIMESTAMP")

	updateQuery := "UPDATE model_deployments SET " + strings.Join(setClauses, ", ") +
		" WHERE id = " + p(argN) + " AND deleted_at IS NULL"
	args = append(args, id)

	selectQuery := "SELECT " + deploymentSelectColumns +
		" FROM model_deployments WHERE id = " + p(1) + " AND deleted_at IS NULL"

	var dep *Deployment
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
		dep, scanErr = scanDeployment(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("update deployment %s: %w", id, err)
	}
	return dep, nil
}

// DeleteDeployment soft-deletes an active deployment by setting deleted_at.
// It returns ErrNotFound if the deployment does not exist or is already deleted.
func (d *DB) DeleteDeployment(ctx context.Context, id string) error {
	p := d.dialect.Placeholder
	query := "UPDATE model_deployments SET deleted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP " +
		"WHERE id = " + p(1) + " AND deleted_at IS NULL"

	result, err := d.sql.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("delete deployment %s: %w", id, translateError(err))
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete deployment %s rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("delete deployment %s: %w", id, ErrNotFound)
	}

	return nil
}

// scanDeployment scans a single deployment row. The scanner may be a *sql.Row
// (from QueryRowContext) or *sql.Rows (from QueryContext); both satisfy the interface.
func scanDeployment(scanner interface{ Scan(...any) error }) (*Deployment, error) {
	var dep Deployment
	var isActiveInt int
	err := scanner.Scan(
		&dep.ID, &dep.ModelID, &dep.Name, &dep.Provider, &dep.BaseURL, &dep.APIKeyEncrypted,
		&dep.AzureDeployment, &dep.AzureAPIVersion, &dep.Weight, &dep.Priority,
		&isActiveInt, &dep.CreatedAt, &dep.UpdatedAt, &dep.DeletedAt,
	)
	if err != nil {
		return nil, err
	}
	dep.IsActive = isActiveInt == 1
	return &dep, nil
}
