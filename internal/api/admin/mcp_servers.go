package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/mcp"
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
	Name            *string `json:"name"`
	Alias           *string `json:"alias"`
	URL             *string `json:"url"`
	AuthType        *string `json:"auth_type"`
	AuthHeader      *string `json:"auth_header"`
	AuthToken       *string `json:"auth_token"` // plaintext; encrypted before storage, never returned
	CodeModeEnabled *bool   `json:"code_mode_enabled"`
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
	// Source indicates how this server was registered: "api" for Admin
	// API-created servers, "yaml" for config-file-sourced servers.
	Source          string  `json:"source"`
	Scope           string  `json:"scope"` // "global", "org", or "team"
	OrgID           *string `json:"org_id,omitempty"`
	TeamID          *string `json:"team_id,omitempty"`
	CodeModeEnabled bool    `json:"code_mode_enabled"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
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

// mcpTestTimeout is the per-request deadline used by TestMCPServerConnection.
const mcpTestTimeout = 30 * time.Second

// mcpServerToResponse converts a db.MCPServer to its API wire representation.
// The scope field is derived from the OrgID and TeamID fields: a server with
// a non-nil TeamID has scope "team", non-nil OrgID has scope "org", and
// both nil yields scope "global".
func mcpServerToResponse(s *db.MCPServer) mcpServerResponse {
	scope := "global"
	if s.TeamID != nil {
		scope = "team"
	} else if s.OrgID != nil {
		scope = "org"
	}
	return mcpServerResponse{
		ID:              s.ID,
		Name:            s.Name,
		Alias:           s.Alias,
		URL:             s.URL,
		AuthType:        s.AuthType,
		AuthHeader:      s.AuthHeader,
		IsActive:        s.IsActive,
		Source:          s.Source,
		Scope:           scope,
		OrgID:           s.OrgID,
		TeamID:          s.TeamID,
		CodeModeEnabled: s.CodeModeEnabled,
		CreatedAt:       s.CreatedAt,
		UpdatedAt:       s.UpdatedAt,
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

// validateAndNormalizeMCPServerRequest validates the common fields of a create
// request and returns the normalized auth type plus whether validation passed.
// When ok is false the HTTP 400 response has already been written; the caller
// must return nil to Fiber without further processing.
func validateAndNormalizeMCPServerRequest(c fiber.Ctx, req *createMCPServerRequest, allowPrivate bool) (authType string, ok bool) {
	fail := func(msg string) (string, bool) {
		_ = apierror.BadRequest(c, msg)
		return "", false
	}

	if req.Name == "" {
		return fail("name is required")
	}
	if req.Alias == "" {
		return fail("alias is required")
	}
	if req.URL == "" {
		return fail("url is required")
	}
	if err := validateMCPServerURL(req.URL, allowPrivate); err != nil {
		return fail(err.Error())
	}
	if msg := validateMCPAlias(req.Alias); msg != "" {
		return fail(msg)
	}

	at := req.AuthType
	if at == "" {
		at = "none"
	}
	if !validMCPAuthTypes[at] {
		return fail("auth_type must be one of: none, bearer, header")
	}
	if at == "header" && req.AuthHeader == "" {
		return fail(`auth_header is required when auth_type is "header"`)
	}
	if at == "header" && blockedHeaders[strings.ToLower(req.AuthHeader)] {
		_ = apierror.Send(c, fiber.StatusBadRequest, "invalid_auth_header",
			"auth_header cannot override structural HTTP headers")
		return "", false
	}
	return at, true
}

// createMCPServerWithToken inserts the server record (without auth token) and
// then, if a plaintext auth token was provided, encrypts it using the server ID
// as AAD and writes it back. Returns the final persisted server.
// On conflict it returns a wrapped db.ErrConflict; on other DB errors it logs
// and returns the error unwrapped. Callers are responsible for translating
// errors to HTTP responses.
func (h *Handler) createMCPServerWithToken(c fiber.Ctx, params db.CreateMCPServerParams, authToken string) (*db.MCPServer, error) {
	ctx := c.Context()

	s, err := h.DB.CreateMCPServer(ctx, params)
	if err != nil {
		if !errors.Is(err, db.ErrConflict) {
			h.Log.ErrorContext(ctx, "create mcp server: db insert", slog.String("error", err.Error()))
		}
		return nil, err
	}

	if authToken != "" {
		enc, encErr := crypto.EncryptString(authToken, h.EncryptionKey, mcpServerAAD(s.ID))
		if encErr != nil {
			h.Log.ErrorContext(ctx, "create mcp server: encrypt auth token", slog.String("error", encErr.Error()))
			return nil, encErr
		}
		s, err = h.DB.UpdateMCPServer(ctx, s.ID, db.UpdateMCPServerParams{AuthTokenEnc: &enc})
		if err != nil {
			h.Log.ErrorContext(ctx, "create mcp server: store encrypted token", slog.String("error", err.Error()))
			return nil, err
		}
	}

	return s, nil
}

// checkMCPServerReadPermission verifies that the caller may read a server.
// The rules are intentionally less restrictive than the write permission check:
//   - system_admin: always allowed.
//   - org_admin / team_admin / member: allowed for org- or team-scoped servers
//     where the caller's org matches the server's org.
//   - Global servers (org_id IS NULL) are restricted to system_admin; direct ID
//     enumeration of global servers by lower-privileged callers is not permitted.
//
// It writes the appropriate 401/403 response and returns an error when access
// is denied.
func checkMCPServerReadPermission(c fiber.Ctx, server *db.MCPServer, ki *auth.KeyInfo) error {
	if ki == nil {
		return apierror.Unauthorized(c, "authentication required")
	}
	if ki.Role == auth.RoleSystemAdmin {
		return nil
	}
	// Global servers are readable by system_admin only.
	if server.OrgID == nil {
		return apierror.Forbidden(c, "system_admin role required to access global servers")
	}
	// Org- or team-scoped: caller must belong to the same org.
	if *server.OrgID != ki.OrgID {
		return apierror.Forbidden(c, "access denied")
	}
	return nil
}

// checkMCPServerScopePermission verifies that the caller has sufficient RBAC
// privilege to mutate a server based on its scope. It writes the appropriate
// 403 response and returns an error when access is denied.
func checkMCPServerScopePermission(c fiber.Ctx, server *db.MCPServer, ki *auth.KeyInfo) error {
	if ki == nil {
		return apierror.Forbidden(c, "authentication required")
	}

	if server.TeamID != nil {
		// Team-scoped: team_admin or higher, and the caller must belong to the same org.
		if ki.Role != auth.RoleTeamAdmin && ki.Role != auth.RoleOrgAdmin && ki.Role != auth.RoleSystemAdmin {
			return apierror.Forbidden(c, "team_admin role required to modify this server")
		}
		if ki.Role != auth.RoleSystemAdmin && *server.OrgID != ki.OrgID {
			return apierror.Forbidden(c, "access denied: server belongs to a different organization")
		}
		// Team admins are scoped to their own team — they must not mutate servers
		// belonging to sibling teams within the same org.
		if ki.Role == auth.RoleTeamAdmin && ki.TeamID != *server.TeamID {
			return apierror.Forbidden(c, "access denied: server belongs to a different team")
		}
		return nil
	}

	if server.OrgID != nil {
		// Org-scoped: org_admin or higher, and the caller must belong to the same org.
		if ki.Role != auth.RoleOrgAdmin && ki.Role != auth.RoleSystemAdmin {
			return apierror.Forbidden(c, "org_admin role required to modify this server")
		}
		if ki.Role != auth.RoleSystemAdmin && *server.OrgID != ki.OrgID {
			return apierror.Forbidden(c, "access denied: server belongs to a different organization")
		}
		return nil
	}

	// Global: system_admin only.
	if ki.Role != auth.RoleSystemAdmin {
		return apierror.Forbidden(c, "system_admin role required to modify global servers")
	}
	return nil
}

// CreateMCPServer handles POST /api/v1/mcp-servers.
// It persists a new global MCP server (org_id = NULL, team_id = NULL).
//
// @Summary      Create a global MCP server
// @Description  Persists a new global MCP server. The auth token is encrypted at rest and never returned. Requires system admin.
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
	var req createMCPServerRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	authType, ok := validateAndNormalizeMCPServerRequest(c, &req, h.MCPAllowPrivateURLs)
	if !ok {
		return nil
	}

	keyInfo := auth.KeyInfoFromCtx(c)
	createdBy := ""
	if keyInfo != nil {
		createdBy = keyInfo.UserID
	}

	s, err := h.createMCPServerWithToken(c, db.CreateMCPServerParams{
		Name:         req.Name,
		Alias:        req.Alias,
		URL:          req.URL,
		AuthType:     authType,
		AuthHeader:   req.AuthHeader,
		AuthTokenEnc: nil,
		OrgID:        nil,
		TeamID:       nil,
		CreatedBy:    createdBy,
		Source:       "api",
	}, req.AuthToken)
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "an MCP server with this alias already exists")
		}
		return apierror.InternalError(c, "failed to create MCP server")
	}

	h.refreshMCPServerCache(c.Context())

	if h.ToolCache != nil {
		alias := s.Alias
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			h.ToolCache.RefreshServer(ctx, alias) //nolint:errcheck
		}()
	}

	return c.Status(fiber.StatusCreated).JSON(mcpServerToResponse(s))
}

// CreateOrgMCPServer handles POST /api/v1/orgs/:org_id/mcp-servers.
// It persists a new org-scoped MCP server (org_id = URL param, team_id = NULL).
//
// @Summary      Create an org-scoped MCP server
// @Description  Persists a new MCP server scoped to the given organization. Requires org admin.
// @Tags         mcp-servers
// @Accept       json
// @Produce      json
// @Param        org_id  path      string                  true  "Organization ID"
// @Param        body    body      createMCPServerRequest  true  "MCP server parameters"
// @Success      201     {object}  mcpServerResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      409     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/mcp-servers [post]
func (h *Handler) CreateOrgMCPServer(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	keyInfo, ok := requireOrgAdmin(c, orgID)
	if !ok {
		return nil
	}

	var req createMCPServerRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	authType, authOK := validateAndNormalizeMCPServerRequest(c, &req, h.MCPAllowPrivateURLs)
	if !authOK {
		return nil
	}

	createdBy := keyInfo.UserID

	s, err := h.createMCPServerWithToken(c, db.CreateMCPServerParams{
		Name:         req.Name,
		Alias:        req.Alias,
		URL:          req.URL,
		AuthType:     authType,
		AuthHeader:   req.AuthHeader,
		AuthTokenEnc: nil,
		OrgID:        &orgID,
		TeamID:       nil,
		CreatedBy:    createdBy,
		Source:       "api",
	}, req.AuthToken)
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "an MCP server with this alias already exists")
		}
		return apierror.InternalError(c, "failed to create MCP server")
	}

	h.refreshMCPServerCache(c.Context())

	if h.ToolCache != nil {
		alias := s.Alias
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			h.ToolCache.RefreshServer(ctx, alias) //nolint:errcheck
		}()
	}

	return c.Status(fiber.StatusCreated).JSON(mcpServerToResponse(s))
}

// CreateTeamMCPServer handles POST /api/v1/orgs/:org_id/teams/:team_id/mcp-servers.
// It persists a new team-scoped MCP server (org_id and team_id from URL params).
// The team is validated to belong to the given organization.
//
// @Summary      Create a team-scoped MCP server
// @Description  Persists a new MCP server scoped to the given team within an organization. Requires team admin.
// @Tags         mcp-servers
// @Accept       json
// @Produce      json
// @Param        org_id   path      string                  true  "Organization ID"
// @Param        team_id  path      string                  true  "Team ID"
// @Param        body     body      createMCPServerRequest  true  "MCP server parameters"
// @Success      201      {object}  mcpServerResponse
// @Failure      400      {object}  swaggerErrorResponse
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      404      {object}  swaggerErrorResponse
// @Failure      409      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams/{team_id}/mcp-servers [post]
func (h *Handler) CreateTeamMCPServer(c fiber.Ctx) error {
	// requireTeamAccess validates org membership, fetches and verifies the team
	// belongs to the org, and enforces team membership for non-org-admins.
	keyInfo, team, ok := h.requireTeamAccess(c)
	if !ok {
		return nil
	}
	orgID := team.OrgID
	teamID := team.ID

	var req createMCPServerRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	authType, authOK := validateAndNormalizeMCPServerRequest(c, &req, h.MCPAllowPrivateURLs)
	if !authOK {
		return nil
	}

	s, createErr := h.createMCPServerWithToken(c, db.CreateMCPServerParams{
		Name:         req.Name,
		Alias:        req.Alias,
		URL:          req.URL,
		AuthType:     authType,
		AuthHeader:   req.AuthHeader,
		AuthTokenEnc: nil,
		OrgID:        &orgID,
		TeamID:       &teamID,
		CreatedBy:    keyInfo.UserID,
		Source:       "api",
	}, req.AuthToken)
	if createErr != nil {
		if errors.Is(createErr, db.ErrConflict) {
			return apierror.Conflict(c, "an MCP server with this alias already exists")
		}
		return apierror.InternalError(c, "failed to create MCP server")
	}

	h.refreshMCPServerCache(c.Context())

	if h.ToolCache != nil {
		alias := s.Alias
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			h.ToolCache.RefreshServer(ctx, alias) //nolint:errcheck
		}()
	}

	return c.Status(fiber.StatusCreated).JSON(mcpServerToResponse(s))
}

// ListMCPServers handles GET /api/v1/mcp-servers.
// It returns all active, non-deleted global MCP servers ordered by alias ascending.
// Intended for system_admin use only.
//
// @Summary      List global MCP servers
// @Description  Returns all active global MCP servers ordered by alias. Requires system admin.
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

// ListOrgMCPServers handles GET /api/v1/orgs/:org_id/mcp-servers.
// It returns org-scoped and global MCP servers visible to the given organization.
//
// @Summary      List org-scoped MCP servers
// @Description  Returns org-scoped and global MCP servers visible to the given organization, ordered by alias.
// @Tags         mcp-servers
// @Produce      json
// @Param        org_id  path      string  true  "Organization ID"
// @Success      200     {array}   mcpServerResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/mcp-servers [get]
func (h *Handler) ListOrgMCPServers(c fiber.Ctx) error {
	ctx := c.Context()
	orgID := c.Params("org_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	servers, err := h.DB.ListMCPServersByOrg(ctx, orgID)
	if err != nil {
		h.Log.ErrorContext(ctx, "list org mcp servers", slog.String("org_id", orgID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to list MCP servers")
	}

	resp := make([]mcpServerResponse, len(servers))
	for i := range servers {
		resp[i] = mcpServerToResponse(&servers[i])
	}
	return c.JSON(resp)
}

// ListTeamMCPServers handles GET /api/v1/orgs/:org_id/teams/:team_id/mcp-servers.
// It returns team-scoped, org-scoped, and global MCP servers visible to the given team.
//
// @Summary      List team-scoped MCP servers
// @Description  Returns team-scoped, org-scoped, and global MCP servers visible to the given team, ordered by alias.
// @Tags         mcp-servers
// @Produce      json
// @Param        org_id   path      string  true  "Organization ID"
// @Param        team_id  path      string  true  "Team ID"
// @Success      200      {array}   mcpServerResponse
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams/{team_id}/mcp-servers [get]
func (h *Handler) ListTeamMCPServers(c fiber.Ctx) error {
	// requireTeamAccess validates org membership, fetches and verifies the team
	// belongs to the org, and enforces team membership for non-org-admins.
	_, team, ok := h.requireTeamAccess(c)
	if !ok {
		return nil
	}
	orgID := team.OrgID
	teamID := team.ID

	ctx := c.Context()
	servers, err := h.DB.ListMCPServersByTeam(ctx, teamID, orgID)
	if err != nil {
		h.Log.ErrorContext(ctx, "list team mcp servers",
			slog.String("org_id", orgID),
			slog.String("team_id", teamID),
			slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to list MCP servers")
	}

	resp := make([]mcpServerResponse, len(servers))
	for i := range servers {
		resp[i] = mcpServerToResponse(&servers[i])
	}
	return c.JSON(resp)
}

// GetMCPServer handles GET /api/v1/mcp-servers/:server_id.
// Access is granted if the caller has sufficient role for the server's scope.
//
// @Summary      Get an MCP server
// @Description  Returns a single MCP server by ID. Access is scope-checked against the caller's role.
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

	ki := auth.KeyInfoFromCtx(c)
	if permErr := checkMCPServerReadPermission(c, s, ki); permErr != nil {
		return permErr
	}

	return c.JSON(mcpServerToResponse(s))
}

// UpdateMCPServer handles PATCH /api/v1/mcp-servers/:server_id.
// Only non-nil fields in the request body are applied; omitted fields are unchanged.
// The scope permission is checked after fetching the server.
//
// @Summary      Update an MCP server
// @Description  Partially updates an MCP server. Access is scope-checked against the caller's role.
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

	existing, err := h.DB.GetMCPServer(ctx, id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "MCP server not found")
		}
		h.Log.ErrorContext(ctx, "update mcp server: get server", slog.String("id", id), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get MCP server")
	}

	ki := auth.KeyInfoFromCtx(c)
	if permErr := checkMCPServerScopePermission(c, existing, ki); permErr != nil {
		return permErr
	}

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
		Name:            req.Name,
		Alias:           req.Alias,
		URL:             req.URL,
		AuthType:        req.AuthType,
		AuthHeader:      req.AuthHeader,
		CodeModeEnabled: req.CodeModeEnabled,
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

	h.refreshMCPServerCache(ctx)

	return c.JSON(mcpServerToResponse(s))
}

// DeleteMCPServer handles DELETE /api/v1/mcp-servers/:server_id.
// The deletion is a soft-delete; the record is retained with deleted_at set.
// The scope permission is checked after fetching the server.
//
// @Summary      Delete an MCP server
// @Description  Soft-deletes an MCP server. Access is scope-checked against the caller's role.
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

	existing, err := h.DB.GetMCPServer(ctx, id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "MCP server not found")
		}
		h.Log.ErrorContext(ctx, "delete mcp server: get server", slog.String("id", id), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get MCP server")
	}

	ki := auth.KeyInfoFromCtx(c)
	if permErr := checkMCPServerScopePermission(c, existing, ki); permErr != nil {
		return permErr
	}

	if err := h.DB.DeleteMCPServer(ctx, id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "MCP server not found")
		}
		h.Log.ErrorContext(ctx, "delete mcp server", slog.String("id", id), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to delete MCP server")
	}

	if h.ToolCache != nil {
		h.ToolCache.InvalidateWithStore(ctx, existing.Alias, existing.ID)
	}

	h.refreshMCPServerCache(ctx)

	return c.SendStatus(fiber.StatusNoContent)
}

// ActivateMCPServer handles PATCH /api/v1/mcp-servers/:server_id/activate.
// It sets is_active = true on the server. Access is scope-checked.
//
// @Summary      Activate an MCP server
// @Description  Marks the MCP server as active. Access is scope-checked against the caller's role.
// @Tags         mcp-servers
// @Produce      json
// @Param        server_id  path      string  true  "MCP server ID"
// @Success      200        {object}  mcpServerResponse
// @Failure      401        {object}  swaggerErrorResponse
// @Failure      403        {object}  swaggerErrorResponse
// @Failure      404        {object}  swaggerErrorResponse
// @Failure      500        {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /mcp-servers/{server_id}/activate [patch]
func (h *Handler) ActivateMCPServer(c fiber.Ctx) error {
	return h.setMCPServerActive(c, true)
}

// DeactivateMCPServer handles PATCH /api/v1/mcp-servers/:server_id/deactivate.
// It sets is_active = false on the server. Access is scope-checked.
//
// @Summary      Deactivate an MCP server
// @Description  Marks the MCP server as inactive. Access is scope-checked against the caller's role.
// @Tags         mcp-servers
// @Produce      json
// @Param        server_id  path      string  true  "MCP server ID"
// @Success      200        {object}  mcpServerResponse
// @Failure      401        {object}  swaggerErrorResponse
// @Failure      403        {object}  swaggerErrorResponse
// @Failure      404        {object}  swaggerErrorResponse
// @Failure      500        {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /mcp-servers/{server_id}/deactivate [patch]
func (h *Handler) DeactivateMCPServer(c fiber.Ctx) error {
	return h.setMCPServerActive(c, false)
}

// setMCPServerActive is the shared implementation for ActivateMCPServer and
// DeactivateMCPServer. It fetches the server, checks scope permissions, and
// writes the updated is_active value.
func (h *Handler) setMCPServerActive(c fiber.Ctx, active bool) error {
	ctx := c.Context()
	serverID := c.Params("server_id")

	server, err := h.DB.GetMCPServer(ctx, serverID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "MCP server not found")
		}
		h.Log.ErrorContext(ctx, "set mcp server active: get server",
			slog.String("id", serverID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get MCP server")
	}

	ki := auth.KeyInfoFromCtx(c)
	if permErr := checkMCPServerScopePermission(c, server, ki); permErr != nil {
		return permErr
	}

	updated, err := h.DB.UpdateMCPServer(ctx, serverID, db.UpdateMCPServerParams{IsActive: &active})
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "MCP server not found")
		}
		h.Log.ErrorContext(ctx, "set mcp server active: update",
			slog.String("id", serverID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to update MCP server")
	}

	if h.ToolCache != nil {
		if active {
			alias := updated.Alias
			go func() {
				rCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				h.ToolCache.RefreshServer(rCtx, alias) //nolint:errcheck
			}()
		} else {
			h.ToolCache.InvalidateWithStore(ctx, updated.Alias, updated.ID)
		}
	}

	h.refreshMCPServerCache(ctx)

	return c.JSON(mcpServerToResponse(updated))
}

// addMCPBlocklistRequest is the JSON body accepted by AddMCPServerBlocklist.
type addMCPBlocklistRequest struct {
	ToolName string `json:"tool_name"`
	Reason   string `json:"reason"`
}

// mcpRefreshToolsResponse is the JSON body returned by HandleRefreshMCPServerTools.
type mcpRefreshToolsResponse struct {
	ToolCount int `json:"tool_count"`
}

// ListMCPServerBlocklist handles GET /api/v1/mcp-servers/:server_id/blocklist.
// It returns all blocklist entries for the given MCP server.
// Read permission is checked against the caller's scope.
//
// @Summary      List tool blocklist for an MCP server
// @Description  Returns all tool blocklist entries for the given MCP server. Access is scope-checked against the caller's role.
// @Tags         mcp-servers
// @Produce      json
// @Param        server_id  path      string  true  "MCP server ID"
// @Success      200        {array}   db.ToolBlocklistEntry
// @Failure      401        {object}  swaggerErrorResponse
// @Failure      403        {object}  swaggerErrorResponse
// @Failure      404        {object}  swaggerErrorResponse
// @Failure      500        {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /mcp-servers/{server_id}/blocklist [get]
func (h *Handler) ListMCPServerBlocklist(c fiber.Ctx) error {
	ctx := c.Context()
	serverID := c.Params("server_id")

	server, err := h.DB.GetMCPServer(ctx, serverID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "MCP server not found")
		}
		h.Log.ErrorContext(ctx, "list mcp blocklist: get server",
			slog.String("id", serverID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get MCP server")
	}

	ki := auth.KeyInfoFromCtx(c)
	if permErr := checkMCPServerReadPermission(c, server, ki); permErr != nil {
		return permErr
	}

	entries, err := h.DB.ListToolBlocklist(ctx, serverID)
	if err != nil {
		h.Log.ErrorContext(ctx, "list mcp blocklist",
			slog.String("server_id", serverID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to list tool blocklist")
	}

	return c.JSON(entries)
}

// AddMCPServerBlocklist handles POST /api/v1/mcp-servers/:server_id/blocklist.
// It adds a tool to the blocklist for the given MCP server.
// Write permission is scope-checked.
//
// @Summary      Add a tool to the MCP server blocklist
// @Description  Adds a tool to the blocklist for the given MCP server. Access is scope-checked against the caller's role.
// @Tags         mcp-servers
// @Accept       json
// @Produce      json
// @Param        server_id  path      string                  true  "MCP server ID"
// @Param        body       body      addMCPBlocklistRequest  true  "Tool to block"
// @Success      201        {object}  db.ToolBlocklistEntry
// @Failure      400        {object}  swaggerErrorResponse
// @Failure      401        {object}  swaggerErrorResponse
// @Failure      403        {object}  swaggerErrorResponse
// @Failure      404        {object}  swaggerErrorResponse
// @Failure      409        {object}  swaggerErrorResponse
// @Failure      500        {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /mcp-servers/{server_id}/blocklist [post]
func (h *Handler) AddMCPServerBlocklist(c fiber.Ctx) error {
	ctx := c.Context()
	serverID := c.Params("server_id")

	server, err := h.DB.GetMCPServer(ctx, serverID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "MCP server not found")
		}
		h.Log.ErrorContext(ctx, "add mcp blocklist: get server",
			slog.String("id", serverID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get MCP server")
	}

	ki := auth.KeyInfoFromCtx(c)
	if permErr := checkMCPServerScopePermission(c, server, ki); permErr != nil {
		return permErr
	}

	var req addMCPBlocklistRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}
	if req.ToolName == "" {
		return apierror.BadRequest(c, "tool_name is required")
	}
	if len(req.ToolName) > 256 {
		return apierror.BadRequest(c, "tool_name must be 256 characters or fewer")
	}

	createdBy := ""
	if ki != nil {
		createdBy = ki.UserID
	}

	entry, err := h.DB.CreateToolBlocklistEntry(ctx, serverID, req.ToolName, req.Reason, createdBy)
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "tool is already on the blocklist for this server")
		}
		h.Log.ErrorContext(ctx, "add mcp blocklist",
			slog.String("server_id", serverID),
			slog.String("tool_name", req.ToolName),
			slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to add tool to blocklist")
	}

	return c.Status(fiber.StatusCreated).JSON(entry)
}

// RemoveMCPServerBlocklist handles DELETE /api/v1/mcp-servers/:server_id/blocklist.
// The tool name is passed as the ?tool_name query parameter to avoid path-routing
// issues with tool names that contain slashes or other special characters.
// Write permission is scope-checked.
//
// @Summary      Remove a tool from the MCP server blocklist
// @Description  Removes a tool from the blocklist for the given MCP server. Access is scope-checked against the caller's role.
// @Tags         mcp-servers
// @Param        server_id  path   string  true  "MCP server ID"
// @Param        tool_name  query  string  true  "Tool name to remove from blocklist"
// @Success      204
// @Failure      400  {object}  swaggerErrorResponse
// @Failure      401  {object}  swaggerErrorResponse
// @Failure      403  {object}  swaggerErrorResponse
// @Failure      404  {object}  swaggerErrorResponse
// @Failure      500  {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /mcp-servers/{server_id}/blocklist [delete]
func (h *Handler) RemoveMCPServerBlocklist(c fiber.Ctx) error {
	ctx := c.Context()
	serverID := c.Params("server_id")
	toolName := c.Query("tool_name")
	if toolName == "" {
		return apierror.BadRequest(c, "tool_name query parameter is required")
	}

	server, err := h.DB.GetMCPServer(ctx, serverID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "MCP server not found")
		}
		h.Log.ErrorContext(ctx, "remove mcp blocklist: get server",
			slog.String("id", serverID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get MCP server")
	}

	ki := auth.KeyInfoFromCtx(c)
	if permErr := checkMCPServerScopePermission(c, server, ki); permErr != nil {
		return permErr
	}

	if err := h.DB.DeleteToolBlocklistEntry(ctx, serverID, toolName); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "blocklist entry not found")
		}
		h.Log.ErrorContext(ctx, "remove mcp blocklist",
			slog.String("server_id", serverID),
			slog.String("tool_name", toolName),
			slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to remove tool from blocklist")
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// HandleRefreshMCPServerTools handles POST /api/v1/mcp-servers/:server_id/refresh-tools.
// It forces a re-fetch of the tool list for the server from the upstream MCP
// endpoint and returns the number of tools now cached. Requires Code Mode to be
// enabled (h.ToolCache non-nil). Read permission is scope-checked.
//
// @Summary      Refresh cached tools for an MCP server
// @Description  Forces a re-fetch of the tool list from the upstream MCP server and returns the updated tool count. Requires Code Mode to be enabled.
// @Tags         mcp-servers
// @Produce      json
// @Param        server_id  path      string  true  "MCP server ID"
// @Success      200        {object}  mcpRefreshToolsResponse
// @Failure      401        {object}  swaggerErrorResponse
// @Failure      403        {object}  swaggerErrorResponse
// @Failure      404        {object}  swaggerErrorResponse
// @Failure      500        {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /mcp-servers/{server_id}/refresh-tools [post]
func (h *Handler) HandleRefreshMCPServerTools(c fiber.Ctx) error {
	ctx := c.Context()
	serverID := c.Params("server_id")

	server, err := h.DB.GetMCPServer(ctx, serverID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "MCP server not found")
		}
		h.Log.ErrorContext(ctx, "refresh mcp server tools: get server",
			slog.String("id", serverID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get MCP server")
	}

	ki := auth.KeyInfoFromCtx(c)
	if permErr := checkMCPServerReadPermission(c, server, ki); permErr != nil {
		return permErr
	}

	if h.ToolCache == nil {
		return apierror.Send(c, fiber.StatusServiceUnavailable, "code_mode_disabled",
			"Code Mode is not enabled on this instance")
	}

	const refreshCooldown = 60 * time.Second
	if age := h.ToolCache.FreshFor(server.Alias); age >= 0 && age < refreshCooldown {
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
			"error":               "tool cache was refreshed recently, try again later",
			"retry_after_seconds": int((refreshCooldown - age).Seconds()),
		})
	}

	if err := h.ToolCache.RefreshServer(ctx, server.Alias); err != nil {
		h.Log.ErrorContext(ctx, "refresh mcp server tools",
			slog.String("server_id", serverID),
			slog.String("alias", server.Alias),
			slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to refresh tools from upstream MCP server")
	}

	return c.JSON(mcpRefreshToolsResponse{
		ToolCount: h.ToolCache.ToolCount(server.Alias),
	})
}

// mcpToolResponse is the JSON representation of a single cached MCP tool
// returned by HandleListMCPServerTools. Blocked indicates that the tool is
// present on the server's blocklist and will not be executed by Code Mode.
type mcpToolResponse struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Blocked     bool   `json:"blocked"`
}

// HandleListMCPServerTools handles GET /api/v1/mcp-servers/:server_id/tools.
// It returns the cached tool schemas for the server, annotated with each
// tool's blocked status. If the cache is empty for this server a fetch is
// triggered first. Requires Code Mode to be enabled (h.ToolCache non-nil).
// Read permission is scope-checked.
//
// @Summary      List cached tools for an MCP server
// @Description  Returns cached tool schemas with blocked status. Triggers an upstream fetch if the cache is empty. Access is scope-checked against the caller's role.
// @Tags         mcp-servers
// @Produce      json
// @Param        server_id  path      string  true  "MCP server ID"
// @Success      200        {array}   mcpToolResponse
// @Failure      401        {object}  swaggerErrorResponse
// @Failure      403        {object}  swaggerErrorResponse
// @Failure      404        {object}  swaggerErrorResponse
// @Failure      502        {object}  swaggerErrorResponse
// @Failure      503        {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /mcp-servers/{server_id}/tools [get]
func (h *Handler) HandleListMCPServerTools(c fiber.Ctx) error {
	ctx := c.Context()
	serverID := c.Params("server_id")

	server, err := h.DB.GetMCPServer(ctx, serverID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "MCP server not found")
		}
		h.Log.ErrorContext(ctx, "list mcp server tools: get server",
			slog.String("id", serverID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get MCP server")
	}

	ki := auth.KeyInfoFromCtx(c)
	if permErr := checkMCPServerReadPermission(c, server, ki); permErr != nil {
		return permErr
	}

	if h.ToolCache == nil {
		return apierror.Send(c, fiber.StatusServiceUnavailable, "code_mode_disabled",
			"Code Mode is not enabled on this instance")
	}

	tools, err := h.ToolCache.GetTools(ctx, server.Alias)
	if err != nil {
		h.Log.ErrorContext(ctx, "list mcp server tools: get tools",
			slog.String("server_id", serverID),
			slog.String("alias", server.Alias),
			slog.String("error", err.Error()))
		return apierror.Send(c, fiber.StatusBadGateway, "upstream_error",
			"failed to retrieve tools from upstream MCP server")
	}

	blockedNames, err := h.DB.ListBlockedToolNames(ctx, server.ID)
	if err != nil {
		h.Log.ErrorContext(ctx, "list mcp server tools: list blocklist",
			slog.String("server_id", serverID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to retrieve tool blocklist")
	}

	blockedSet := make(map[string]struct{}, len(blockedNames))
	for _, name := range blockedNames {
		blockedSet[name] = struct{}{}
	}

	resp := make([]mcpToolResponse, len(tools))
	for i, t := range tools {
		_, blocked := blockedSet[t.Name]
		resp[i] = mcpToolResponse{
			Name:        t.Name,
			Description: t.Description,
			Blocked:     blocked,
		}
	}

	return c.JSON(resp)
}

// TestMCPServerConnection handles POST /api/v1/mcp-servers/:server_id/test.
// It sends a tools/list JSON-RPC request to the MCP server and reports the
// number of available tools on success, or an error message on failure.
// The scope permission is checked after fetching the server.
//
// @Summary      Test an MCP server connection
// @Description  Sends a tools/list request to the MCP server and reports available tool count. Access is scope-checked against the caller's role.
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

	ki := auth.KeyInfoFromCtx(c)
	if permErr := checkMCPServerScopePermission(c, s, ki); permErr != nil {
		return permErr
	}

	token, err := h.decryptMCPAuthToken(s)
	if err != nil {
		h.Log.ErrorContext(ctx, "test mcp server: decrypt token", slog.String("id", id), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to decrypt auth token")
	}

	// Send a tools/list JSON-RPC 2.0 request to probe the MCP server.
	// NewHTTPTransport with h.MCPAllowPrivateURLs applies the same SSRF-safe
	// dialer used by the live proxy path, guarding against DNS rebinding even
	// when the URL appeared safe at registration time.
	transport := mcp.NewHTTPTransport(s.URL, s.AuthType, s.AuthHeader, token, mcpTestTimeout, h.MCPAllowPrivateURLs)
	defer transport.Close()

	tools, probeErr := transport.ListTools(ctx)
	if probeErr != nil {
		// If the server uses deprecated SSE transport, auto-deactivate it.
		if errors.Is(probeErr, mcp.ErrSSENotSupported) {
			isActive := false
			h.DB.UpdateMCPServer(ctx, s.ID, db.UpdateMCPServerParams{IsActive: &isActive}) //nolint:errcheck
		}
		return c.JSON(testMCPServerResponse{
			Success: false,
			Error:   probeErr.Error(),
		})
	}
	return c.JSON(testMCPServerResponse{
		Success: true,
		Tools:   len(tools),
	})
}
