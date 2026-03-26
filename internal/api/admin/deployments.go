package admin

import (
	"errors"
	"log/slog"
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/provider"
	voidredis "github.com/voidmind-io/voidllm/internal/redis"
	"github.com/voidmind-io/voidllm/pkg/crypto"
)

// createDeploymentRequest is the JSON body accepted by createDeployment.
type createDeploymentRequest struct {
	Name            string `json:"name"`
	Provider        string `json:"provider"`
	BaseURL         string `json:"base_url"`
	APIKey          string `json:"api_key"`
	AzureDeployment string `json:"azure_deployment"`
	AzureAPIVersion string `json:"azure_api_version"`
	Weight          int    `json:"weight"`
	Priority        int    `json:"priority"`
}

// updateDeploymentRequest is the JSON body accepted by updateDeployment.
// All fields are optional; a nil pointer means the field is left unchanged.
type updateDeploymentRequest struct {
	Name            *string `json:"name"`
	Provider        *string `json:"provider"`
	BaseURL         *string `json:"base_url"`
	APIKey          *string `json:"api_key"`
	AzureDeployment *string `json:"azure_deployment"`
	AzureAPIVersion *string `json:"azure_api_version"`
	Weight          *int    `json:"weight"`
	Priority        *int    `json:"priority"`
}

// deploymentResponse is the JSON representation of a deployment returned by the API.
// The API key is write-only and is never included in responses.
type deploymentResponse struct {
	ID              string `json:"id"`
	ModelID         string `json:"model_id"`
	Name            string `json:"name"`
	Provider        string `json:"provider"`
	BaseURL         string `json:"base_url"`
	AzureDeployment string `json:"azure_deployment,omitempty"`
	AzureAPIVersion string `json:"azure_api_version,omitempty"`
	Weight          int    `json:"weight"`
	Priority        int    `json:"priority"`
	IsActive        bool   `json:"is_active"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

// deploymentAAD returns the additional authenticated data used when encrypting
// or decrypting a deployment's upstream API key. The deployment ID is used as
// AAD because it is immutable — the key does not need re-encryption on rename.
func deploymentAAD(id string) []byte {
	return []byte("deployment:" + id)
}

// deploymentToResponse converts a db.Deployment to its API wire representation.
func deploymentToResponse(d *db.Deployment) deploymentResponse {
	return deploymentResponse{
		ID:              d.ID,
		ModelID:         d.ModelID,
		Name:            d.Name,
		Provider:        d.Provider,
		BaseURL:         d.BaseURL,
		AzureDeployment: d.AzureDeployment,
		AzureAPIVersion: d.AzureAPIVersion,
		Weight:          d.Weight,
		Priority:        d.Priority,
		IsActive:        d.IsActive,
		CreatedAt:       d.CreatedAt,
		UpdatedAt:       d.UpdatedAt,
	}
}

// validateDeploymentBaseURL returns a non-empty error message if baseURL is
// empty or does not start with http:// or https://.
func validateDeploymentBaseURL(baseURL string) string {
	if baseURL == "" {
		return "base_url is required"
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "base_url must begin with http:// or https://"
	}
	return ""
}

// createDeployment handles POST /api/v1/models/:model_id/deployments.
//
// @Summary      Create a deployment
// @Description  Creates a new upstream deployment for the specified model. The API key is encrypted at rest. Requires system admin.
// @Tags         deployments
// @Accept       json
// @Produce      json
// @Param        model_id  path      string                   true  "Model ID"
// @Param        body     body      createDeploymentRequest  true  "Deployment parameters"
// @Success      201      {object}  deploymentResponse
// @Failure      400      {object}  swaggerErrorResponse
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      404      {object}  swaggerErrorResponse
// @Failure      409      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /models/{model_id}/deployments [post]
//
// The API key is encrypted using the deployment ID as AES-GCM additional
// authenticated data (AAD). Because the ID is generated before encryption,
// a single insert is sufficient — unlike the two-step model key flow.
func (h *Handler) createDeployment(c fiber.Ctx) error {
	ctx := c.Context()
	modelID := c.Params("model_id")

	if _, err := h.DB.GetModel(ctx, modelID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "model not found")
		}
		h.Log.ErrorContext(ctx, "create deployment: get model", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get model")
	}

	var req createDeploymentRequest
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
	if msg := validateDeploymentBaseURL(req.BaseURL); msg != "" {
		return apierror.BadRequest(c, msg)
	}

	// Insert without API key to obtain the stable deployment ID used as AAD.
	dep, err := h.DB.CreateDeployment(ctx, db.CreateDeploymentParams{
		ModelID:         modelID,
		Name:            req.Name,
		Provider:        req.Provider,
		BaseURL:         req.BaseURL,
		APIKeyEncrypted: nil,
		AzureDeployment: req.AzureDeployment,
		AzureAPIVersion: req.AzureAPIVersion,
		Weight:          req.Weight,
		Priority:        req.Priority,
	})
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "a deployment with this name already exists for this model")
		}
		h.Log.ErrorContext(ctx, "create deployment: db insert", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to create deployment")
	}

	// Encrypt the API key using the immutable deployment ID as AAD and persist it.
	if req.APIKey != "" {
		enc, encErr := crypto.EncryptString(req.APIKey, h.EncryptionKey, deploymentAAD(dep.ID))
		if encErr != nil {
			h.Log.ErrorContext(ctx, "create deployment: encrypt api key", slog.String("error", encErr.Error()))
			return apierror.InternalError(c, "failed to encrypt api key")
		}
		dep, err = h.DB.UpdateDeployment(ctx, dep.ID, db.UpdateDeploymentParams{APIKeyEncrypted: &enc})
		if err != nil {
			h.Log.ErrorContext(ctx, "create deployment: store encrypted key", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to store api key")
		}
	}

	if h.Redis != nil {
		if pubErr := h.Redis.PublishInvalidation(ctx, voidredis.ChannelModels, "reload"); pubErr != nil {
			h.Log.ErrorContext(ctx, "create deployment: publish invalidation", slog.String("error", pubErr.Error()))
		}
	}

	return c.Status(fiber.StatusCreated).JSON(deploymentToResponse(dep))
}

// listDeployments handles GET /api/v1/models/:model_id/deployments.
//
// @Summary      List deployments
// @Description  Returns all non-deleted deployments for the specified model, ordered by priority. Requires system admin.
// @Tags         deployments
// @Produce      json
// @Param        model_id  path      string  true  "Model ID"
// @Success      200      {array}   deploymentResponse
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      404      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /models/{model_id}/deployments [get]
func (h *Handler) listDeployments(c fiber.Ctx) error {
	ctx := c.Context()
	modelID := c.Params("model_id")

	if _, err := h.DB.GetModel(ctx, modelID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "model not found")
		}
		h.Log.ErrorContext(ctx, "list deployments: get model", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get model")
	}

	deps, err := h.DB.ListDeployments(ctx, modelID)
	if err != nil {
		h.Log.ErrorContext(ctx, "list deployments", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to list deployments")
	}

	resp := make([]deploymentResponse, len(deps))
	for i := range deps {
		resp[i] = deploymentToResponse(&deps[i])
	}

	return c.JSON(resp)
}

// updateDeployment handles PATCH /api/v1/models/:model_id/deployments/:deployment_id.
// Only non-nil fields are updated. When the API key is changed the new value
// is encrypted using the stable deployment ID as AAD.
//
// @Summary      Update a deployment
// @Description  Partially updates a deployment and publishes a registry reload. Requires system admin.
// @Tags         deployments
// @Accept       json
// @Produce      json
// @Param        modelId       path      string                   true  "Model ID"
// @Param        deployment_id  path      string                   true  "Deployment ID"
// @Param        body          body      updateDeploymentRequest  true  "Fields to update"
// @Success      200           {object}  deploymentResponse
// @Failure      400           {object}  swaggerErrorResponse
// @Failure      401           {object}  swaggerErrorResponse
// @Failure      403           {object}  swaggerErrorResponse
// @Failure      404           {object}  swaggerErrorResponse
// @Failure      409           {object}  swaggerErrorResponse
// @Failure      500           {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /models/{model_id}/deployments/{deployment_id} [patch]
func (h *Handler) updateDeployment(c fiber.Ctx) error {
	ctx := c.Context()
	modelID := c.Params("model_id")
	deploymentID := c.Params("deployment_id")

	if _, err := h.DB.GetModel(ctx, modelID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "model not found")
		}
		h.Log.ErrorContext(ctx, "update deployment: get model", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get model")
	}

	var req updateDeploymentRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	if req.Provider != nil && !provider.ValidProviders[*req.Provider] {
		return apierror.BadRequest(c, "provider must be one of: "+strings.Join(provider.Names(), ", "))
	}
	if req.BaseURL != nil {
		if msg := validateDeploymentBaseURL(*req.BaseURL); msg != "" {
			return apierror.BadRequest(c, msg)
		}
	}

	dep, err := h.DB.GetDeployment(ctx, deploymentID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "deployment not found")
		}
		h.Log.ErrorContext(ctx, "update deployment: get deployment", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get deployment")
	}
	if dep.ModelID != modelID {
		return apierror.Send(c, fiber.StatusNotFound, "not_found", "deployment not found")
	}

	params := db.UpdateDeploymentParams{
		Name:            req.Name,
		Provider:        req.Provider,
		BaseURL:         req.BaseURL,
		AzureDeployment: req.AzureDeployment,
		AzureAPIVersion: req.AzureAPIVersion,
		Weight:          req.Weight,
		Priority:        req.Priority,
	}

	if req.APIKey != nil {
		enc, encErr := crypto.EncryptString(*req.APIKey, h.EncryptionKey, deploymentAAD(deploymentID))
		if encErr != nil {
			h.Log.ErrorContext(ctx, "update deployment: encrypt api key", slog.String("error", encErr.Error()))
			return apierror.InternalError(c, "failed to encrypt api key")
		}
		params.APIKeyEncrypted = &enc
	}

	dep, err = h.DB.UpdateDeployment(ctx, deploymentID, params)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "deployment not found")
		}
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "a deployment with this name already exists for this model")
		}
		h.Log.ErrorContext(ctx, "update deployment", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to update deployment")
	}

	if h.Redis != nil {
		if pubErr := h.Redis.PublishInvalidation(ctx, voidredis.ChannelModels, "reload"); pubErr != nil {
			h.Log.ErrorContext(ctx, "update deployment: publish invalidation", slog.String("error", pubErr.Error()))
		}
	}

	return c.JSON(deploymentToResponse(dep))
}

// deleteDeployment handles DELETE /api/v1/models/:model_id/deployments/:deployment_id.
// The deployment is soft-deleted; it is no longer returned by list or proxy calls.
//
// @Summary      Delete a deployment
// @Description  Soft-deletes a deployment and publishes a registry reload. Requires system admin.
// @Tags         deployments
// @Param        modelId       path  string  true  "Model ID"
// @Param        deployment_id  path  string  true  "Deployment ID"
// @Success      204           "No Content"
// @Failure      401           {object}  swaggerErrorResponse
// @Failure      403           {object}  swaggerErrorResponse
// @Failure      404           {object}  swaggerErrorResponse
// @Failure      500           {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /models/{model_id}/deployments/{deployment_id} [delete]
func (h *Handler) deleteDeployment(c fiber.Ctx) error {
	ctx := c.Context()
	modelID := c.Params("model_id")
	deploymentID := c.Params("deployment_id")

	if _, err := h.DB.GetModel(ctx, modelID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "model not found")
		}
		h.Log.ErrorContext(ctx, "delete deployment: get model", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get model")
	}

	dep, err := h.DB.GetDeployment(ctx, deploymentID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "deployment not found")
		}
		h.Log.ErrorContext(ctx, "delete deployment: get deployment", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get deployment")
	}
	if dep.ModelID != modelID {
		return apierror.Send(c, fiber.StatusNotFound, "not_found", "deployment not found")
	}

	if err := h.DB.DeleteDeployment(ctx, deploymentID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "deployment not found")
		}
		h.Log.ErrorContext(ctx, "delete deployment", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to delete deployment")
	}

	if h.Redis != nil {
		if pubErr := h.Redis.PublishInvalidation(ctx, voidredis.ChannelModels, "reload"); pubErr != nil {
			h.Log.ErrorContext(ctx, "delete deployment: publish invalidation", slog.String("error", pubErr.Error()))
		}
	}

	return c.SendStatus(fiber.StatusNoContent)
}
