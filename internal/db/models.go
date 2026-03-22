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

// modelSelectColumns is the ordered column list used in all models SELECT queries.
// It must match the scan order in scanModel exactly.
const modelSelectColumns = "id, name, provider, base_url, api_key_encrypted, " +
	"max_context_tokens, input_price_per_1m, output_price_per_1m, " +
	"azure_deployment, azure_api_version, " +
	"is_active, source, created_by, created_at, updated_at, deleted_at, aliases, timeout"

// Model represents a model record in the database.
// This is the storage layer representation; see proxy.Model for the in-memory registry type.
type Model struct {
	ID               string
	Name             string
	Provider         string
	BaseURL          string
	APIKeyEncrypted  *string
	MaxContextTokens int
	InputPricePer1M  float64
	OutputPricePer1M float64
	AzureDeployment  string
	AzureAPIVersion  string
	IsActive         bool
	Source           string
	CreatedBy        *string
	CreatedAt        string
	UpdatedAt        string
	DeletedAt        *string
	// Aliases is a comma-separated list of alias names, e.g. "default,gpt4".
	// An empty string means no aliases are configured.
	Aliases string
	// Timeout is the per-model upstream timeout as a duration string (e.g. "30s", "2m").
	// An empty string means use the global default.
	Timeout string
}

// CreateModelParams holds the input for creating a model.
type CreateModelParams struct {
	Name             string
	Provider         string
	BaseURL          string
	APIKeyEncrypted  *string
	MaxContextTokens int
	InputPricePer1M  float64
	OutputPricePer1M float64
	AzureDeployment  string
	AzureAPIVersion  string
	// Source is "yaml" for config-file-sourced models or "api" for Admin API-created models.
	Source    string
	CreatedBy *string
	// Aliases is a comma-separated list of alias names, e.g. "default,gpt4".
	// Pass an empty string when no aliases are required.
	Aliases string
	// Timeout is the per-model upstream timeout as a duration string (e.g. "30s", "2m").
	// Pass an empty string to use the global default.
	Timeout string
}

// UpdateModelParams holds optional fields for updating a model.
// A nil pointer means the field is not changed.
type UpdateModelParams struct {
	Name             *string
	Provider         *string
	BaseURL          *string
	APIKeyEncrypted  *string
	MaxContextTokens *int
	InputPricePer1M  *float64
	OutputPricePer1M *float64
	AzureDeployment  *string
	AzureAPIVersion  *string
	// Aliases, when non-nil, replaces the stored comma-separated alias list.
	// Set to a pointer to an empty string to clear all aliases.
	Aliases *string
	// Timeout, when non-nil, replaces the stored timeout duration string.
	// Set to a pointer to an empty string to clear the per-model timeout.
	Timeout *string
}

// CreateModel inserts a new model and returns the persisted record.
// It returns ErrConflict if a model with the same name already exists.
func (d *DB) CreateModel(ctx context.Context, params CreateModelParams) (*Model, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("create model: generate id: %w", err)
	}

	p := d.dialect.Placeholder
	insertQuery := "INSERT INTO models " +
		"(id, name, provider, base_url, api_key_encrypted, " +
		"max_context_tokens, input_price_per_1m, output_price_per_1m, " +
		"azure_deployment, azure_api_version, " +
		"is_active, source, created_by, aliases, timeout, created_at, updated_at) " +
		"VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " + p(5) + ", " +
		p(6) + ", " + p(7) + ", " + p(8) + ", " +
		p(9) + ", " + p(10) + ", " +
		"1, " + p(11) + ", " + p(12) + ", " + p(13) + ", " + p(14) + ", " +
		"CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"

	selectQuery := "SELECT " + modelSelectColumns +
		" FROM models WHERE id = " + p(1) + " AND deleted_at IS NULL"

	var model *Model
	err = d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx, insertQuery,
			id.String(),
			params.Name,
			params.Provider,
			params.BaseURL,
			params.APIKeyEncrypted,
			params.MaxContextTokens,
			params.InputPricePer1M,
			params.OutputPricePer1M,
			params.AzureDeployment,
			params.AzureAPIVersion,
			params.Source,
			params.CreatedBy,
			params.Aliases,
			params.Timeout,
		)
		if execErr != nil {
			return translateError(execErr)
		}

		row := q.QueryRowContext(ctx, selectQuery, id.String())
		var scanErr error
		model, scanErr = scanModel(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("create model: %w", err)
	}
	return model, nil
}

// GetModel retrieves an active model by its ID.
// It returns ErrNotFound if the model does not exist or has been soft-deleted.
func (d *DB) GetModel(ctx context.Context, id string) (*Model, error) {
	query := "SELECT " + modelSelectColumns +
		" FROM models WHERE id = " + d.dialect.Placeholder(1) + " AND deleted_at IS NULL"

	row := d.sql.QueryRowContext(ctx, query, id)
	model, err := scanModel(row)
	if err != nil {
		return nil, fmt.Errorf("get model %s: %w", id, translateError(err))
	}
	return model, nil
}

// GetModelByName retrieves an active model by its canonical name.
// It returns ErrNotFound if the model does not exist or has been soft-deleted.
func (d *DB) GetModelByName(ctx context.Context, name string) (*Model, error) {
	query := "SELECT " + modelSelectColumns +
		" FROM models WHERE name = " + d.dialect.Placeholder(1) + " AND deleted_at IS NULL"

	row := d.sql.QueryRowContext(ctx, query, name)
	model, err := scanModel(row)
	if err != nil {
		return nil, fmt.Errorf("get model by name %q: %w", name, translateError(err))
	}
	return model, nil
}

// ListModels returns a page of models ordered by ID ascending.
// cursor is an exclusive lower bound on ID for keyset pagination; pass "" to start from the beginning.
// limit controls the maximum number of records returned.
// includeInactive controls whether models with is_active=0 are included.
// Soft-deleted models are never returned.
func (d *DB) ListModels(ctx context.Context, cursor string, limit int, includeInactive bool) ([]Model, error) {
	p := d.dialect.Placeholder
	argN := 1
	var conditions []string
	var args []any

	conditions = append(conditions, "deleted_at IS NULL")

	if !includeInactive {
		conditions = append(conditions, "is_active = 1")
	}
	if cursor != "" {
		conditions = append(conditions, "id > "+p(argN))
		args = append(args, cursor)
		argN++
	}

	query := "SELECT " + modelSelectColumns + " FROM models" +
		" WHERE " + strings.Join(conditions, " AND ") +
		" ORDER BY id ASC LIMIT " + p(argN)
	args = append(args, limit)

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list models query: %w", err)
	}
	defer rows.Close()

	var models []Model
	for rows.Next() {
		m, scanErr := scanModel(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list models scan: %w", scanErr)
		}
		models = append(models, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list models rows: %w", err)
	}

	return models, nil
}

// UpdateModel applies a partial update to an active model.
// Only non-nil fields in params are written. If all fields are nil the record
// is returned unchanged without issuing an UPDATE.
// It returns ErrNotFound if the model does not exist or has been soft-deleted,
// and ErrConflict if the new name collides with an existing model name.
func (d *DB) UpdateModel(ctx context.Context, id string, params UpdateModelParams) (*Model, error) {
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
	if params.MaxContextTokens != nil {
		setClauses = append(setClauses, "max_context_tokens = "+p(argN))
		args = append(args, *params.MaxContextTokens)
		argN++
	}
	if params.InputPricePer1M != nil {
		setClauses = append(setClauses, "input_price_per_1m = "+p(argN))
		args = append(args, *params.InputPricePer1M)
		argN++
	}
	if params.OutputPricePer1M != nil {
		setClauses = append(setClauses, "output_price_per_1m = "+p(argN))
		args = append(args, *params.OutputPricePer1M)
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
	if params.Aliases != nil {
		setClauses = append(setClauses, "aliases = "+p(argN))
		args = append(args, *params.Aliases)
		argN++
	}
	if params.Timeout != nil {
		setClauses = append(setClauses, "timeout = "+p(argN))
		args = append(args, *params.Timeout)
		argN++
	}

	if len(setClauses) == 0 {
		return d.GetModel(ctx, id)
	}

	setClauses = append(setClauses, "updated_at = CURRENT_TIMESTAMP")

	updateQuery := "UPDATE models SET " + strings.Join(setClauses, ", ") +
		" WHERE id = " + p(argN) + " AND deleted_at IS NULL"
	args = append(args, id)

	selectQuery := "SELECT " + modelSelectColumns +
		" FROM models WHERE id = " + p(1) + " AND deleted_at IS NULL"

	var model *Model
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
		model, scanErr = scanModel(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("update model %s: %w", id, err)
	}
	return model, nil
}

// DeleteModel soft-deletes an active model by setting deleted_at.
// It returns ErrNotFound if the model does not exist or is already deleted.
func (d *DB) DeleteModel(ctx context.Context, id string) error {
	p := d.dialect.Placeholder
	query := "UPDATE models SET deleted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP " +
		"WHERE id = " + p(1) + " AND deleted_at IS NULL"

	result, err := d.sql.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("delete model %s: %w", id, translateError(err))
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete model %s rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("delete model %s: %w", id, ErrNotFound)
	}

	return nil
}

// ActivateModel sets is_active = 1 for the given model.
// It returns ErrNotFound if the model does not exist or has been soft-deleted.
func (d *DB) ActivateModel(ctx context.Context, id string) error {
	p := d.dialect.Placeholder
	query := "UPDATE models SET is_active = 1, updated_at = CURRENT_TIMESTAMP " +
		"WHERE id = " + p(1) + " AND deleted_at IS NULL"

	result, err := d.sql.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("activate model %s: %w", id, translateError(err))
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("activate model %s rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("activate model %s: %w", id, ErrNotFound)
	}

	return nil
}

// DeactivateModel sets is_active = 0 for the given model.
// It returns ErrNotFound if the model does not exist or has been soft-deleted.
func (d *DB) DeactivateModel(ctx context.Context, id string) error {
	p := d.dialect.Placeholder
	query := "UPDATE models SET is_active = 0, updated_at = CURRENT_TIMESTAMP " +
		"WHERE id = " + p(1) + " AND deleted_at IS NULL"

	result, err := d.sql.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("deactivate model %s: %w", id, translateError(err))
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("deactivate model %s rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("deactivate model %s: %w", id, ErrNotFound)
	}

	return nil
}

// ListActiveModels returns all active, non-deleted models with no pagination.
// This is intended for the registry reload path only; it should not be called
// on the hot proxy path.
func (d *DB) ListActiveModels(ctx context.Context) ([]Model, error) {
	query := "SELECT " + modelSelectColumns +
		" FROM models WHERE is_active = 1 AND deleted_at IS NULL ORDER BY id ASC"

	rows, err := d.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list active models query: %w", err)
	}
	defer rows.Close()

	var models []Model
	for rows.Next() {
		m, scanErr := scanModel(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list active models scan: %w", scanErr)
		}
		models = append(models, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list active models rows: %w", err)
	}

	return models, nil
}

// scanModel scans a single model row. The scanner may be a *sql.Row (from
// QueryRowContext) or *sql.Rows (from QueryContext); both satisfy the interface.
func scanModel(scanner interface{ Scan(...any) error }) (*Model, error) {
	var m Model
	var isActiveInt int
	err := scanner.Scan(
		&m.ID, &m.Name, &m.Provider, &m.BaseURL, &m.APIKeyEncrypted,
		&m.MaxContextTokens, &m.InputPricePer1M, &m.OutputPricePer1M,
		&m.AzureDeployment, &m.AzureAPIVersion,
		&isActiveInt, &m.Source, &m.CreatedBy,
		&m.CreatedAt, &m.UpdatedAt, &m.DeletedAt, &m.Aliases, &m.Timeout,
	)
	if err != nil {
		return nil, err
	}
	m.IsActive = isActiveInt == 1
	return &m, nil
}

// SyncYAMLModels upserts YAML-configured models into the database.
//
// For each model in the provided slice:
//   - If the model is not present in the DB it is created with source="yaml".
//   - If the model exists with source="yaml" it is updated to reflect the
//     current YAML values.
//   - If the model exists with source="api" it is left untouched; API-created
//     models take precedence over YAML configuration.
//
// When a model entry carries an API key it is encrypted with AES-256-GCM
// before being written to the database. The model's database ID is used as
// additional authenticated data (AAD) so the ciphertext is bound to that
// specific row; for newly created models the key is written in a separate
// UPDATE after the INSERT returns the generated ID.
//
// encKey must be a 32-byte AES-256 key (see crypto.ParseKey).
func (d *DB) SyncYAMLModels(ctx context.Context, models []config.ModelConfig, encKey []byte) error {
	aliasOwner := make(map[string]string)
	for _, m := range models {
		for _, a := range m.Aliases {
			if owner, exists := aliasOwner[a]; exists {
				return fmt.Errorf("sync yaml models: duplicate alias %q in models %q and %q", a, owner, m.Name)
			}
			aliasOwner[a] = m.Name
		}
	}

	for _, m := range models {
		existing, err := d.GetModelByName(ctx, m.Name)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("sync yaml models: check %s: %w", m.Name, err)
		}

		if errors.Is(err, ErrNotFound) {
			// Model is not in the DB — create it with source="yaml".
			aliases := strings.Join(m.Aliases, ",")
			created, createErr := d.CreateModel(ctx, CreateModelParams{
				Name:             m.Name,
				Provider:         m.Provider,
				BaseURL:          m.BaseURL,
				MaxContextTokens: m.MaxContextTokens,
				InputPricePer1M:  m.Pricing.InputPer1M,
				OutputPricePer1M: m.Pricing.OutputPer1M,
				AzureDeployment:  m.AzureDeployment,
				AzureAPIVersion:  m.AzureAPIVersion,
				Source:           "yaml",
				Aliases:          aliases,
				Timeout:          m.Timeout,
			})
			if createErr != nil {
				return fmt.Errorf("sync yaml models: create %s: %w", m.Name, createErr)
			}

			// Encrypt the API key now that we have the model ID to use as AAD,
			// then store it in a follow-up UPDATE.
			if m.APIKey != "" {
				enc, encErr := crypto.EncryptString(m.APIKey, encKey, []byte("model:"+created.ID))
				if encErr != nil {
					return fmt.Errorf("sync yaml models: encrypt key for %s: %w", m.Name, encErr)
				}
				if _, updateErr := d.UpdateModel(ctx, created.ID, UpdateModelParams{
					APIKeyEncrypted: &enc,
				}); updateErr != nil {
					return fmt.Errorf("sync yaml models: set key for %s: %w", m.Name, updateErr)
				}
			}
			continue
		}

		// Model exists in DB — skip if it was created via the Admin API.
		if existing.Source != "yaml" {
			continue
		}

		// source="yaml" — update with the current YAML values.
		aliases := strings.Join(m.Aliases, ",")
		name := m.Name
		provider := m.Provider
		baseURL := m.BaseURL
		maxCtx := m.MaxContextTokens
		inputPrice := m.Pricing.InputPer1M
		outputPrice := m.Pricing.OutputPer1M
		azureDeploy := m.AzureDeployment
		azureVersion := m.AzureAPIVersion
		timeout := m.Timeout

		params := UpdateModelParams{
			Name:             &name,
			Provider:         &provider,
			BaseURL:          &baseURL,
			MaxContextTokens: &maxCtx,
			InputPricePer1M:  &inputPrice,
			OutputPricePer1M: &outputPrice,
			AzureDeployment:  &azureDeploy,
			AzureAPIVersion:  &azureVersion,
			Aliases:          &aliases,
			Timeout:          &timeout,
		}

		if m.APIKey != "" {
			enc, encErr := crypto.EncryptString(m.APIKey, encKey, []byte("model:"+existing.ID))
			if encErr != nil {
				return fmt.Errorf("sync yaml models: encrypt key for %s: %w", m.Name, encErr)
			}
			params.APIKeyEncrypted = &enc
		} else if existing.APIKeyEncrypted != nil {
			empty := ""
			params.APIKeyEncrypted = &empty
		}

		if _, updateErr := d.UpdateModel(ctx, existing.ID, params); updateErr != nil {
			return fmt.Errorf("sync yaml models: update %s: %w", m.Name, updateErr)
		}
	}
	return nil
}

