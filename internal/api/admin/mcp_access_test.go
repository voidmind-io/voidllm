package admin_test

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// mustCreateGlobalMCPServer creates an active global MCP server (no OrgID, no TeamID)
// directly via the DB layer. Fails the test on error.
func mustCreateGlobalMCPServer(t *testing.T, database *db.DB, name, alias string) *db.MCPServer {
	t.Helper()
	server, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     name,
		Alias:    alias,
		URL:      "https://example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("mustCreateGlobalMCPServer(%q, %q): %v", name, alias, err)
	}
	return server
}

// mustCreateInactiveGlobalMCPServer creates a global MCP server then deactivates it
// using UpdateMCPServer. Fails the test on error.
func mustCreateInactiveGlobalMCPServer(t *testing.T, database *db.DB, name, alias string) *db.MCPServer {
	t.Helper()
	server := mustCreateGlobalMCPServer(t, database, name, alias)
	inactive := false
	updated, err := database.UpdateMCPServer(context.Background(), server.ID, db.UpdateMCPServerParams{
		IsActive: &inactive,
	})
	if err != nil {
		t.Fatalf("mustCreateInactiveGlobalMCPServer: UpdateMCPServer: %v", err)
	}
	return updated
}

// mustCreateOrgScopedMCPServer creates an MCP server scoped to the given org (non-global).
func mustCreateOrgScopedMCPServer(t *testing.T, database *db.DB, orgID, name, alias string) *db.MCPServer {
	t.Helper()
	server, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     name,
		Alias:    alias,
		URL:      "https://example.com",
		AuthType: "none",
		OrgID:    &orgID,
	})
	if err != nil {
		t.Fatalf("mustCreateOrgScopedMCPServer(%q, %q): %v", name, alias, err)
	}
	return server
}

// mustCreateAPIKeyInDB creates a user API key directly in the DB for use in key-level
// MCP access tests. It returns the *db.APIKey with the generated ID.
func mustCreateAPIKeyInDB(t *testing.T, database *db.DB, orgID, userID string) *db.APIKey {
	t.Helper()
	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("mustCreateAPIKeyInDB: generate: %v", err)
	}
	keyHash := keygen.Hash(plaintext, testHMACSecret)
	keyHint := keygen.Hint(plaintext)

	apiKey, err := database.CreateAPIKey(context.Background(), db.CreateAPIKeyParams{
		KeyHash:   keyHash,
		KeyHint:   keyHint,
		KeyType:   keygen.KeyTypeUser,
		Name:      "mcp-access-test-key",
		OrgID:     orgID,
		UserID:    &userID,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("mustCreateAPIKeyInDB: CreateAPIKey: %v", err)
	}
	return apiKey
}

// orgMCPAccessURL returns the org MCP access URL.
func orgMCPAccessURL(orgID string) string {
	return "/api/v1/orgs/" + orgID + "/mcp-access"
}

// teamMCPAccessURL returns the team MCP access URL.
func teamMCPAccessURL(orgID, teamID string) string {
	return "/api/v1/orgs/" + orgID + "/teams/" + teamID + "/mcp-access"
}

// keyMCPAccessURL returns the key MCP access URL.
func keyMCPAccessURL(orgID, keyID string) string {
	return "/api/v1/orgs/" + orgID + "/keys/" + keyID + "/mcp-access"
}

// ---- GET /api/v1/orgs/:org_id/mcp-access ----------------------------------------

func TestGetOrgMCPAccess_Empty(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupMCPServersTestApp(t, "file:TestGetOrgMCPAccess_Empty?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "MCP Access Org", "mcp-access-org-get-empty")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("GET", orgMCPAccessURL(org.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	servers, ok := got["servers"].([]any)
	if !ok {
		t.Fatalf("servers is not an array: %v", got["servers"])
	}
	if len(servers) != 0 {
		t.Errorf("len(servers) = %d, want 0 (empty allowlist)", len(servers))
	}
}

func TestGetOrgMCPAccess_RBAC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		sameOrg    bool
		wantStatus int
	}{
		{
			name:       "system_admin can get",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "org_admin of same org can get",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "member of same org returns 403",
			role:       auth.RoleMember,
			sameOrg:    true,
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "org_admin of different org returns 403",
			role:       auth.RoleOrgAdmin,
			sameOrg:    false,
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestGetOrgMCPAccess_RBAC_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupMCPServersTestApp(t, dsn)

			org := mustCreateOrg(t, database, "RBAC MCP Org", "rbac-mcp-org-"+strings.ReplaceAll(tc.name, " ", "-"))

			keyOrgID := "00000000-0000-0000-0000-000000000000"
			if tc.sameOrg {
				keyOrgID = org.ID
			}
			testKey := addTestKey(t, keyCache, tc.role, keyOrgID)

			req := httptest.NewRequest("GET", orgMCPAccessURL(org.ID), nil)
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, b)
			}
		})
	}
}

// ---- PUT /api/v1/orgs/:org_id/mcp-access ----------------------------------------

func TestSetOrgMCPAccess_Basic(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupMCPServersTestApp(t, "file:TestSetOrgMCPAccess_Basic?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Set MCP Org", "set-mcp-org-basic")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	serverA := mustCreateGlobalMCPServer(t, database, "Server A", "server-a-basic")
	serverB := mustCreateGlobalMCPServer(t, database, "Server B", "server-b-basic")

	req := httptest.NewRequest("PUT", orgMCPAccessURL(org.ID),
		bodyJSON(t, map[string]any{"servers": []string{serverA.ID, serverB.ID}}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("PUT app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	var put map[string]any
	decodeBody(t, resp.Body, &put)

	servers, ok := put["servers"].([]any)
	if !ok {
		t.Fatalf("servers is not an array: %v", put["servers"])
	}
	if len(servers) != 2 {
		t.Errorf("len(servers) = %d, want 2", len(servers))
	}

	// Verify GET reflects the same list.
	getReq := httptest.NewRequest("GET", orgMCPAccessURL(org.ID), nil)
	getReq.Header.Set("Authorization", "Bearer "+testKey)
	getResp, err := app.Test(getReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("GET app.Test: %v", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(getResp.Body)
		t.Fatalf("GET status = %d; body: %s", getResp.StatusCode, b)
	}

	var get map[string]any
	decodeBody(t, getResp.Body, &get)

	gotServers, ok := get["servers"].([]any)
	if !ok {
		t.Fatalf("servers from GET is not an array: %v", get["servers"])
	}
	if len(gotServers) != 2 {
		t.Errorf("GET len(servers) = %d, want 2", len(gotServers))
	}
}

func TestSetOrgMCPAccess_Clear(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupMCPServersTestApp(t, "file:TestSetOrgMCPAccess_Clear?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Clear MCP Org", "clear-mcp-org")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	server := mustCreateGlobalMCPServer(t, database, "Clearable Server", "clearable-server")

	// First set a server.
	setReq := httptest.NewRequest("PUT", orgMCPAccessURL(org.ID),
		bodyJSON(t, map[string]any{"servers": []string{server.ID}}))
	setReq.Header.Set("Content-Type", "application/json")
	setReq.Header.Set("Authorization", "Bearer "+testKey)
	setResp, err := app.Test(setReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("first PUT: %v", err)
	}
	setResp.Body.Close()
	if setResp.StatusCode != fiber.StatusOK {
		t.Fatalf("first PUT status = %d, want 200", setResp.StatusCode)
	}

	// Now clear with empty array.
	clearReq := httptest.NewRequest("PUT", orgMCPAccessURL(org.ID),
		bodyJSON(t, map[string]any{"servers": []string{}}))
	clearReq.Header.Set("Content-Type", "application/json")
	clearReq.Header.Set("Authorization", "Bearer "+testKey)
	clearResp, err := app.Test(clearReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("clear PUT: %v", err)
	}
	defer clearResp.Body.Close()

	if clearResp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(clearResp.Body)
		t.Fatalf("clear PUT status = %d; body: %s", clearResp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, clearResp.Body, &got)
	gotServers, _ := got["servers"].([]any)
	if len(gotServers) != 0 {
		t.Errorf("len(servers) = %d after clear, want 0", len(gotServers))
	}

	// GET also returns empty.
	getReq := httptest.NewRequest("GET", orgMCPAccessURL(org.ID), nil)
	getReq.Header.Set("Authorization", "Bearer "+testKey)
	getResp, err := app.Test(getReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer getResp.Body.Close()

	var getGot map[string]any
	decodeBody(t, getResp.Body, &getGot)
	getServers, _ := getGot["servers"].([]any)
	if len(getServers) != 0 {
		t.Errorf("GET len(servers) = %d after clear, want 0", len(getServers))
	}
}

func TestSetOrgMCPAccess_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		setupServer func(t *testing.T, database *db.DB) string // returns a server ID to use
		wantStatus  int
	}{
		{
			name: "unknown server ID returns 400",
			setupServer: func(t *testing.T, database *db.DB) string {
				t.Helper()
				return "00000000-0000-0000-0000-000000000000"
			},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "non-global org-scoped server returns 400",
			setupServer: func(t *testing.T, database *db.DB) string {
				t.Helper()
				org := mustCreateOrg(t, database, "Scoped Org Val", "scoped-org-val-srv")
				server := mustCreateOrgScopedMCPServer(t, database, org.ID, "Org Scoped", "org-scoped-val")
				return server.ID
			},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "inactive server returns 400",
			setupServer: func(t *testing.T, database *db.DB) string {
				t.Helper()
				server := mustCreateInactiveGlobalMCPServer(t, database, "Inactive Server Val", "inactive-srv-val")
				return server.ID
			},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "duplicate server ID returns 400",
			setupServer: func(t *testing.T, database *db.DB) string {
				t.Helper()
				// Return a valid server ID; the test body will duplicate it.
				server := mustCreateGlobalMCPServer(t, database, "Dup Server Val", "dup-srv-val")
				return server.ID
			},
			wantStatus: fiber.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestSetOrgMCPAccess_Validation_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupMCPServersTestApp(t, dsn)
			org := mustCreateOrg(t, database, "Validation Org", "val-org-"+strings.ReplaceAll(tc.name, " ", "-"))
			testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

			serverID := tc.setupServer(t, database)

			var serverIDs []string
			if tc.name == "duplicate server ID returns 400" {
				serverIDs = []string{serverID, serverID}
			} else {
				serverIDs = []string{serverID}
			}

			req := httptest.NewRequest("PUT", orgMCPAccessURL(org.ID),
				bodyJSON(t, map[string]any{"servers": serverIDs}))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, b)
			}
		})
	}
}

func TestSetOrgMCPAccess_RBAC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		sameOrg    bool
		wantStatus int
	}{
		{
			name:       "member returns 403",
			role:       auth.RoleMember,
			sameOrg:    true,
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "org_admin of different org returns 403",
			role:       auth.RoleOrgAdmin,
			sameOrg:    false,
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestSetOrgMCPAccess_RBAC_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupMCPServersTestApp(t, dsn)

			org := mustCreateOrg(t, database, "RBAC Set Org", "rbac-set-mcp-"+strings.ReplaceAll(tc.name, " ", "-"))
			server := mustCreateGlobalMCPServer(t, database, "RBAC Server", "rbac-srv-"+strings.ReplaceAll(tc.name, " ", "-"))

			keyOrgID := "00000000-0000-0000-0000-000000000000"
			if tc.sameOrg {
				keyOrgID = org.ID
			}
			testKey := addTestKey(t, keyCache, tc.role, keyOrgID)

			req := httptest.NewRequest("PUT", orgMCPAccessURL(org.ID),
				bodyJSON(t, map[string]any{"servers": []string{server.ID}}))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, b)
			}
		})
	}
}

// ---- GET /api/v1/orgs/:org_id/teams/:team_id/mcp-access -------------------------

func TestGetTeamMCPAccess_Empty(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupMCPServersTestApp(t, "file:TestGetTeamMCPAccess_Empty?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Team MCP Org", "team-mcp-org-get")
	team := mustCreateTeam(t, database, org.ID, "MCP Team", "mcp-team-get")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("GET", teamMCPAccessURL(org.ID, team.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	servers, ok := got["servers"].([]any)
	if !ok {
		t.Fatalf("servers is not an array: %v", got["servers"])
	}
	if len(servers) != 0 {
		t.Errorf("len(servers) = %d, want 0", len(servers))
	}
}

// ---- PUT /api/v1/orgs/:org_id/teams/:team_id/mcp-access -------------------------

func TestSetTeamMCPAccess_SubsetConstraint(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupMCPServersTestApp(t, "file:TestSetTeamMCPAccess_SubsetConstraint?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Subset Org", "subset-mcp-org")
	team := mustCreateTeam(t, database, org.ID, "Subset Team", "subset-mcp-team")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	serverA := mustCreateGlobalMCPServer(t, database, "Server A Subset", "server-a-subset")
	serverB := mustCreateGlobalMCPServer(t, database, "Server B Subset", "server-b-subset")
	serverC := mustCreateGlobalMCPServer(t, database, "Server C Subset", "server-c-subset")

	// Set org allowlist to A and B only.
	orgReq := httptest.NewRequest("PUT", orgMCPAccessURL(org.ID),
		bodyJSON(t, map[string]any{"servers": []string{serverA.ID, serverB.ID}}))
	orgReq.Header.Set("Content-Type", "application/json")
	orgReq.Header.Set("Authorization", "Bearer "+testKey)
	orgResp, err := app.Test(orgReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("set org access: %v", err)
	}
	orgResp.Body.Close()
	if orgResp.StatusCode != fiber.StatusOK {
		t.Fatalf("set org access: status = %d, want 200", orgResp.StatusCode)
	}

	// Team sets A (subset of org) — must succeed.
	teamAllowReq := httptest.NewRequest("PUT", teamMCPAccessURL(org.ID, team.ID),
		bodyJSON(t, map[string]any{"servers": []string{serverA.ID}}))
	teamAllowReq.Header.Set("Content-Type", "application/json")
	teamAllowReq.Header.Set("Authorization", "Bearer "+testKey)
	teamAllowResp, err := app.Test(teamAllowReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("set team access (allow): %v", err)
	}
	teamAllowResp.Body.Close()
	if teamAllowResp.StatusCode != fiber.StatusOK {
		t.Errorf("team set A (in org): status = %d, want 200", teamAllowResp.StatusCode)
	}

	// Team sets C (not in org allowlist) — must fail.
	teamDenyReq := httptest.NewRequest("PUT", teamMCPAccessURL(org.ID, team.ID),
		bodyJSON(t, map[string]any{"servers": []string{serverC.ID}}))
	teamDenyReq.Header.Set("Content-Type", "application/json")
	teamDenyReq.Header.Set("Authorization", "Bearer "+testKey)
	teamDenyResp, err := app.Test(teamDenyReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("set team access (deny): %v", err)
	}
	defer teamDenyResp.Body.Close()

	if teamDenyResp.StatusCode != fiber.StatusBadRequest {
		b, _ := io.ReadAll(teamDenyResp.Body)
		t.Errorf("team set C (not in org): status = %d, want 400; body: %s", teamDenyResp.StatusCode, b)
	}
}

func TestSetTeamMCPAccess_OrgHasNoAccess(t *testing.T) {
	t.Parallel()

	// Org has empty allowlist (deny all). Team tries to set any server — must get 400.
	app, database, keyCache := setupMCPServersTestApp(t, "file:TestSetTeamMCPAccess_OrgHasNoAccess?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Empty Org MCP", "empty-org-mcp")
	team := mustCreateTeam(t, database, org.ID, "Team No MCP", "team-no-mcp")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	server := mustCreateGlobalMCPServer(t, database, "Any Server", "any-server-noorg")

	// Org has no MCP access configured (empty allowlist).

	teamReq := httptest.NewRequest("PUT", teamMCPAccessURL(org.ID, team.ID),
		bodyJSON(t, map[string]any{"servers": []string{server.ID}}))
	teamReq.Header.Set("Content-Type", "application/json")
	teamReq.Header.Set("Authorization", "Bearer "+testKey)
	teamResp, err := app.Test(teamReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer teamResp.Body.Close()

	if teamResp.StatusCode != fiber.StatusBadRequest {
		b, _ := io.ReadAll(teamResp.Body)
		t.Errorf("status = %d, want 400 (organization has no MCP servers allowed); body: %s", teamResp.StatusCode, b)
	}
}

func TestSetTeamMCPAccess_RoundTrip(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupMCPServersTestApp(t, "file:TestSetTeamMCPAccess_RoundTrip?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "RT Team MCP Org", "rt-team-mcp-org")
	team := mustCreateTeam(t, database, org.ID, "RT MCP Team", "rt-mcp-team")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	server := mustCreateGlobalMCPServer(t, database, "RT Server", "rt-server-team")

	// Grant org access first.
	orgReq := httptest.NewRequest("PUT", orgMCPAccessURL(org.ID),
		bodyJSON(t, map[string]any{"servers": []string{server.ID}}))
	orgReq.Header.Set("Content-Type", "application/json")
	orgReq.Header.Set("Authorization", "Bearer "+testKey)
	orgResp, err := app.Test(orgReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("set org access: %v", err)
	}
	orgResp.Body.Close()
	if orgResp.StatusCode != fiber.StatusOK {
		t.Fatalf("set org access: status = %d", orgResp.StatusCode)
	}

	// Set team access.
	setReq := httptest.NewRequest("PUT", teamMCPAccessURL(org.ID, team.ID),
		bodyJSON(t, map[string]any{"servers": []string{server.ID}}))
	setReq.Header.Set("Content-Type", "application/json")
	setReq.Header.Set("Authorization", "Bearer "+testKey)
	setResp, err := app.Test(setReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	setResp.Body.Close()
	if setResp.StatusCode != fiber.StatusOK {
		t.Fatalf("PUT status = %d, want 200", setResp.StatusCode)
	}

	// GET back and verify.
	getReq := httptest.NewRequest("GET", teamMCPAccessURL(org.ID, team.ID), nil)
	getReq.Header.Set("Authorization", "Bearer "+testKey)
	getResp, err := app.Test(getReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(getResp.Body)
		t.Fatalf("GET status = %d; body: %s", getResp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, getResp.Body, &got)

	servers, ok := got["servers"].([]any)
	if !ok {
		t.Fatalf("servers is not an array: %v", got["servers"])
	}
	if len(servers) != 1 {
		t.Fatalf("len(servers) = %d, want 1", len(servers))
	}
	if servers[0] != server.ID {
		t.Errorf("servers[0] = %q, want %q", servers[0], server.ID)
	}
}

func TestGetTeamMCPAccess_NonMember(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupMCPServersTestApp(t, "file:TestGetTeamMCPAccess_NonMember?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Non Member MCP Org", "nm-mcp-org")
	team := mustCreateTeam(t, database, org.ID, "NM MCP Team", "nm-mcp-team")

	// A team_admin who is in the org but not the specific team gets 404.
	user := mustCreateUser(t, database, "nonmember-mcp@example.com", "Non Member MCP")
	_, err := database.CreateOrgMembership(context.Background(), db.CreateOrgMembershipParams{
		OrgID:  org.ID,
		UserID: user.ID,
		Role:   auth.RoleTeamAdmin,
	})
	if err != nil {
		t.Fatalf("CreateOrgMembership: %v", err)
	}

	testKey := addTestKeyWithUser(t, keyCache, auth.RoleTeamAdmin, org.ID, user.ID)

	req := httptest.NewRequest("GET", teamMCPAccessURL(org.ID, team.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("non-member GET status = %d, want 404; body: %s", resp.StatusCode, b)
	}
}

// ---- GET /api/v1/orgs/:org_id/keys/:key_id/mcp-access ---------------------------

func TestGetKeyMCPAccess_Empty(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupMCPServersTestApp(t, "file:TestGetKeyMCPAccess_Empty?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Key MCP Org", "key-mcp-org-get")
	user := mustCreateUser(t, database, "key-mcp-user@example.com", "Key MCP User")
	apiKey := mustCreateAPIKeyInDB(t, database, org.ID, user.ID)
	callerKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("GET", keyMCPAccessURL(org.ID, apiKey.ID), nil)
	req.Header.Set("Authorization", "Bearer "+callerKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	servers, ok := got["servers"].([]any)
	if !ok {
		t.Fatalf("servers is not an array: %v", got["servers"])
	}
	if len(servers) != 0 {
		t.Errorf("len(servers) = %d, want 0 (empty allowlist)", len(servers))
	}
}

// ---- PUT /api/v1/orgs/:org_id/keys/:key_id/mcp-access ---------------------------

func TestSetKeyMCPAccess_SubsetConstraint(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupMCPServersTestApp(t, "file:TestSetKeyMCPAccess_SubsetConstraint?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Key Subset Org", "key-subset-mcp-org")
	user := mustCreateUser(t, database, "key-subset-mcp@example.com", "Key Subset User")
	apiKey := mustCreateAPIKeyInDB(t, database, org.ID, user.ID)
	callerKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	serverA := mustCreateGlobalMCPServer(t, database, "Key Server A", "key-server-a-subset")
	serverB := mustCreateGlobalMCPServer(t, database, "Key Server B", "key-server-b-subset")
	serverC := mustCreateGlobalMCPServer(t, database, "Key Server C", "key-server-c-subset")

	// Set org allowlist to A and B only.
	orgReq := httptest.NewRequest("PUT", orgMCPAccessURL(org.ID),
		bodyJSON(t, map[string]any{"servers": []string{serverA.ID, serverB.ID}}))
	orgReq.Header.Set("Content-Type", "application/json")
	orgReq.Header.Set("Authorization", "Bearer "+callerKey)
	orgResp, err := app.Test(orgReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("set org access: %v", err)
	}
	orgResp.Body.Close()
	if orgResp.StatusCode != fiber.StatusOK {
		t.Fatalf("set org access: status = %d, want 200", orgResp.StatusCode)
	}

	// Key sets A (in org allowlist) — must succeed.
	allowReq := httptest.NewRequest("PUT", keyMCPAccessURL(org.ID, apiKey.ID),
		bodyJSON(t, map[string]any{"servers": []string{serverA.ID}}))
	allowReq.Header.Set("Content-Type", "application/json")
	allowReq.Header.Set("Authorization", "Bearer "+callerKey)
	allowResp, err := app.Test(allowReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("set key access (allow): %v", err)
	}
	allowResp.Body.Close()
	if allowResp.StatusCode != fiber.StatusOK {
		t.Errorf("key set A (in org): status = %d, want 200", allowResp.StatusCode)
	}

	// Key sets C (not in org allowlist) — must fail.
	denyReq := httptest.NewRequest("PUT", keyMCPAccessURL(org.ID, apiKey.ID),
		bodyJSON(t, map[string]any{"servers": []string{serverC.ID}}))
	denyReq.Header.Set("Content-Type", "application/json")
	denyReq.Header.Set("Authorization", "Bearer "+callerKey)
	denyResp, err := app.Test(denyReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("set key access (deny): %v", err)
	}
	defer denyResp.Body.Close()

	if denyResp.StatusCode != fiber.StatusBadRequest {
		b, _ := io.ReadAll(denyResp.Body)
		t.Errorf("key set C (not in org): status = %d, want 400; body: %s", denyResp.StatusCode, b)
	}
}

func TestSetKeyMCPAccess_RoundTrip(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupMCPServersTestApp(t, "file:TestSetKeyMCPAccess_RoundTrip?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "RT Key MCP Org", "rt-key-mcp-org")
	user := mustCreateUser(t, database, "rt-key-mcp@example.com", "RT Key MCP User")
	apiKey := mustCreateAPIKeyInDB(t, database, org.ID, user.ID)
	callerKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	server := mustCreateGlobalMCPServer(t, database, "RT Key Server", "rt-key-server")

	// Grant org access first.
	orgReq := httptest.NewRequest("PUT", orgMCPAccessURL(org.ID),
		bodyJSON(t, map[string]any{"servers": []string{server.ID}}))
	orgReq.Header.Set("Content-Type", "application/json")
	orgReq.Header.Set("Authorization", "Bearer "+callerKey)
	orgResp, err := app.Test(orgReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("set org access: %v", err)
	}
	orgResp.Body.Close()
	if orgResp.StatusCode != fiber.StatusOK {
		t.Fatalf("set org access: status = %d", orgResp.StatusCode)
	}

	// Set key access.
	setReq := httptest.NewRequest("PUT", keyMCPAccessURL(org.ID, apiKey.ID),
		bodyJSON(t, map[string]any{"servers": []string{server.ID}}))
	setReq.Header.Set("Content-Type", "application/json")
	setReq.Header.Set("Authorization", "Bearer "+callerKey)
	setResp, err := app.Test(setReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	setResp.Body.Close()
	if setResp.StatusCode != fiber.StatusOK {
		t.Fatalf("PUT status = %d, want 200", setResp.StatusCode)
	}

	// GET back and verify.
	getReq := httptest.NewRequest("GET", keyMCPAccessURL(org.ID, apiKey.ID), nil)
	getReq.Header.Set("Authorization", "Bearer "+callerKey)
	getResp, err := app.Test(getReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(getResp.Body)
		t.Fatalf("GET status = %d; body: %s", getResp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, getResp.Body, &got)

	servers, ok := got["servers"].([]any)
	if !ok {
		t.Fatalf("servers is not an array: %v", got["servers"])
	}
	if len(servers) != 1 {
		t.Fatalf("len(servers) = %d, want 1", len(servers))
	}
	if servers[0] != server.ID {
		t.Errorf("servers[0] = %q, want %q", servers[0], server.ID)
	}
}
