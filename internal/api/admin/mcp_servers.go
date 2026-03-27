package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/crypto"
)

// createMCPServerRequest is the JSON body accepted by CreateMCPServer.
type createMCPServerRequest struct {
	Name       string `json:"name"`
	Alias      string `json:"alias"`
	URL        string `json:"url"`
	AuthType   string `json:"auth_type"`   // "none", "bearer", or "header"
	AuthHeader string `json:"auth_header"` // header name for "header" auth type
	AuthToken  string `json:"auth_token"`  // plaintext; encrypted before storage, never returned
}

// updateMCPServerRequest is the JSON body accepted by UpdateMCPServer.
// All fields are optional; a nil pointer means the field is left unchanged.
type updateMCPServerRequest struct {
	Name       *string `json:"name"`
	Alias      *string `json:"alias"`
	URL        *string `json:"url"`
	AuthType   *string `json:"auth_type"`
	AuthHeader *string `json:"auth_header"`
	AuthToken  *string `json:"auth_token"` // plaintext; encrypted before storage, never returned
}

// mcpServerResponse is the JSON representation of an MCP server returned by the API.
// The auth token is never included in the response — it is write-only.
type mcpServerResponse struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Alias      string `json:"alias"`
	URL        string `json:"url"`
	AuthType   string `json:"auth_type"`
	AuthHeader string `json:"auth_header,omitempty"`
	IsActive   bool   `json:"is_active"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// testMCPServerResponse is the JSON response from TestMCPServerConnection.
type testMCPServerResponse struct {
	Success bool   `json:"success"`
	Tools   int    `json:"tools,omitempty"`
	Error   string `json:"error,omitempty"`
}

// validMCPAuthTypes is the set of supported MCP server auth type values.
var validMCPAuthTypes = map[string]bool{
	"none":   true,
	"bearer": true,
	"header": true,
}

// mcpAliasRe matches a valid MCP server alias: lowercase alphanumeric characters
// and hyphens, starting with an alphanumeric character.
var mcpAliasRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// blockedHeaders is the set of structural HTTP header names that must not be
// overridden by the auth_header field. Comparison is done on the lowercased value.
var blockedHeaders = map[string]bool{
	"host":              true,
	"content-type":      true,
	"content-length":    true,
	"transfer-encoding": true,
	"connection":        true,
	"upgrade":           true,
	"te":                true,
	"trailer":           true,
}

// cloudMetadataIP is the well-known link-local address used by cloud provider
// instance metadata services (AWS, GCP, Azure, DigitalOcean, etc.).
var cloudMetadataIP = net.ParseIP("169.254.169.254")

// validateMCPServerURL checks that rawURL is an http/https URL that does not
// resolve to a loopback, private, or link-local network address. It is called
// on both creation and update to prevent SSRF attacks via registered MCP servers.
// DNS resolution failures are tolerated at registration time — they are checked
// again at call time by the transport layer.
func validateMCPServerURL(rawURL string, allowPrivate bool) error {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return fmt.Errorf("URL must start with http:// or https://")
	}
	if allowPrivate {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	host := u.Hostname()

	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0" {
		return fmt.Errorf("URL must not point to localhost")
	}

	ips, err := net.LookupHost(host)
	if err != nil {
		// DNS resolution failure is acceptable at registration time.
		return nil
	}
	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("URL must not point to a private or internal network address")
		}
		if ip.Equal(cloudMetadataIP) {
			return fmt.Errorf("URL must not point to cloud metadata service")
		}
	}
	return nil
}

// mcpTestClient is the shared HTTP client used by TestMCPServerConnection.
// Redirects are disabled to prevent redirect-based SSRF bypass; the caller
// receives the first response regardless of its Location header.
var mcpTestClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// mcpServerToResponse converts a db.MCPServer to its API wire representation.
func mcpServerToResponse(s *db.MCPServer) mcpServerResponse {
	return mcpServerResponse{
		ID:         s.ID,
		Name:       s.Name,
		Alias:      s.Alias,
		URL:        s.URL,
		AuthType:   s.AuthType,
		AuthHeader: s.AuthHeader,
		IsActive:   s.IsActive,
		CreatedAt:  s.CreatedAt,
		UpdatedAt:  s.UpdatedAt,
	}
}

// validateMCPAlias checks that the alias is URL-safe and not the reserved
// value "voidllm". It returns a non-empty error message when the alias is invalid.
func validateMCPAlias(alias string) string {
	if alias == "voidllm" {
		return `alias "voidllm" is reserved`
	}
	if !mcpAliasRe.MatchString(alias) {
		return "alias must contain only lowercase alphanumeric characters and hyphens, and must start with an alphanumeric character"
	}
	return ""
}

// decryptMCPAuthToken decrypts the stored encrypted auth token for server s.
// It returns an empty string when the server has no auth token set.
func (h *Handler) decryptMCPAuthToken(s *db.MCPServer) (string, error) {
	if s.AuthTokenEnc == nil {
		return "", nil
	}
	plaintext, err := crypto.DecryptString(*s.AuthTokenEnc, h.EncryptionKey, mcpServerAAD(s.ID))
	if err != nil {
		return "", fmt.Errorf("decrypt mcp auth token: %w", err)
	}
	return plaintext, nil
}

// CreateMCPServer handles POST /api/v1/mcp-servers.
// It persists a new MCP server to the database, encrypting the auth token at rest.
//
// @Summary      Create an MCP server
// @Description  Persists a new MCP server. The auth token is encrypted at rest and never returned. Requires system admin.
// @Tags         mcp-servers
// @Accept       json
// @Produce      json
// @Param        body  body      createMCPServerRequest  true  "MCP server parameters"
// @Success      201   {object}  mcpServerResponse
// @Failure      400   {object}  swaggerErrorResponse
// @Failure      401   {object}  swaggerErrorResponse
// @Failure      403   {object}  swaggerErrorResponse
// @Failure      409   {object}  swaggerErrorResponse
// @Failure      500   {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /mcp-servers [post]
//
// When an auth token is provided the handler inserts the server without the
// token first to obtain the stable server ID, then encrypts the token using
// that ID as AES-GCM additional authenticated data (AAD), and finally writes
// the encrypted value via UpdateMCPServer. This two-step approach ensures the
// AAD is bound to the immutable ID rather than any mutable field.
func (h *Handler) CreateMCPServer(c fiber.Ctx) error {
	ctx := c.Context()

	var req createMCPServerRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	if req.Name == "" {
		return apierror.BadRequest(c, "name is required")
	}
	if req.Alias == "" {
		return apierror.BadRequest(c, "alias is required")
	}
	if req.URL == "" {
		return apierror.BadRequest(c, "url is required")
	}
	if err := validateMCPServerURL(req.URL, h.MCPAllowPrivateURLs); err != nil {
		return apierror.BadRequest(c, err.Error())
	}
	if msg := validateMCPAlias(req.Alias); msg != "" {
		return apierror.BadRequest(c, msg)
	}

	authType := req.AuthType
	if authType == "" {
		authType = "none"
	}
	if !validMCPAuthTypes[authType] {
		return apierror.BadRequest(c, "auth_type must be one of: none, bearer, header")
	}
	if authType == "header" && req.AuthHeader == "" {
		return apierror.BadRequest(c, `auth_header is required when auth_type is "header"`)
	}
	if authType == "header" && blockedHeaders[strings.ToLower(req.AuthHeader)] {
		return apierror.Send(c, fiber.StatusBadRequest, "invalid_auth_header",
			"auth_header cannot override structural HTTP headers")
	}

	keyInfo := auth.KeyInfoFromCtx(c)
	createdBy := ""
	if keyInfo != nil {
		createdBy = keyInfo.UserID
	}

	// Insert without the auth token first to obtain the stable server ID for AAD.
	s, err := h.DB.CreateMCPServer(ctx, db.CreateMCPServerParams{
		Name:         req.Name,
		Alias:        req.Alias,
		URL:          req.URL,
		AuthType:     authType,
		AuthHeader:   req.AuthHeader,
		AuthTokenEnc: nil,
		CreatedBy:    createdBy,
	})
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "an MCP server with this alias already exists")
		}
		h.Log.ErrorContext(ctx, "create mcp server: db insert", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to create MCP server")
	}

	// Encrypt the auth token using the immutable server ID as AAD and persist it.
	if req.AuthToken != "" {
		enc, encErr := crypto.EncryptString(req.AuthToken, h.EncryptionKey, mcpServerAAD(s.ID))
		if encErr != nil {
			h.Log.ErrorContext(ctx, "create mcp server: encrypt auth token", slog.String("error", encErr.Error()))
			return apierror.InternalError(c, "failed to encrypt auth token")
		}
		s, err = h.DB.UpdateMCPServer(ctx, s.ID, db.UpdateMCPServerParams{AuthTokenEnc: &enc})
		if err != nil {
			h.Log.ErrorContext(ctx, "create mcp server: store encrypted token", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to store auth token")
		}
	}

	return c.Status(fiber.StatusCreated).JSON(mcpServerToResponse(s))
}

// ListMCPServers handles GET /api/v1/mcp-servers.
// It returns all active, non-deleted MCP servers ordered by alias ascending.
//
// @Summary      List MCP servers
// @Description  Returns all active MCP servers ordered by alias. Requires system admin.
// @Tags         mcp-servers
// @Produce      json
// @Success      200  {array}   mcpServerResponse
// @Failure      401  {object}  swaggerErrorResponse
// @Failure      403  {object}  swaggerErrorResponse
// @Failure      500  {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /mcp-servers [get]
func (h *Handler) ListMCPServers(c fiber.Ctx) error {
	ctx := c.Context()

	servers, err := h.DB.ListMCPServers(ctx)
	if err != nil {
		h.Log.ErrorContext(ctx, "list mcp servers", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to list MCP servers")
	}

	resp := make([]mcpServerResponse, len(servers))
	for i := range servers {
		resp[i] = mcpServerToResponse(&servers[i])
	}
	return c.JSON(resp)
}

// GetMCPServer handles GET /api/v1/mcp-servers/:server_id.
//
// @Summary      Get an MCP server
// @Description  Returns a single MCP server by ID. Requires system admin.
// @Tags         mcp-servers
// @Produce      json
// @Param        server_id  path      string  true  "MCP server ID"
// @Success      200        {object}  mcpServerResponse
// @Failure      401        {object}  swaggerErrorResponse
// @Failure      403        {object}  swaggerErrorResponse
// @Failure      404        {object}  swaggerErrorResponse
// @Failure      500        {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /mcp-servers/{server_id} [get]
func (h *Handler) GetMCPServer(c fiber.Ctx) error {
	ctx := c.Context()
	id := c.Params("server_id")

	s, err := h.DB.GetMCPServer(ctx, id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "MCP server not found")
		}
		h.Log.ErrorContext(ctx, "get mcp server", slog.String("id", id), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get MCP server")
	}

	return c.JSON(mcpServerToResponse(s))
}

// UpdateMCPServer handles PATCH /api/v1/mcp-servers/:server_id.
// Only non-nil fields in the request body are applied; omitted fields are unchanged.
//
// @Summary      Update an MCP server
// @Description  Partially updates an MCP server. Requires system admin.
// @Tags         mcp-servers
// @Accept       json
// @Produce      json
// @Param        server_id  path      string                  true  "MCP server ID"
// @Param        body       body      updateMCPServerRequest  true  "Fields to update"
// @Success      200        {object}  mcpServerResponse
// @Failure      400        {object}  swaggerErrorResponse
// @Failure      401        {object}  swaggerErrorResponse
// @Failure      403        {object}  swaggerErrorResponse
// @Failure      404        {object}  swaggerErrorResponse
// @Failure      409        {object}  swaggerErrorResponse
// @Failure      500        {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /mcp-servers/{server_id} [patch]
func (h *Handler) UpdateMCPServer(c fiber.Ctx) error {
	ctx := c.Context()
	id := c.Params("server_id")

	var req updateMCPServerRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	if req.URL != nil {
		if err := validateMCPServerURL(*req.URL, h.MCPAllowPrivateURLs); err != nil {
			return apierror.BadRequest(c, err.Error())
		}
	}
	if req.Alias != nil {
		if msg := validateMCPAlias(*req.Alias); msg != "" {
			return apierror.BadRequest(c, msg)
		}
	}
	if req.AuthType != nil && !validMCPAuthTypes[*req.AuthType] {
		return apierror.BadRequest(c, "auth_type must be one of: none, bearer, header")
	}
	if req.AuthHeader != nil && blockedHeaders[strings.ToLower(*req.AuthHeader)] {
		return apierror.Send(c, fiber.StatusBadRequest, "invalid_auth_header",
			"auth_header cannot override structural HTTP headers")
	}

	params := db.UpdateMCPServerParams{
		Name:       req.Name,
		Alias:      req.Alias,
		URL:        req.URL,
		AuthType:   req.AuthType,
		AuthHeader: req.AuthHeader,
	}

	// Encrypt the auth token using the immutable server ID as AAD.
	if req.AuthToken != nil {
		enc, encErr := crypto.EncryptString(*req.AuthToken, h.EncryptionKey, mcpServerAAD(id))
		if encErr != nil {
			h.Log.ErrorContext(ctx, "update mcp server: encrypt auth token", slog.String("id", id), slog.String("error", encErr.Error()))
			return apierror.InternalError(c, "failed to encrypt auth token")
		}
		params.AuthTokenEnc = &enc
	}

	s, err := h.DB.UpdateMCPServer(ctx, id, params)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "MCP server not found")
		}
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "an MCP server with this alias already exists")
		}
		h.Log.ErrorContext(ctx, "update mcp server", slog.String("id", id), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to update MCP server")
	}

	return c.JSON(mcpServerToResponse(s))
}

// DeleteMCPServer handles DELETE /api/v1/mcp-servers/:server_id.
// The deletion is a soft-delete; the record is retained with deleted_at set.
//
// @Summary      Delete an MCP server
// @Description  Soft-deletes an MCP server. Requires system admin.
// @Tags         mcp-servers
// @Param        server_id  path  string  true  "MCP server ID"
// @Success      204
// @Failure      401  {object}  swaggerErrorResponse
// @Failure      403  {object}  swaggerErrorResponse
// @Failure      404  {object}  swaggerErrorResponse
// @Failure      500  {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /mcp-servers/{server_id} [delete]
func (h *Handler) DeleteMCPServer(c fiber.Ctx) error {
	ctx := c.Context()
	id := c.Params("server_id")

	if err := h.DB.DeleteMCPServer(ctx, id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "MCP server not found")
		}
		h.Log.ErrorContext(ctx, "delete mcp server", slog.String("id", id), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to delete MCP server")
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// TestMCPServerConnection handles POST /api/v1/mcp-servers/:server_id/test.
// It sends a tools/list JSON-RPC request to the MCP server and reports the
// number of available tools on success, or an error message on failure.
//
// @Summary      Test an MCP server connection
// @Description  Sends a tools/list request to the MCP server and reports available tool count. Requires system admin.
// @Tags         mcp-servers
// @Produce      json
// @Param        server_id  path      string  true  "MCP server ID"
// @Success      200        {object}  testMCPServerResponse
// @Failure      401        {object}  swaggerErrorResponse
// @Failure      403        {object}  swaggerErrorResponse
// @Failure      404        {object}  swaggerErrorResponse
// @Failure      500        {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /mcp-servers/{server_id}/test [post]
func (h *Handler) TestMCPServerConnection(c fiber.Ctx) error {
	ctx := c.Context()
	id := c.Params("server_id")

	s, err := h.DB.GetMCPServer(ctx, id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "MCP server not found")
		}
		h.Log.ErrorContext(ctx, "test mcp server: get server", slog.String("id", id), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get MCP server")
	}

	token, err := h.decryptMCPAuthToken(s)
	if err != nil {
		h.Log.ErrorContext(ctx, "test mcp server: decrypt token", slog.String("id", id), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to decrypt auth token")
	}

	// Send a tools/list JSON-RPC 2.0 request to probe the MCP server.
	const probeBody = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	httpReq, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, strings.NewReader(probeBody))
	if reqErr != nil {
		return c.JSON(testMCPServerResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to build request: %s", reqErr.Error()),
		})
	}
	httpReq.Header.Set("Content-Type", "application/json")

	switch s.AuthType {
	case "bearer":
		if token != "" {
			httpReq.Header.Set("Authorization", "Bearer "+token)
		}
	case "header":
		if s.AuthHeader != "" && token != "" {
			httpReq.Header.Set(s.AuthHeader, token)
		}
	}

	httpResp, doErr := mcpTestClient.Do(httpReq)
	if doErr != nil {
		return c.JSON(testMCPServerResponse{
			Success: false,
			Error:   fmt.Sprintf("connection failed: %s", doErr.Error()),
		})
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return c.JSON(testMCPServerResponse{
			Success: false,
			Error:   fmt.Sprintf("server returned HTTP %d", httpResp.StatusCode),
		})
	}

	var rpcResp struct {
		Result *struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if decErr := json.NewDecoder(io.LimitReader(httpResp.Body, 10<<20)).Decode(&rpcResp); decErr != nil {
		return c.JSON(testMCPServerResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to decode response: %s", decErr.Error()),
		})
	}

	if rpcResp.Error != nil {
		return c.JSON(testMCPServerResponse{
			Success: false,
			Error:   rpcResp.Error.Message,
		})
	}

	toolCount := 0
	if rpcResp.Result != nil {
		toolCount = len(rpcResp.Result.Tools)
	}
	return c.JSON(testMCPServerResponse{
		Success: true,
		Tools:   toolCount,
	})
}
