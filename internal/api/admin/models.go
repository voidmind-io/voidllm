package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/provider"
	"github.com/voidmind-io/voidllm/internal/proxy"
	voidredis "github.com/voidmind-io/voidllm/internal/redis"
	"github.com/voidmind-io/voidllm/pkg/crypto"
)

// createModelRequest is the JSON body accepted by CreateModel.
type createModelRequest struct {
	Name             string   `json:"name"`
	Provider         string   `json:"provider"`
	BaseURL          string   `json:"base_url"`
	APIKey           string   `json:"api_key,omitempty"`
	MaxContextTokens int      `json:"max_context_tokens"`
	InputPricePer1M  float64  `json:"input_price_per_1m"`
	OutputPricePer1M float64  `json:"output_price_per_1m"`
	AzureDeployment  string   `json:"azure_deployment,omitempty"`
	AzureAPIVersion  string   `json:"azure_api_version,omitempty"`
	// Aliases are optional short names that resolve to this model in proxy requests.
	Aliases []string `json:"aliases"`
	// Timeout is the per-model upstream timeout as a Go duration string (e.g. "30s",
	// "2m"). When non-empty it overrides the global stream/response timeout for
	// this model. Omit or pass an empty string to use the global default.
	Timeout string `json:"timeout,omitempty"`
}

// updateModelRequest is the JSON body accepted by UpdateModel.
// All fields are optional; a nil pointer means the field is left unchanged.
type updateModelRequest struct {
	Name             *string   `json:"name"`
	Provider         *string   `json:"provider"`
	BaseURL          *string   `json:"base_url"`
	APIKey           *string   `json:"api_key"`
	MaxContextTokens *int      `json:"max_context_tokens"`
	InputPricePer1M  *float64  `json:"input_price_per_1m"`
	OutputPricePer1M *float64  `json:"output_price_per_1m"`
	AzureDeployment  *string   `json:"azure_deployment"`
	AzureAPIVersion  *string   `json:"azure_api_version"`
	// Aliases, when non-nil, replaces the full set of aliases for the model.
	// Pass an empty slice to remove all aliases.
	Aliases *[]string `json:"aliases"`
	// Timeout, when non-nil, replaces the per-model timeout. Pass a pointer to
	// an empty string to clear the timeout and revert to the global default.
	Timeout *string `json:"timeout"`
}

// modelResponse is the JSON representation of a model returned by the API.
// It omits the encrypted API key; the plaintext is never returned after creation.
type modelResponse struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Provider         string   `json:"provider"`
	BaseURL          string   `json:"base_url"`
	MaxContextTokens int      `json:"max_context_tokens"`
	InputPricePer1M  float64  `json:"input_price_per_1m"`
	OutputPricePer1M float64  `json:"output_price_per_1m"`
	AzureDeployment  string   `json:"azure_deployment,omitempty"`
	AzureAPIVersion  string   `json:"azure_api_version,omitempty"`
	IsActive         bool     `json:"is_active"`
	Source           string   `json:"source"`
	Aliases          []string `json:"aliases"`
	// Timeout is the per-model upstream timeout (e.g. "30s", "2m").
	// An empty string means the global default is used.
	Timeout   string `json:"timeout,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// paginatedModelsResponse wraps a page of models with pagination metadata.
type paginatedModelsResponse struct {
	Data    []modelResponse `json:"data"`
	HasMore bool            `json:"has_more"`
	Cursor  string          `json:"next_cursor,omitempty"`
}

// testClient is the shared HTTP client used by TestModelConnection.
// Redirects are disabled to prevent redirect-based SSRF bypass; the caller
// receives the first response as-is regardless of its Location header.
var testClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// modelToResponse converts a db.Model to its API wire representation.
func modelToResponse(m *db.Model) modelResponse {
	aliases := []string{}
	if m.Aliases != "" {
		aliases = strings.Split(m.Aliases, ",")
	}
	return modelResponse{
		ID:               m.ID,
		Name:             m.Name,
		Provider:         m.Provider,
		BaseURL:          m.BaseURL,
		MaxContextTokens: m.MaxContextTokens,
		InputPricePer1M:  m.InputPricePer1M,
		OutputPricePer1M: m.OutputPricePer1M,
		AzureDeployment:  m.AzureDeployment,
		AzureAPIVersion:  m.AzureAPIVersion,
		IsActive:         m.IsActive,
		Source:           m.Source,
		Aliases:          aliases,
		Timeout:          m.Timeout,
		CreatedAt:        m.CreatedAt,
		UpdatedAt:        m.UpdatedAt,
	}
}

// dbModelToProxy converts a db.Model to a proxy.Model for registry insertion.
// apiKeyPlaintext is the decrypted API key; pass an empty string when no key is set.
func dbModelToProxy(m *db.Model, apiKeyPlaintext string) proxy.Model {
	var aliases []string
	if m.Aliases != "" {
		aliases = strings.Split(m.Aliases, ",")
	}
	var timeout time.Duration
	if m.Timeout != "" {
		if d, err := time.ParseDuration(m.Timeout); err == nil {
			timeout = d
		}
	}
	return proxy.Model{
		Name:             m.Name,
		Provider:         m.Provider,
		BaseURL:          m.BaseURL,
		APIKey:           apiKeyPlaintext,
		Aliases:          aliases,
		MaxContextTokens: m.MaxContextTokens,
		Pricing: config.PricingConfig{
			InputPer1M:  m.InputPricePer1M,
			OutputPer1M: m.OutputPricePer1M,
		},
		AzureDeployment: m.AzureDeployment,
		AzureAPIVersion: m.AzureAPIVersion,
		Timeout:         timeout,
	}
}

// validateAndJoinAliases validates the provided alias slice and returns the
// comma-separated string suitable for storage. It checks for empty values,
// commas within individual aliases, intra-list duplicates, and conflicts with
// any name or alias already present in the registry (excluding excludeName, so
// that an update can keep its own existing aliases without false conflicts).
// It also queries the database to catch inactive models whose names would
// conflict when they are later reactivated.
// On the first violation it returns an empty string and a non-empty error
// message suitable for a 400 response. On success it returns the joined string
// and an empty message; an empty slice yields an empty string.
func (h *Handler) validateAndJoinAliases(ctx context.Context, aliases []string, excludeName string) (string, string) {
	if len(aliases) == 0 {
		return "", ""
	}
	seen := make(map[string]struct{}, len(aliases))
	cleaned := make([]string, 0, len(aliases))
	for _, a := range aliases {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if strings.Contains(a, ",") {
			return "", "alias must not contain a comma: " + a
		}
		if _, dup := seen[a]; dup {
			return "", "duplicate alias: " + a
		}
		seen[a] = struct{}{}
		// Allow the alias if it resolves only to the model being updated.
		if resolved, err := h.Registry.Resolve(a); err == nil && resolved.Name != excludeName {
			return "", "alias conflicts with existing model or alias: " + a
		}
		// Also check the DB for inactive models with this name so that
		// reactivating them later does not produce a conflict.
		if dbModel, err := h.DB.GetModelByName(ctx, a); err == nil && dbModel.Name != excludeName {
			return "", "alias conflicts with existing model name: " + a
		}
		cleaned = append(cleaned, a)
	}
	return strings.Join(cleaned, ","), ""
}

// modelAAD returns the additional authenticated data used when encrypting or
// decrypting a model's upstream API key. The model ID is used as AAD because
// it is immutable — renames do not require re-encryption of the stored key.
func modelAAD(id string) []byte {
	return []byte("model:" + id)
}

// decryptModelAPIKey decrypts the stored encrypted API key for m.
// It returns an empty string when the model has no API key set.
func (h *Handler) decryptModelAPIKey(m *db.Model) (string, error) {
	if m.APIKeyEncrypted == nil {
		return "", nil
	}
	plaintext, err := crypto.DecryptString(*m.APIKeyEncrypted, h.EncryptionKey, modelAAD(m.ID))
	if err != nil {
		return "", err
	}
	return plaintext, nil
}

// CreateModel handles POST /api/v1/models.
// It persists a new model to the database and, when the model is active,
// adds it to the in-memory registry so proxy requests can immediately use it.
//
// @Summary      Create a model
// @Description  Persists a new model and adds it to the live registry. The API key is encrypted at rest. Requires system admin.
// @Tags         models
// @Accept       json
// @Produce      json
// @Param        body  body      createModelRequest  true  "Model parameters"
// @Success      201   {object}  modelResponse
// @Failure      400   {object}  swaggerErrorResponse
// @Failure      401   {object}  swaggerErrorResponse
// @Failure      403   {object}  swaggerErrorResponse
// @Failure      409   {object}  swaggerErrorResponse
// @Failure      500   {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /models [post]
//
// When an API key is provided the handler inserts the model without the key
// first to obtain the stable model ID, then encrypts the key using that ID as
// AES-GCM additional authenticated data (AAD), and finally writes the
// encrypted value via UpdateModel. This two-step approach ensures the AAD is
// bound to the immutable ID rather than the mutable name.
func (h *Handler) CreateModel(c fiber.Ctx) error {
	ctx := c.Context()

	var req createModelRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	if req.Name == "" {
		return apierror.BadRequest(c, "name is required")
	}
	if req.Provider == "" {
		return apierror.BadRequest(c, "provider is required")
	}
	if !provider.ValidProviders[req.Provider] {
		return apierror.BadRequest(c, "provider must be one of: "+strings.Join(provider.Names(), ", "))
	}
	if req.BaseURL == "" {
		return apierror.BadRequest(c, "base_url is required")
	}
	if req.Timeout != "" {
		if _, err := time.ParseDuration(req.Timeout); err != nil {
			return apierror.BadRequest(c, "timeout must be a valid Go duration string (e.g. \"30s\", \"2m\")")
		}
	}

	aliasStr, aliasMsg := h.validateAndJoinAliases(ctx, req.Aliases, "")
	if aliasMsg != "" {
		return apierror.BadRequest(c, aliasMsg)
	}

	keyInfo := auth.KeyInfoFromCtx(c)
	var createdBy *string
	if keyInfo != nil && keyInfo.UserID != "" {
		createdBy = &keyInfo.UserID
	}

	// Insert without the API key so we have the model ID available as AAD.
	m, err := h.DB.CreateModel(ctx, db.CreateModelParams{
		Name:             req.Name,
		Provider:         req.Provider,
		BaseURL:          req.BaseURL,
		APIKeyEncrypted:  nil,
		MaxContextTokens: req.MaxContextTokens,
		InputPricePer1M:  req.InputPricePer1M,
		OutputPricePer1M: req.OutputPricePer1M,
		AzureDeployment:  req.AzureDeployment,
		AzureAPIVersion:  req.AzureAPIVersion,
		Source:           "api",
		CreatedBy:        createdBy,
		Aliases:          aliasStr,
		Timeout:          req.Timeout,
	})
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "a model with this name already exists")
		}
		h.Log.ErrorContext(ctx, "create model: db insert", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to create model")
	}

	// Encrypt the API key using the immutable model ID as AAD and persist it.
	if req.APIKey != "" {
		enc, encErr := crypto.EncryptString(req.APIKey, h.EncryptionKey, modelAAD(m.ID))
		if encErr != nil {
			h.Log.ErrorContext(ctx, "create model: encrypt api key", slog.String("error", encErr.Error()))
			return apierror.InternalError(c, "failed to encrypt api key")
		}
		m, err = h.DB.UpdateModel(ctx, m.ID, db.UpdateModelParams{APIKeyEncrypted: &enc})
		if err != nil {
			h.Log.ErrorContext(ctx, "create model: store encrypted key", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to store api key")
		}
	}

	if m.IsActive {
		h.Registry.AddModel(dbModelToProxy(m, req.APIKey))
	}

	if h.Redis != nil {
		if pubErr := h.Redis.PublishInvalidation(ctx, voidredis.ChannelModels, "reload"); pubErr != nil {
			h.Log.ErrorContext(ctx, "create model: publish invalidation", slog.String("error", pubErr.Error()))
		}
	}

	return c.Status(fiber.StatusCreated).JSON(modelToResponse(m))
}

// ListModels handles GET /api/v1/models.
// Supports cursor-based pagination and an include_inactive query parameter.
//
// @Summary      List models
// @Description  Returns a cursor-paginated list of models. Requires system admin.
// @Tags         models
// @Produce      json
// @Param        limit             query     int     false  "Page size (default 20, max 100)"
// @Param        cursor            query     string  false  "Pagination cursor (UUIDv7 of the last seen model)"
// @Param        include_inactive  query     bool    false  "Include inactive models"
// @Success      200               {object}  paginatedModelsResponse
// @Failure      400               {object}  swaggerErrorResponse
// @Failure      401               {object}  swaggerErrorResponse
// @Failure      403               {object}  swaggerErrorResponse
// @Failure      500               {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /models [get]
func (h *Handler) ListModels(c fiber.Ctx) error {
	p, err := parsePagination(c)
	if err != nil {
		return apierror.BadRequest(c, err.Error())
	}

	includeInactive := c.Query("include_inactive") == "true"

	models, err := h.DB.ListModels(c.Context(), p.Cursor, p.Limit+1, includeInactive)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "list models", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to list models")
	}

	hasMore := len(models) > p.Limit
	if hasMore {
		models = models[:p.Limit]
	}

	resp := paginatedModelsResponse{
		Data:    make([]modelResponse, len(models)),
		HasMore: hasMore,
	}
	for i := range models {
		resp.Data[i] = modelToResponse(&models[i])
	}
	if hasMore && len(models) > 0 {
		resp.Cursor = models[len(models)-1].ID
	}

	return c.JSON(resp)
}

// GetModel handles GET /api/v1/models/:model_id.
//
// @Summary      Get a model
// @Description  Returns a single model by ID. Requires system admin.
// @Tags         models
// @Produce      json
// @Param        model_id  path      string  true  "Model ID"
// @Success      200       {object}  modelResponse
// @Failure      401       {object}  swaggerErrorResponse
// @Failure      403       {object}  swaggerErrorResponse
// @Failure      404       {object}  swaggerErrorResponse
// @Failure      500       {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /models/{model_id} [get]
func (h *Handler) GetModel(c fiber.Ctx) error {
	modelID := c.Params("model_id")

	m, err := h.DB.GetModel(c.Context(), modelID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "model not found")
		}
		h.Log.ErrorContext(c.Context(), "get model", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get model")
	}

	return c.JSON(modelToResponse(m))
}

// UpdateModel handles PATCH /api/v1/models/:model_id.
// Only non-nil fields are updated. When the API key is changed the new value
// is encrypted using the stable model ID as AAD — renames do not require
// re-encryption. The registry is refreshed after every successful update.
//
// @Summary      Update a model
// @Description  Updates model fields and refreshes the live registry. Requires system admin.
// @Tags         models
// @Accept       json
// @Produce      json
// @Param        model_id  path      string              true  "Model ID"
// @Param        body      body      updateModelRequest  true  "Fields to update"
// @Success      200       {object}  modelResponse
// @Failure      400       {object}  swaggerErrorResponse
// @Failure      401       {object}  swaggerErrorResponse
// @Failure      403       {object}  swaggerErrorResponse
// @Failure      404       {object}  swaggerErrorResponse
// @Failure      409       {object}  swaggerErrorResponse
// @Failure      500       {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /models/{model_id} [patch]
func (h *Handler) UpdateModel(c fiber.Ctx) error {
	ctx := c.Context()
	modelID := c.Params("model_id")

	existing, err := h.DB.GetModel(ctx, modelID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "model not found")
		}
		h.Log.ErrorContext(ctx, "update model: get", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get model")
	}

	var req updateModelRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	if req.Provider != nil && !provider.ValidProviders[*req.Provider] {
		return apierror.BadRequest(c, "provider must be one of: "+strings.Join(provider.Names(), ", "))
	}
	if req.Timeout != nil && *req.Timeout != "" {
		if _, err := time.ParseDuration(*req.Timeout); err != nil {
			return apierror.BadRequest(c, "timeout must be a valid Go duration string (e.g. \"30s\", \"2m\")")
		}
	}

	params := db.UpdateModelParams{
		Name:             req.Name,
		Provider:         req.Provider,
		BaseURL:          req.BaseURL,
		MaxContextTokens: req.MaxContextTokens,
		InputPricePer1M:  req.InputPricePer1M,
		OutputPricePer1M: req.OutputPricePer1M,
		AzureDeployment:  req.AzureDeployment,
		AzureAPIVersion:  req.AzureAPIVersion,
		Timeout:          req.Timeout,
	}

	if req.Aliases != nil {
		aliasStr, aliasMsg := h.validateAndJoinAliases(ctx, *req.Aliases, existing.Name)
		if aliasMsg != "" {
			return apierror.BadRequest(c, aliasMsg)
		}
		params.Aliases = &aliasStr
	}

	if req.APIKey != nil {
		// The model ID is the AAD — immutable, so no re-encryption is needed on rename.
		enc, encErr := crypto.EncryptString(*req.APIKey, h.EncryptionKey, modelAAD(modelID))
		if encErr != nil {
			h.Log.ErrorContext(ctx, "update model: encrypt api key", slog.String("error", encErr.Error()))
			return apierror.InternalError(c, "failed to encrypt api key")
		}
		params.APIKeyEncrypted = &enc
	}

	updated, err := h.DB.UpdateModel(ctx, modelID, params)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "model not found")
		}
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "a model with this name already exists")
		}
		h.Log.ErrorContext(ctx, "update model", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to update model")
	}

	if updated.IsActive {
		// When the name changed, the registry entry under the old name must be
		// removed before the updated entry is added under the new name.
		if existing.Name != updated.Name {
			h.Registry.RemoveModel(existing.Name)
		}
		plaintext, decErr := h.decryptModelAPIKey(updated)
		if decErr != nil {
			h.Log.ErrorContext(ctx, "update model: decrypt api key for registry", slog.String("error", decErr.Error()))
			// Registry is not updated but the DB write succeeded — return the
			// updated record and log the inconsistency. A process restart will
			// reconcile the registry from the database.
		} else {
			h.Registry.AddModel(dbModelToProxy(updated, plaintext))
		}
	} else {
		h.Registry.RemoveModel(existing.Name)
	}

	if h.Redis != nil {
		if pubErr := h.Redis.PublishInvalidation(ctx, voidredis.ChannelModels, "reload"); pubErr != nil {
			h.Log.ErrorContext(ctx, "update model: publish invalidation", slog.String("error", pubErr.Error()))
		}
	}

	return c.JSON(modelToResponse(updated))
}

// DeleteModel handles DELETE /api/v1/models/:model_id.
// The model is soft-deleted in the database and removed from the registry.
//
// @Summary      Delete a model
// @Description  Soft-deletes the model and removes it from the live registry. Requires system admin.
// @Tags         models
// @Produce      json
// @Param        model_id  path  string  true  "Model ID"
// @Success      204       "No Content"
// @Failure      401       {object}  swaggerErrorResponse
// @Failure      403       {object}  swaggerErrorResponse
// @Failure      404       {object}  swaggerErrorResponse
// @Failure      500       {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /models/{model_id} [delete]
func (h *Handler) DeleteModel(c fiber.Ctx) error {
	ctx := c.Context()
	modelID := c.Params("model_id")

	m, err := h.DB.GetModel(ctx, modelID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "model not found")
		}
		h.Log.ErrorContext(ctx, "delete model: get", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get model")
	}

	if err := h.DB.DeleteModel(ctx, m.ID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "model not found")
		}
		h.Log.ErrorContext(ctx, "delete model", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to delete model")
	}

	h.Registry.RemoveModel(m.Name)

	if h.Redis != nil {
		if pubErr := h.Redis.PublishInvalidation(ctx, voidredis.ChannelModels, "reload"); pubErr != nil {
			h.Log.ErrorContext(ctx, "delete model: publish invalidation", slog.String("error", pubErr.Error()))
		}
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// ActivateModel handles PATCH /api/v1/models/:model_id/activate.
// It sets is_active = true and adds the model to the in-memory registry.
//
// @Summary      Activate a model
// @Description  Marks the model as active and adds it to the live registry so proxy requests can use it immediately. Requires system admin.
// @Tags         models
// @Produce      json
// @Param        model_id  path      string  true  "Model ID"
// @Success      200       {object}  modelResponse
// @Failure      401       {object}  swaggerErrorResponse
// @Failure      403       {object}  swaggerErrorResponse
// @Failure      404       {object}  swaggerErrorResponse
// @Failure      500       {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /models/{model_id}/activate [patch]
func (h *Handler) ActivateModel(c fiber.Ctx) error {
	ctx := c.Context()
	modelID := c.Params("model_id")

	if err := h.DB.ActivateModel(ctx, modelID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "model not found")
		}
		h.Log.ErrorContext(ctx, "activate model", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to activate model")
	}

	m, err := h.DB.GetModel(ctx, modelID)
	if err != nil {
		h.Log.ErrorContext(ctx, "activate model: get after activate", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to retrieve model after activation")
	}

	plaintext, err := h.decryptModelAPIKey(m)
	if err != nil {
		h.Log.ErrorContext(ctx, "activate model: decrypt api key", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to decrypt model api key")
	}

	h.Registry.AddModel(dbModelToProxy(m, plaintext))

	if h.Redis != nil {
		if pubErr := h.Redis.PublishInvalidation(ctx, voidredis.ChannelModels, "reload"); pubErr != nil {
			h.Log.ErrorContext(ctx, "activate model: publish invalidation", slog.String("error", pubErr.Error()))
		}
	}

	return c.JSON(modelToResponse(m))
}

// DeactivateModel handles PATCH /api/v1/models/:model_id/deactivate.
// It sets is_active = false and removes the model from the in-memory registry.
//
// @Summary      Deactivate a model
// @Description  Marks the model as inactive and removes it from the live registry. In-flight requests are not affected. Requires system admin.
// @Tags         models
// @Produce      json
// @Param        model_id  path      string  true  "Model ID"
// @Success      200       {object}  modelResponse
// @Failure      401       {object}  swaggerErrorResponse
// @Failure      403       {object}  swaggerErrorResponse
// @Failure      404       {object}  swaggerErrorResponse
// @Failure      500       {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /models/{model_id}/deactivate [patch]
func (h *Handler) DeactivateModel(c fiber.Ctx) error {
	ctx := c.Context()
	modelID := c.Params("model_id")

	m, err := h.DB.GetModel(ctx, modelID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "model not found")
		}
		h.Log.ErrorContext(ctx, "deactivate model: get", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get model")
	}

	if err := h.DB.DeactivateModel(ctx, m.ID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "model not found")
		}
		h.Log.ErrorContext(ctx, "deactivate model", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to deactivate model")
	}

	h.Registry.RemoveModel(m.Name)

	if h.Redis != nil {
		if pubErr := h.Redis.PublishInvalidation(ctx, voidredis.ChannelModels, "reload"); pubErr != nil {
			h.Log.ErrorContext(ctx, "deactivate model: publish invalidation", slog.String("error", pubErr.Error()))
		}
	}

	m.IsActive = false
	return c.JSON(modelToResponse(m))
}

// GetModelHealth handles GET /api/v1/models/health.
// It returns the most recent health probe results for all registered models.
// When health monitoring is not enabled, an empty list is returned.
//
// @Summary      Get upstream model health
// @Description  Returns the latest health check results for all registered models. Requires member role or above.
// @Tags         models
// @Produce      json
// @Success      200  {object}  map[string]any
// @Failure      401  {object}  swaggerErrorResponse
// @Failure      403  {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /models/health [get]
func (h *Handler) GetModelHealth(c fiber.Ctx) error {
	if h.HealthChecker == nil {
		return c.JSON(fiber.Map{"models": []any{}})
	}
	return c.JSON(fiber.Map{"models": h.HealthChecker.GetAllHealth()})
}

// testConnectionRequest is the JSON body accepted by TestModelConnection.
type testConnectionRequest struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
}

// testConnectionResponse is the JSON response returned by TestModelConnection.
type testConnectionResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// TestModelConnection handles POST /api/v1/models/test-connection.
// It probes the upstream provider's GET /models endpoint to verify connectivity
// and authentication without persisting any data.
//
// @Summary      Test upstream provider connectivity
// @Description  Probes the provider's /models endpoint to verify URL and API key without persisting any data. Requires system admin.
// @Tags         models
// @Accept       json
// @Produce      json
// @Param        body  body      testConnectionRequest   true  "Connection parameters"
// @Success      200   {object}  testConnectionResponse
// @Failure      400   {object}  swaggerErrorResponse
// @Failure      401   {object}  swaggerErrorResponse
// @Failure      403   {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /models/test-connection [post]
//
// Security notes:
//   - Only http and https URL schemes are accepted; file://, gopher://, etc. are rejected.
//   - HTTP redirects are not followed (testClient.CheckRedirect) to prevent redirect-based SSRF.
//   - Raw error details are never returned to the caller; they are logged server-side only.
//   - Private and loopback addresses are intentionally NOT blocked because self-hosted
//     deployments (Ollama, vLLM) commonly run on localhost or RFC-1918 addresses, and
//     this endpoint is restricted to system_admin only.
func (h *Handler) TestModelConnection(c fiber.Ctx) error {
	ctx := c.Context()

	var req testConnectionRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}
	if req.BaseURL == "" {
		return apierror.BadRequest(c, "base_url is required")
	}

	// Validate scheme — only http and https are permitted.
	parsed, err := url.Parse(req.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return c.JSON(testConnectionResponse{
			Success: false,
			Message: "base_url must use http or https",
		})
	}

	testURL := strings.TrimRight(req.BaseURL, "/") + "/models"

	// Use a background context with an explicit timeout so the outbound request
	// is not cancelled if the Fiber request context is recycled.
	reqCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, testURL, nil)
	if err != nil {
		h.Log.WarnContext(ctx, "test-connection: build request failed",
			slog.String("url", req.BaseURL),
			slog.String("error", err.Error()),
		)
		return c.JSON(testConnectionResponse{
			Success: false,
			Message: "Invalid base URL format",
		})
	}

	if req.APIKey != "" {
		switch req.Provider {
		case "anthropic":
			httpReq.Header.Set("x-api-key", req.APIKey)
			httpReq.Header.Set("anthropic-version", "2023-06-01")
		default:
			httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
		}
	}

	resp, err := testClient.Do(httpReq)
	if err != nil {
		h.Log.WarnContext(ctx, "test-connection: request failed",
			slog.String("url", req.BaseURL),
			slog.String("error", err.Error()),
		)
		return c.JSON(testConnectionResponse{
			Success: false,
			Message: "Unable to reach the provided URL",
		})
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return c.JSON(testConnectionResponse{
			Success: false,
			Message: "authentication failed (HTTP " + strconv.Itoa(resp.StatusCode) + ")",
		})
	}

	if resp.StatusCode >= 400 {
		return c.JSON(testConnectionResponse{
			Success: false,
			Message: "server returned HTTP " + strconv.Itoa(resp.StatusCode),
		})
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	var modelsResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &modelsResp); err == nil && len(modelsResp.Data) > 0 {
		return c.JSON(testConnectionResponse{
			Success: true,
			Message: fmt.Sprintf("connected successfully. %d models available.", len(modelsResp.Data)),
		})
	}

	return c.JSON(testConnectionResponse{
		Success: true,
		Message: "connected successfully.",
	})
}
