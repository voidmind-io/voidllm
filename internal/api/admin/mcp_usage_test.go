package admin_test

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
)

// insertMCPToolCallHTTP inserts a single mcp_tool_calls row into the given DB.
// Used to seed test data before calling the MCP usage HTTP handler.
func insertMCPToolCallHTTP(t *testing.T, database *db.DB, id, orgID, teamID, serverAlias, toolName, status string, durationMS int, codeMode bool, createdAt time.Time) {
	t.Helper()

	teamVal := "NULL"
	if teamID != "" {
		teamVal = fmt.Sprintf("'%s'", teamID)
	}
	codeModeVal := 0
	if codeMode {
		codeModeVal = 1
	}
	query := fmt.Sprintf(
		`INSERT INTO mcp_tool_calls
			(id, key_id, key_type, org_id, team_id,
			 server_alias, tool_name, duration_ms, status, code_mode, created_at)
		 VALUES
			('%s', 'key-1', 'user_key', '%s', %s,
			 '%s', '%s', %d, '%s', %d, '%s')`,
		id, orgID, teamVal,
		serverAlias, toolName, durationMS, status, codeModeVal,
		createdAt.UTC().Format(time.RFC3339),
	)
	if _, err := database.SQL().ExecContext(context.Background(), query); err != nil {
		t.Fatalf("insertMCPToolCallHTTP id=%q: %v", id, err)
	}
}

// mcpUsageURL constructs the GET /api/v1/orgs/:org_id/mcp-usage URL.
func mcpUsageURL(orgID, from, to, groupBy string) string {
	params := []string{}
	if from != "" {
		params = append(params, "from="+from)
	}
	if to != "" {
		params = append(params, "to="+to)
	}
	if groupBy != "" {
		params = append(params, "group_by="+groupBy)
	}
	u := "/api/v1/orgs/" + orgID + "/mcp-usage"
	if len(params) > 0 {
		u += "?" + strings.Join(params, "&")
	}
	return u
}

// systemMCPUsageURL constructs the GET /api/v1/mcp-usage URL.
func systemMCPUsageURL(from, to, groupBy string) string {
	params := []string{}
	if from != "" {
		params = append(params, "from="+from)
	}
	if to != "" {
		params = append(params, "to="+to)
	}
	if groupBy != "" {
		params = append(params, "group_by="+groupBy)
	}
	u := "/api/v1/mcp-usage"
	if len(params) > 0 {
		u += "?" + strings.Join(params, "&")
	}
	return u
}

// ---- GET /api/v1/orgs/:org_id/mcp-usage -------------------------------------

func TestGetOrgMCPUsage_ValidRange_ReturnsCorrectTotals(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgMCPUsage_Valid?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "MCP Usage Org", "mcp-usage-valid")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	insertMCPToolCallHTTP(t, database, "mu-1", org.ID, "", "server-a", "tool-x", "success", 100, false, now.Add(-90*time.Minute))
	insertMCPToolCallHTTP(t, database, "mu-2", org.ID, "", "server-a", "tool-x", "error", 200, true, now.Add(-60*time.Minute))

	req := httptest.NewRequest("GET", mcpUsageURL(org.ID, from, to, ""), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	if got["org_id"] != org.ID {
		t.Errorf("org_id = %q, want %q", got["org_id"], org.ID)
	}
	if got["from"] == "" {
		t.Error("from field is missing")
	}
	if got["to"] == "" {
		t.Error("to field is missing")
	}

	data, ok := got["data"].([]any)
	if !ok || len(data) == 0 {
		t.Fatalf("data is empty or wrong type: %v", got["data"])
	}

	row := data[0].(map[string]any)
	totalCalls := row["total_calls"].(float64)
	if totalCalls != 2 {
		t.Errorf("total_calls = %v, want 2", totalCalls)
	}
	successCount := row["success_count"].(float64)
	if successCount != 1 {
		t.Errorf("success_count = %v, want 1", successCount)
	}
	codeModeCalls := row["code_mode_calls"].(float64)
	if codeModeCalls != 1 {
		t.Errorf("code_mode_calls = %v, want 1", codeModeCalls)
	}
}

func TestGetOrgMCPUsage_GroupByServer(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgMCPUsage_GroupByServer?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "MCP Server Org", "mcp-usage-server")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	insertMCPToolCallHTTP(t, database, "gs-1", org.ID, "", "server-alpha", "tool-a", "success", 50, false, now.Add(-90*time.Minute))
	insertMCPToolCallHTTP(t, database, "gs-2", org.ID, "", "server-alpha", "tool-b", "success", 60, false, now.Add(-80*time.Minute))
	insertMCPToolCallHTTP(t, database, "gs-3", org.ID, "", "server-beta", "tool-c", "success", 70, false, now.Add(-60*time.Minute))

	req := httptest.NewRequest("GET", mcpUsageURL(org.ID, from, to, "server"), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	if got["group_by"] != "server" {
		t.Errorf("group_by = %q, want %q", got["group_by"], "server")
	}

	data, ok := got["data"].([]any)
	if !ok {
		t.Fatalf("data is not an array: %v", got["data"])
	}
	if len(data) != 2 {
		t.Errorf("len(data) = %d, want 2 (one row per server)", len(data))
	}
}

func TestGetOrgMCPUsage_InvalidTimeRange_MissingFrom_Returns400(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgMCPUsage_NoFrom?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "MCP No From", "mcp-usage-no-from")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	to := now.Add(time.Minute).Format(time.RFC3339)

	req := httptest.NewRequest("GET", mcpUsageURL(org.ID, "", to, ""), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, body)
	}
}

func TestGetOrgMCPUsage_InvalidTimeRange_FromAfterTo_Returns400(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgMCPUsage_FromAfterTo?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "MCP Bad Range", "mcp-usage-bad-range")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	from := now.Add(time.Hour).Format(time.RFC3339)
	to := now.Add(-time.Hour).Format(time.RFC3339)

	req := httptest.NewRequest("GET", mcpUsageURL(org.ID, from, to, ""), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, body)
	}
}

func TestGetOrgMCPUsage_InvalidGroupBy_Returns400(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgMCPUsage_BadGroupBy?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "MCP Bad GroupBy", "mcp-usage-bad-groupby")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	from := now.Add(-time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	req := httptest.NewRequest("GET", mcpUsageURL(org.ID, from, to, "org"), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	// "org" is only valid for system-wide queries, not org-scoped ones.
	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, body)
	}
}

func TestGetOrgMCPUsage_OrgAdminDifferentOrg_Returns403(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgMCPUsage_WrongOrg?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Target MCP Org", "mcp-usage-target")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "00000000-0000-0000-0000-000000000001")

	now := time.Now().UTC()
	from := now.Add(-time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	req := httptest.NewRequest("GET", mcpUsageURL(org.ID, from, to, ""), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 403; body: %s", resp.StatusCode, body)
	}
}

func TestGetOrgMCPUsage_NoAuth_Returns401(t *testing.T) {
	t.Parallel()

	app, database, _ := setupTestApp(t, "file:TestGetOrgMCPUsage_NoAuth?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "MCP Unauth Org", "mcp-usage-noauth")

	now := time.Now().UTC()
	from := now.Add(-time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	req := httptest.NewRequest("GET", mcpUsageURL(org.ID, from, to, ""), nil)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 401; body: %s", resp.StatusCode, body)
	}
}

func TestGetOrgMCPUsage_RangeExceeds90Days_Returns400(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgMCPUsage_91Days?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "MCP 91 Days", "mcp-usage-91d")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	from := now.Add(-91 * 24 * time.Hour).Format(time.RFC3339)
	to := now.Format(time.RFC3339)

	req := httptest.NewRequest("GET", mcpUsageURL(org.ID, from, to, ""), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, body)
	}
}

func TestGetOrgMCPUsage_SystemAdmin_Returns200(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgMCPUsage_SysAdmin?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "MCP SysAdmin Org", "mcp-usage-sysadmin")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	now := time.Now().UTC()
	from := now.Add(-time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	// Insert at least one event so the totals query returns a scannable row.
	insertMCPToolCallHTTP(t, database, "sa-1", org.ID, "", "server-a", "tool-a", "success", 50, false, now.Add(-30*time.Minute))

	req := httptest.NewRequest("GET", mcpUsageURL(org.ID, from, to, ""), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}
}

// ---- GET /api/v1/mcp-usage ---------------------------------------------------

func TestGetSystemMCPUsage_RequiresSystemAdmin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		wantStatus int
	}{
		{name: "org_admin forbidden", role: auth.RoleOrgAdmin, wantStatus: fiber.StatusForbidden},
		{name: "team_admin forbidden", role: auth.RoleTeamAdmin, wantStatus: fiber.StatusForbidden},
		{name: "member forbidden", role: auth.RoleMember, wantStatus: fiber.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			safeName := strings.ReplaceAll(tc.name, " ", "_")
			app, _, keyCache := setupTestApp(t, "file:TestGetSysMCPUsage_"+safeName+"?mode=memory&cache=private")
			testKey := addTestKey(t, keyCache, tc.role, "org-1")

			now := time.Now().UTC()
			from := now.Add(-time.Hour).Format(time.RFC3339)
			to := now.Add(time.Minute).Format(time.RFC3339)

			req := httptest.NewRequest("GET", systemMCPUsageURL(from, to, ""), nil)
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("role=%q: status = %d, want %d; body: %s", tc.role, resp.StatusCode, tc.wantStatus, body)
			}
		})
	}
}

// TestGetSystemMCPUsage_SystemAdmin_Returns200 verifies that a system admin key
// can successfully call the system-wide MCP usage endpoint when data exists.
func TestGetSystemMCPUsage_SystemAdmin_Returns200(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetSysMCPUsage_SysAdmin200?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	now := time.Now().UTC()
	from := now.Add(-time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	// Insert data so the totals query returns a scannable row.
	insertMCPToolCallHTTP(t, database, "sys-ok-1", "org-sys-ok", "", "server-a", "tool-a", "success", 50, false, now.Add(-30*time.Minute))

	req := httptest.NewRequest("GET", systemMCPUsageURL(from, to, "server"), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}
}

func TestGetSystemMCPUsage_NoAuth_Returns401(t *testing.T) {
	t.Parallel()

	app, _, _ := setupTestApp(t, "file:TestGetSysMCPUsage_NoAuth?mode=memory&cache=private")

	now := time.Now().UTC()
	from := now.Add(-time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	req := httptest.NewRequest("GET", systemMCPUsageURL(from, to, ""), nil)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 401; body: %s", resp.StatusCode, body)
	}
}

func TestGetSystemMCPUsage_GroupByOrg_ReturnsCorrectData(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetSysMCPUsage_GroupByOrg?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	now := time.Now().UTC()
	from := now.Add(-time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	insertMCPToolCallHTTP(t, database, "sys-1", "org-sys-a", "", "server-a", "tool-a", "success", 50, false, now.Add(-30*time.Minute))
	insertMCPToolCallHTTP(t, database, "sys-2", "org-sys-b", "", "server-a", "tool-a", "success", 60, false, now.Add(-20*time.Minute))

	req := httptest.NewRequest("GET", systemMCPUsageURL(from, to, "org"), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	if got["group_by"] != "org" {
		t.Errorf("group_by = %q, want %q", got["group_by"], "org")
	}
	// org_id must not be present in the system-wide response envelope.
	if _, hasOrgID := got["org_id"]; hasOrgID {
		t.Error("org_id field must not be present in system-wide MCP usage response")
	}

	data, ok := got["data"].([]any)
	if !ok {
		t.Fatalf("data is not an array: %v", got["data"])
	}
	if len(data) != 2 {
		t.Errorf("len(data) = %d, want 2 (one row per org)", len(data))
	}
}

func TestGetSystemMCPUsage_InvalidGroupBy_Returns400(t *testing.T) {
	t.Parallel()

	app, _, keyCache := setupTestApp(t, "file:TestGetSysMCPUsage_BadGroupBy?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	now := time.Now().UTC()
	from := now.Add(-time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	req := httptest.NewRequest("GET", systemMCPUsageURL(from, to, "notvalid"), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, body)
	}
}

func TestGetSystemMCPUsage_MissingFrom_Returns400(t *testing.T) {
	t.Parallel()

	app, _, keyCache := setupTestApp(t, "file:TestGetSysMCPUsage_NoFrom?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	now := time.Now().UTC()
	to := now.Add(time.Minute).Format(time.RFC3339)

	req := httptest.NewRequest("GET", systemMCPUsageURL("", to, ""), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, body)
	}
}
