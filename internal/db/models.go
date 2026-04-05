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
const modelSelectColumns = "id, name, provider, model_type, base_url, api_key_encrypted, " +
	"max_context_tokens, input_price_per_1m, output_price_per_1m, " +
	"azure_deployment, azure_api_version, gcp_project, gcp_location, " +
	"aws_region, aws_access_key_enc, aws_secret_key_enc, aws_session_token_enc, " +
	"is_active, source, created_by, created_at, updated_at, deleted_at, aliases, timeout, " +
	"strategy, max_retries"

// Model represents a model record in the database.
// This is the storage layer representation; see proxy.Model for the in-memory registry type.
type Model struct {
	ID       string
	Name     string
	Provider string
	// "completion", "image", "audio_transcription", or "tts". Defaults to "chat".
	ModelType        string
	BaseURL          string
	APIKeyEncrypted  *string
	MaxContextTokens int
	InputPricePer1M  float64
	OutputPricePer1M float64
	AzureDeployment  string
	AzureAPIVersion  string
	// GCPProject is the Google Cloud project ID. Non-empty only for provider "vertex".
	GCPProject string
	// GCPLocation is the Google Cloud region. Non-empty only for provider "vertex".
	GCPLocation string
	// AWSRegion is the AWS region. Non-empty only for provider "bedrock-converse".
	AWSRegion string
	// AWSAccessKeyEnc is the AES-256-GCM encrypted AWS IAM access key ID.
	// Non-nil only for provider "bedrock-converse".
	AWSAccessKeyEnc *string
	// AWSSecretKeyEnc is the AES-256-GCM encrypted AWS IAM secret access key.
	// Non-nil only for provider "bedrock-converse".
	AWSSecretKeyEnc *string
	// AWSSessionTokenEnc is the AES-256-GCM encrypted AWS STS session token.
	// Non-nil when temporary credentials are configured.
	AWSSessionTokenEnc *string
	IsActive           bool
	Source             string
	CreatedBy          *string
	CreatedAt          string
	UpdatedAt          string
	DeletedAt          *string
	// Aliases is a comma-separated list of alias names, e.g. "default,gpt4".
	// An empty string means no aliases are configured.
	Aliases string
	// Timeout is the per-model upstream timeout as a duration string (e.g. "30s", "2m").
	// An empty string means use the global default.
	Timeout string
	// Strategy is the load balancing strategy used when multiple deployments are
	// configured. Valid values: "round-robin", "weighted", "priority". An empty
	// string means single-deployment (legacy) mode.
	Strategy string
	// MaxRetries is the maximum number of deployments to attempt before returning
	// an error to the caller. 0 means try all available deployments.
	MaxRetries int
}

// CreateModelParams holds the input for creating a model.
type CreateModelParams struct {
	Name             string
	Provider         string
	ModelType        *string
	BaseURL          string
	APIKeyEncrypted  *string
	MaxContextTokens int
	InputPricePer1M  float64
	OutputPricePer1M float64
	AzureDeployment  string
	AzureAPIVersion  string
	// GCPProject is the Google Cloud project ID. Required when Provider is "vertex".
	GCPProject string
	// GCPLocation is the Google Cloud region. Required when Provider is "vertex".
	GCPLocation string
	// AWSRegion is the AWS region. Required when Provider is "bedrock-converse".
	AWSRegion string
	// AWSAccessKeyEnc is the AES-256-GCM encrypted AWS IAM access key ID.
	AWSAccessKeyEnc *string
	// AWSSecretKeyEnc is the AES-256-GCM encrypted AWS IAM secret access key.
	AWSSecretKeyEnc *string
	// AWSSessionTokenEnc is the AES-256-GCM encrypted AWS STS session token.
	AWSSessionTokenEnc *string
	// Source is "yaml" for config-file-sourced models or "api" for Admin API-created models.
	Source    string
	CreatedBy *string
	// Aliases is a comma-separated list of alias names, e.g. "default,gpt4".
	// Pass an empty string when no aliases are required.
	Aliases string
	// Timeout is the per-model upstream timeout as a duration string (e.g. "30s", "2m").
	// Pass an empty string to use the global default.
	Timeout string
	// Strategy is the load balancing strategy. Pass an empty string for
	// single-deployment (legacy) mode.
	Strategy *string
	// MaxRetries is the maximum number of deployments to attempt. 0 means try all.
	MaxRetries *int
}

// UpdateModelParams holds optional fields for updating a model.
// A nil pointer means the field is not changed.
type UpdateModelParams struct {
	Name             *string
	Provider         *string
	ModelType        *string
	BaseURL          *string
	APIKeyEncrypted  *string
	MaxContextTokens *int
	InputPricePer1M  *float64
	OutputPricePer1M *float64
	AzureDeployment  *string
	AzureAPIVersion  *string
	// GCPProject, when non-nil, replaces the stored Google Cloud project ID.
	GCPProject *string
	// GCPLocation, when non-nil, replaces the stored Google Cloud region.
	GCPLocation *string
	// AWSRegion, when non-nil, replaces the stored AWS region.
	AWSRegion *string
	// AWSAccessKeyEnc, when non-nil, replaces the stored encrypted AWS access key.
	AWSAccessKeyEnc *string
	// AWSSecretKeyEnc, when non-nil, replaces the stored encrypted AWS secret key.
	AWSSecretKeyEnc *string
	// AWSSessionTokenEnc, when non-nil, replaces the stored encrypted session token.
	// Set to a pointer to an empty string to clear the session token.
	AWSSessionTokenEnc *string
	// Aliases, when non-nil, replaces the stored comma-separated alias list.
	// Set to a pointer to an empty string to clear all aliases.
	Aliases *string
	// Timeout, when non-nil, replaces the stored timeout duration string.
	// Set to a pointer to an empty string to clear the per-model timeout.
	Timeout *string
	// Strategy, when non-nil, replaces the stored load balancing strategy.
	// Set to a pointer to an empty string to revert to single-deployment mode.
	Strategy *string
	// MaxRetries, when non-nil, replaces the stored retry count.
	MaxRetries *int
}

// CreateModel inserts a new model and returns the persisted record.
// It returns ErrConflict if a model with the same name already exists.
func (d *DB) CreateModel(ctx context.Context, params CreateModelParams) (*Model, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("create model: generate id: %w", err)
	}

	modelType := "chat"
	if params.ModelType != nil && *params.ModelType != "" {
		modelType = *params.ModelType
	}

	strategy := ""
	if params.Strategy != nil {
		strategy = *params.Strategy
	}
	maxRetries := 0
	if params.MaxRetries != nil {
		maxRetries = *params.MaxRetries
	}

	p := d.dialect.Placeholder
	insertQuery := "INSERT INTO models " +
		"(id, name, provider, model_type, base_url, api_key_encrypted, " +
		"max_context_tokens, input_price_per_1m, output_price_per_1m, " +
		"azure_deployment, azure_api_version, gcp_project, gcp_location, " +
		"aws_region, aws_access_key_enc, aws_secret_key_enc, aws_session_token_enc, " +
		"is_active, source, created_by, aliases, timeout, strategy, max_retries, " +
		"created_at, updated_at) " +
		"VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " + p(5) + ", " + p(6) + ", " +
		p(7) + ", " + p(8) + ", " + p(9) + ", " +
		p(10) + ", " + p(11) + ", " + p(12) + ", " + p(13) + ", " +
		p(14) + ", " + p(15) + ", " + p(16) + ", " + p(17) + ", " +
		"1, " + p(18) + ", " + p(19) + ", " + p(20) + ", " + p(21) + ", " + p(22) + ", " + p(23) + ", " +
		"CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"

	selectQuery := "SELECT " + modelSelectColumns +
		" FROM models WHERE id = " + p(1) + " AND deleted_at IS NULL"

	var model *Model
	err = d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx, insertQuery,
			id.String(),
			params.Name,
			params.Provider,
			modelType,
			params.BaseURL,
			params.APIKeyEncrypted,
			params.MaxContextTokens,
			params.InputPricePer1M,
			params.OutputPricePer1M,
			params.AzureDeployment,
			params.AzureAPIVersion,
			params.GCPProject,
			params.GCPLocation,
			params.AWSRegion,
			params.AWSAccessKeyEnc,
			params.AWSSecretKeyEnc,
			params.AWSSessionTokenEnc,
			params.Source,
			params.CreatedBy,
			params.Aliases,
			params.Timeout,
			strategy,
			maxRetries,
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
	if params.ModelType != nil {
		setClauses = append(setClauses, "model_type = "+p(argN))
		args = append(args, *params.ModelType)
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
	if params.GCPProject != nil {
		setClauses = append(setClauses, "gcp_project = "+p(argN))
		args = append(args, *params.GCPProject)
		argN++
	}
	if params.GCPLocation != nil {
		setClauses = append(setClauses, "gcp_location = "+p(argN))
		args = append(args, *params.GCPLocation)
		argN++
	}
	if params.AWSRegion != nil {
		setClauses = append(setClauses, "aws_region = "+p(argN))
		args = append(args, *params.AWSRegion)
		argN++
	}
	if params.AWSAccessKeyEnc != nil {
		setClauses = append(setClauses, "aws_access_key_enc = "+p(argN))
		args = append(args, *params.AWSAccessKeyEnc)
		argN++
	}
	if params.AWSSecretKeyEnc != nil {
		setClauses = append(setClauses, "aws_secret_key_enc = "+p(argN))
		args = append(args, *params.AWSSecretKeyEnc)
		argN++
	}
	if params.AWSSessionTokenEnc != nil {
		setClauses = append(setClauses, "aws_session_token_enc = "+p(argN))
		args = append(args, *params.AWSSessionTokenEnc)
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
	if params.Strategy != nil {
		setClauses = append(setClauses, "strategy = "+p(argN))
		args = append(args, *params.Strategy)
		argN++
	}
	if params.MaxRetries != nil {
		setClauses = append(setClauses, "max_retries = "+p(argN))
		args = append(args, *params.MaxRetries)
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
		&m.ID, &m.Name, &m.Provider, &m.ModelType, &m.BaseURL, &m.APIKeyEncrypted,
		&m.MaxContextTokens, &m.InputPricePer1M, &m.OutputPricePer1M,
		&m.AzureDeployment, &m.AzureAPIVersion, &m.GCPProject, &m.GCPLocation,
		&m.AWSRegion, &m.AWSAccessKeyEnc, &m.AWSSecretKeyEnc, &m.AWSSessionTokenEnc,
		&isActiveInt, &m.Source, &m.CreatedBy,
		&m.CreatedAt, &m.UpdatedAt, &m.DeletedAt, &m.Aliases, &m.Timeout,
		&m.Strategy, &m.MaxRetries,
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
			modelType := m.Type
			if modelType == "" {
				modelType = "chat"
			}
			strategy := m.Strategy
			maxRetries := m.MaxRetries
			created, createErr := d.CreateModel(ctx, CreateModelParams{
				Name:             m.Name,
				Provider:         m.Provider,
				ModelType:        &modelType,
				BaseURL:          m.BaseURL,
				MaxContextTokens: m.MaxContextTokens,
				InputPricePer1M:  m.Pricing.InputPer1M,
				OutputPricePer1M: m.Pricing.OutputPer1M,
				AzureDeployment:  m.AzureDeployment,
				AzureAPIVersion:  m.AzureAPIVersion,
				GCPProject:       m.GCPProject,
				GCPLocation:      m.GCPLocation,
				AWSRegion:        m.AWSRegion,
				Source:           "yaml",
				Aliases:          aliases,
				Timeout:          m.Timeout,
				Strategy:         &strategy,
				MaxRetries:       &maxRetries,
			})
			if createErr != nil {
				return fmt.Errorf("sync yaml models: create %s: %w", m.Name, createErr)
			}

			// Encrypt secrets now that we have the model ID to use as AAD,
			// then store them in a follow-up UPDATE.
			updateAfterCreate := UpdateModelParams{}
			needsUpdate := false
			if m.APIKey != "" {
				enc, encErr := crypto.EncryptString(m.APIKey, encKey, []byte("model:"+created.ID))
				if encErr != nil {
					return fmt.Errorf("sync yaml models: encrypt key for %s: %w", m.Name, encErr)
				}
				updateAfterCreate.APIKeyEncrypted = &enc
				needsUpdate = true
			}
			if m.AWSAccessKey != "" {
				enc, encErr := crypto.EncryptString(m.AWSAccessKey, encKey, []byte("model-aws-access:"+created.ID))
				if encErr != nil {
					return fmt.Errorf("sync yaml models: encrypt aws access key for %s: %w", m.Name, encErr)
				}
				updateAfterCreate.AWSAccessKeyEnc = &enc
				needsUpdate = true
			}
			if m.AWSSecretKey != "" {
				enc, encErr := crypto.EncryptString(m.AWSSecretKey, encKey, []byte("model-aws-secret:"+created.ID))
				if encErr != nil {
					return fmt.Errorf("sync yaml models: encrypt aws secret key for %s: %w", m.Name, encErr)
				}
				updateAfterCreate.AWSSecretKeyEnc = &enc
				needsUpdate = true
			}
			if m.AWSSessionToken != "" {
				enc, encErr := crypto.EncryptString(m.AWSSessionToken, encKey, []byte("model-aws-session:"+created.ID))
				if encErr != nil {
					return fmt.Errorf("sync yaml models: encrypt aws session token for %s: %w", m.Name, encErr)
				}
				updateAfterCreate.AWSSessionTokenEnc = &enc
				needsUpdate = true
			}
			if needsUpdate {
				if _, updateErr := d.UpdateModel(ctx, created.ID, updateAfterCreate); updateErr != nil {
					return fmt.Errorf("sync yaml models: set secrets for %s: %w", m.Name, updateErr)
				}
			}

			// Sync deployments for the newly created model.
			if syncErr := d.syncYAMLDeployments(ctx, created.ID, m.Deployments, encKey); syncErr != nil {
				return fmt.Errorf("sync yaml models: deployments for %s: %w", m.Name, syncErr)
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
		modelType := m.Type
		if modelType == "" {
			modelType = "chat"
		}
		baseURL := m.BaseURL
		maxCtx := m.MaxContextTokens
		inputPrice := m.Pricing.InputPer1M
		outputPrice := m.Pricing.OutputPer1M
		azureDeploy := m.AzureDeployment
		azureVersion := m.AzureAPIVersion
		gcpProject := m.GCPProject
		gcpLocation := m.GCPLocation
		awsRegion := m.AWSRegion
		timeout := m.Timeout
		strategy := m.Strategy
		maxRetries := m.MaxRetries

		params := UpdateModelParams{
			Name:             &name,
			Provider:         &provider,
			ModelType:        &modelType,
			BaseURL:          &baseURL,
			MaxContextTokens: &maxCtx,
			InputPricePer1M:  &inputPrice,
			OutputPricePer1M: &outputPrice,
			AzureDeployment:  &azureDeploy,
			AzureAPIVersion:  &azureVersion,
			GCPProject:       &gcpProject,
			GCPLocation:      &gcpLocation,
			AWSRegion:        &awsRegion,
			Aliases:          &aliases,
			Timeout:          &timeout,
			Strategy:         &strategy,
			MaxRetries:       &maxRetries,
		}

		if m.APIKey != "" {
			enc, encErr := crypto.EncryptString(m.APIKey, encKey, []byte("model:"+existing.ID))
			if encErr != nil {
				return fmt.Errorf("sync yaml models: encrypt key for %s: %w", m.Name, encErr)
			}
			params.APIKeyEncrypted = &enc
		}
		if m.AWSAccessKey != "" {
			enc, encErr := crypto.EncryptString(m.AWSAccessKey, encKey, []byte("model-aws-access:"+existing.ID))
			if encErr != nil {
				return fmt.Errorf("sync yaml models: encrypt aws access key for %s: %w", m.Name, encErr)
			}
			params.AWSAccessKeyEnc = &enc
		}
		if m.AWSSecretKey != "" {
			enc, encErr := crypto.EncryptString(m.AWSSecretKey, encKey, []byte("model-aws-secret:"+existing.ID))
			if encErr != nil {
				return fmt.Errorf("sync yaml models: encrypt aws secret key for %s: %w", m.Name, encErr)
			}
			params.AWSSecretKeyEnc = &enc
		}
		if m.AWSSessionToken != "" {
			enc, encErr := crypto.EncryptString(m.AWSSessionToken, encKey, []byte("model-aws-session:"+existing.ID))
			if encErr != nil {
				return fmt.Errorf("sync yaml models: encrypt aws session token for %s: %w", m.Name, encErr)
			}
			params.AWSSessionTokenEnc = &enc
		}

		if _, updateErr := d.UpdateModel(ctx, existing.ID, params); updateErr != nil {
			return fmt.Errorf("sync yaml models: update %s: %w", m.Name, updateErr)
		}

		// Sync deployments for the existing model.
		if syncErr := d.syncYAMLDeployments(ctx, existing.ID, m.Deployments, encKey); syncErr != nil {
			return fmt.Errorf("sync yaml models: deployments for %s: %w", m.Name, syncErr)
		}
	}
	return nil
}

// syncYAMLDeployments reconciles the deployments stored in the DB for a model
// against the list declared in the YAML configuration. It creates deployments
// that do not yet exist (matched by name), updates those that have changed, and
// soft-deletes those that are no longer present in the YAML. API keys are
// encrypted using AES-256-GCM before being written; the deployment's own ID is
// used as additional authenticated data.
func (d *DB) syncYAMLDeployments(ctx context.Context, modelID string, cfgDeps []config.DeploymentConfig, encKey []byte) error {
	dbDeps, err := d.ListDeployments(ctx, modelID)
	if err != nil {
		return fmt.Errorf("list deployments: %w", err)
	}

	// Index existing DB deployments by name for O(1) lookup.
	byName := make(map[string]Deployment, len(dbDeps))
	for _, dep := range dbDeps {
		byName[dep.Name] = dep
	}

	// Track which DB deployment names are present in the YAML config so we can
	// soft-delete the remainder afterwards.
	seen := make(map[string]bool, len(cfgDeps))

	for _, cd := range cfgDeps {
		seen[cd.Name] = true

		existing, exists := byName[cd.Name]
		if !exists {
			// Deployment is new — create it. Encrypt the API key after creation
			// so we have the generated ID available as AAD.
			weight := cd.Weight
			if weight < 1 {
				weight = 1
			}
			created, createErr := d.CreateDeployment(ctx, CreateDeploymentParams{
				ModelID:         modelID,
				Name:            cd.Name,
				Provider:        cd.Provider,
				BaseURL:         cd.BaseURL,
				AzureDeployment: cd.AzureDeployment,
				AzureAPIVersion: cd.AzureAPIVersion,
				GCPProject:      cd.GCPProject,
				GCPLocation:     cd.GCPLocation,
				AWSRegion:       cd.AWSRegion,
				Weight:          weight,
				Priority:        cd.Priority,
			})
			if createErr != nil {
				return fmt.Errorf("create deployment %s: %w", cd.Name, createErr)
			}
			depUpdateAfterCreate := UpdateDeploymentParams{}
			depNeedsUpdate := false
			if cd.APIKey != "" {
				enc, encErr := crypto.EncryptString(cd.APIKey, encKey, deploymentAAD(created.ID))
				if encErr != nil {
					return fmt.Errorf("encrypt api key for deployment %s: %w", cd.Name, encErr)
				}
				depUpdateAfterCreate.APIKeyEncrypted = &enc
				depNeedsUpdate = true
			}
			if cd.AWSAccessKey != "" {
				enc, encErr := crypto.EncryptString(cd.AWSAccessKey, encKey, []byte("dep-aws-access:"+created.ID))
				if encErr != nil {
					return fmt.Errorf("encrypt aws access key for deployment %s: %w", cd.Name, encErr)
				}
				depUpdateAfterCreate.AWSAccessKeyEnc = &enc
				depNeedsUpdate = true
			}
			if cd.AWSSecretKey != "" {
				enc, encErr := crypto.EncryptString(cd.AWSSecretKey, encKey, []byte("dep-aws-secret:"+created.ID))
				if encErr != nil {
					return fmt.Errorf("encrypt aws secret key for deployment %s: %w", cd.Name, encErr)
				}
				depUpdateAfterCreate.AWSSecretKeyEnc = &enc
				depNeedsUpdate = true
			}
			if cd.AWSSessionToken != "" {
				enc, encErr := crypto.EncryptString(cd.AWSSessionToken, encKey, []byte("dep-aws-session:"+created.ID))
				if encErr != nil {
					return fmt.Errorf("encrypt aws session token for deployment %s: %w", cd.Name, encErr)
				}
				depUpdateAfterCreate.AWSSessionTokenEnc = &enc
				depNeedsUpdate = true
			}
			if depNeedsUpdate {
				if _, updateErr := d.UpdateDeployment(ctx, created.ID, depUpdateAfterCreate); updateErr != nil {
					return fmt.Errorf("set secrets for deployment %s: %w", cd.Name, updateErr)
				}
			}
			continue
		}

		// Deployment exists — build an update with the current YAML values.
		provider := cd.Provider
		baseURL := cd.BaseURL
		azureDeploy := cd.AzureDeployment
		azureVersion := cd.AzureAPIVersion
		gcpProject := cd.GCPProject
		gcpLocation := cd.GCPLocation
		awsRegion := cd.AWSRegion
		weight := cd.Weight
		if weight < 1 {
			weight = 1
		}
		priority := cd.Priority

		updateParams := UpdateDeploymentParams{
			Provider:        &provider,
			BaseURL:         &baseURL,
			AzureDeployment: &azureDeploy,
			AzureAPIVersion: &azureVersion,
			GCPProject:      &gcpProject,
			GCPLocation:     &gcpLocation,
			AWSRegion:       &awsRegion,
			Weight:          &weight,
			Priority:        &priority,
		}

		if cd.APIKey != "" {
			enc, encErr := crypto.EncryptString(cd.APIKey, encKey, deploymentAAD(existing.ID))
			if encErr != nil {
				return fmt.Errorf("encrypt api key for deployment %s: %w", cd.Name, encErr)
			}
			updateParams.APIKeyEncrypted = &enc
		}
		if cd.AWSAccessKey != "" {
			enc, encErr := crypto.EncryptString(cd.AWSAccessKey, encKey, []byte("dep-aws-access:"+existing.ID))
			if encErr != nil {
				return fmt.Errorf("encrypt aws access key for deployment %s: %w", cd.Name, encErr)
			}
			updateParams.AWSAccessKeyEnc = &enc
		}
		if cd.AWSSecretKey != "" {
			enc, encErr := crypto.EncryptString(cd.AWSSecretKey, encKey, []byte("dep-aws-secret:"+existing.ID))
			if encErr != nil {
				return fmt.Errorf("encrypt aws secret key for deployment %s: %w", cd.Name, encErr)
			}
			updateParams.AWSSecretKeyEnc = &enc
		}
		if cd.AWSSessionToken != "" {
			enc, encErr := crypto.EncryptString(cd.AWSSessionToken, encKey, []byte("dep-aws-session:"+existing.ID))
			if encErr != nil {
				return fmt.Errorf("encrypt aws session token for deployment %s: %w", cd.Name, encErr)
			}
			updateParams.AWSSessionTokenEnc = &enc
		}

		if _, updateErr := d.UpdateDeployment(ctx, existing.ID, updateParams); updateErr != nil {
			return fmt.Errorf("update deployment %s: %w", cd.Name, updateErr)
		}
	}

	// Soft-delete DB deployments no longer present in the YAML config.
	for _, dep := range dbDeps {
		if !seen[dep.Name] {
			if deleteErr := d.DeleteDeployment(ctx, dep.ID); deleteErr != nil {
				return fmt.Errorf("delete removed deployment %s: %w", dep.Name, deleteErr)
			}
		}
	}

	return nil
}

// deploymentAAD returns the additional authenticated data used when encrypting
// or decrypting a deployment API key. The AAD binds the ciphertext to the
// specific deployment row so that a ciphertext from one row cannot be replayed
// against a different row.
func deploymentAAD(id string) []byte {
	return []byte("deployment:" + id)
}
