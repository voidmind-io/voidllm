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
	// GCPProject is the Google Cloud project ID. Required when provider is "vertex".
	GCPProject string `json:"gcp_project"`
	// GCPLocation is the Google Cloud region (e.g. "us-central1"). Required when provider is "vertex".
	GCPLocation string `json:"gcp_location"`
	// AWSRegion is the AWS region (e.g. "us-east-1"). Required when provider is "bedrock-converse".
	AWSRegion string `json:"aws_region"`
	// AWSAccessKey is the AWS IAM access key ID. Required when provider is "bedrock-converse".
	AWSAccessKey string `json:"aws_access_key"`
	// AWSSecretKey is the AWS IAM secret access key. Required when provider is "bedrock-converse".
	AWSSecretKey string `json:"aws_secret_key"`
	// AWSSessionToken is an optional AWS STS session token for temporary credentials.
	AWSSessionToken string `json:"aws_session_token"`
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
	// GCPProject, when non-nil, replaces the stored Google Cloud project ID.
	GCPProject *string `json:"gcp_project"`
	// GCPLocation, when non-nil, replaces the stored Google Cloud region.
	GCPLocation *string `json:"gcp_location"`
	// AWSRegion, when non-nil, replaces the stored AWS region.
	AWSRegion *string `json:"aws_region"`
	// AWSAccessKey, when non-nil, replaces the stored AWS IAM access key ID.
	AWSAccessKey *string `json:"aws_access_key"`
	// AWSSecretKey, when non-nil, replaces the stored AWS IAM secret access key.
	AWSSecretKey *string `json:"aws_secret_key"`
	// AWSSessionToken, when non-nil, replaces the stored AWS STS session token.
	AWSSessionToken *string `json:"aws_session_token"`
	Weight          *int    `json:"weight"`
	Priority        *int    `json:"priority"`
}

// deploymentResponse is the JSON representation of a deployment returned by the API.
// The API key and AWS credentials are write-only and are never included in responses.
type deploymentResponse struct {
	ID              string `json:"id"`
	ModelID         string `json:"model_id"`
	Name            string `json:"name"`
	Provider        string `json:"provider"`
	BaseURL         string `json:"base_url"`
	AzureDeployment string `json:"azure_deployment,omitempty"`
	AzureAPIVersion string `json:"azure_api_version,omitempty"`
	// GCPProject is the Google Cloud project ID. Non-empty only for provider "vertex".
	GCPProject string `json:"gcp_project,omitempty"`
	// GCPLocation is the Google Cloud region. Non-empty only for provider "vertex".
	GCPLocation string `json:"gcp_location,omitempty"`
	// AWSRegion is the AWS region. Non-empty only for provider "bedrock-converse".
	AWSRegion string `json:"aws_region,omitempty"`
	Weight    int    `json:"weight"`
	Priority  int    `json:"priority"`
	IsActive  bool   `json:"is_active"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
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
		GCPProject:      d.GCPProject,
		GCPLocation:     d.GCPLocation,
		AWSRegion:       d.AWSRegion,
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
	if req.Provider == "bedrock-converse" {
		if req.AWSRegion == "" {
			return apierror.BadRequest(c, "aws_region is required for bedrock-converse provider")
		}
		if req.AWSAccessKey == "" {
			return apierror.BadRequest(c, "aws_access_key is required for bedrock-converse provider")
		}
		if req.AWSSecretKey == "" {
			return apierror.BadRequest(c, "aws_secret_key is required for bedrock-converse provider")
		}
	}

	// Insert without secrets to obtain the stable deployment ID used as AAD.
	dep, err := h.DB.CreateDeployment(ctx, db.CreateDeploymentParams{
		ModelID:         modelID,
		Name:            req.Name,
		Provider:        req.Provider,
		BaseURL:         req.BaseURL,
		APIKeyEncrypted: nil,
		AzureDeployment: req.AzureDeployment,
		AzureAPIVersion: req.AzureAPIVersion,
		GCPProject:      req.GCPProject,
		GCPLocation:     req.GCPLocation,
		AWSRegion:       req.AWSRegion,
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

	// Encrypt secrets using the immutable deployment ID as AAD and persist them.
	depSecretsUpdate := db.UpdateDeploymentParams{}
	depNeedsSecretsUpdate := false
	if req.APIKey != "" {
		enc, encErr := crypto.EncryptString(req.APIKey, h.EncryptionKey, deploymentAAD(dep.ID))
		if encErr != nil {
			h.Log.ErrorContext(ctx, "create deployment: encrypt api key", slog.String("error", encErr.Error()))
			return apierror.InternalError(c, "failed to encrypt api key")
		}
		depSecretsUpdate.APIKeyEncrypted = &enc
		depNeedsSecretsUpdate = true
	}
	if req.AWSAccessKey != "" {
		enc, encErr := crypto.EncryptString(req.AWSAccessKey, h.EncryptionKey, []byte("dep-aws-access:"+dep.ID))
		if encErr != nil {
			h.Log.ErrorContext(ctx, "create deployment: encrypt aws access key", slog.String("error", encErr.Error()))
			return apierror.InternalError(c, "failed to encrypt aws access key")
		}
		depSecretsUpdate.AWSAccessKeyEnc = &enc
		depNeedsSecretsUpdate = true
	}
	if req.AWSSecretKey != "" {
		enc, encErr := crypto.EncryptString(req.AWSSecretKey, h.EncryptionKey, []byte("dep-aws-secret:"+dep.ID))
		if encErr != nil {
			h.Log.ErrorContext(ctx, "create deployment: encrypt aws secret key", slog.String("error", encErr.Error()))
			return apierror.InternalError(c, "failed to encrypt aws secret key")
		}
		depSecretsUpdate.AWSSecretKeyEnc = &enc
		depNeedsSecretsUpdate = true
	}
	if req.AWSSessionToken != "" {
		enc, encErr := crypto.EncryptString(req.AWSSessionToken, h.EncryptionKey, []byte("dep-aws-session:"+dep.ID))
		if encErr != nil {
			h.Log.ErrorContext(ctx, "create deployment: encrypt aws session token", slog.String("error", encErr.Error()))
			return apierror.InternalError(c, "failed to encrypt aws session token")
		}
		depSecretsUpdate.AWSSessionTokenEnc = &enc
		depNeedsSecretsUpdate = true
	}
	if depNeedsSecretsUpdate {
		dep, err = h.DB.UpdateDeployment(ctx, dep.ID, depSecretsUpdate)
		if err != nil {
			h.Log.ErrorContext(ctx, "create deployment: store secrets", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to store deployment secrets")
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
		GCPProject:      req.GCPProject,
		GCPLocation:     req.GCPLocation,
		AWSRegion:       req.AWSRegion,
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
	if req.AWSAccessKey != nil {
		enc, encErr := crypto.EncryptString(*req.AWSAccessKey, h.EncryptionKey, []byte("dep-aws-access:"+deploymentID))
		if encErr != nil {
			h.Log.ErrorContext(ctx, "update deployment: encrypt aws access key", slog.String("error", encErr.Error()))
			return apierror.InternalError(c, "failed to encrypt aws access key")
		}
		params.AWSAccessKeyEnc = &enc
	}
	if req.AWSSecretKey != nil {
		enc, encErr := crypto.EncryptString(*req.AWSSecretKey, h.EncryptionKey, []byte("dep-aws-secret:"+deploymentID))
		if encErr != nil {
			h.Log.ErrorContext(ctx, "update deployment: encrypt aws secret key", slog.String("error", encErr.Error()))
			return apierror.InternalError(c, "failed to encrypt aws secret key")
		}
		params.AWSSecretKeyEnc = &enc
	}
	if req.AWSSessionToken != nil {
		enc, encErr := crypto.EncryptString(*req.AWSSessionToken, h.EncryptionKey, []byte("dep-aws-session:"+deploymentID))
		if encErr != nil {
			h.Log.ErrorContext(ctx, "update deployment: encrypt aws session token", slog.String("error", encErr.Error()))
			return apierror.InternalError(c, "failed to encrypt aws session token")
		}
		params.AWSSessionTokenEnc = &enc
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
