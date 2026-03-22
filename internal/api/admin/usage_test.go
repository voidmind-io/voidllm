package admin_test

import (
	"context"
	"encoding/json"
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

// insertUsageEventHTTP inserts a single usage_events row into the given DB.
// Used to seed test data before calling the HTTP handler.
func insertUsageEventHTTP(t *testing.T, database *db.DB, id, keyID, teamID, orgID, modelName string, promptTokens, compTokens, totalTokens int64, createdAt time.Time) {
	t.Helper()

	teamVal := "NULL"
	if teamID != "" {
		teamVal = fmt.Sprintf("'%s'", teamID)
	}
	query := fmt.Sprintf(
		`INSERT INTO usage_events
			(id, key_id, key_type, org_id, team_id, model_name,
			 prompt_tokens, completion_tokens, total_tokens, status_code, created_at)
		 VALUES
			('%s', '%s', 'user_key', '%s', %s, '%s',
			 %d, %d, %d, 200, '%s')`,
		id, keyID, orgID, teamVal, modelName,
		promptTokens, compTokens, totalTokens,
		createdAt.UTC().Format(time.RFC3339),
	)
	if _, err := database.SQL().ExecContext(context.Background(), query); err != nil {
		t.Fatalf("insertUsageEventHTTP id=%q: %v", id, err)
	}
}

// usageURL constructs the GET /api/v1/orgs/:org_id/usage URL with the given query params.
func usageURL(orgID, from, to, groupBy string) string {
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
	u := "/api/v1/orgs/" + orgID + "/usage"
	if len(params) > 0 {
		u += "?" + strings.Join(params, "&")
	}
	return u
}

// ---- GET /api/v1/orgs/:org_id/usage ------------------------------------------

func TestGetOrgUsage_ValidRange_ReturnsCorrectTotals(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_ValidRange?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Usage Org", "usage-org-valid")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	insertUsageEventHTTP(t, database, "uh-1", "key-1", "", org.ID, "gpt-4", 100, 50, 150, now.Add(-90*time.Minute))
	insertUsageEventHTTP(t, database, "uh-2", "key-1", "", org.ID, "gpt-4", 200, 80, 280, now.Add(-60*time.Minute))

	req := httptest.NewRequest("GET", usageURL(org.ID, from, to, ""), nil)
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

	data, ok := got["data"].([]any)
	if !ok || len(data) == 0 {
		t.Fatalf("data is empty or wrong type: %v", got["data"])
	}

	row := data[0].(map[string]any)
	totalRequests := row["total_requests"].(float64)
	if totalRequests != 2 {
		t.Errorf("total_requests = %v, want 2", totalRequests)
	}
	totalTokens := row["total_tokens"].(float64)
	if totalTokens != 430 {
		t.Errorf("total_tokens = %v, want 430", totalTokens)
	}
}

func TestGetOrgUsage_GroupByModel(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_GroupByModel?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Usage Model Org", "usage-org-model")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	insertUsageEventHTTP(t, database, "um-1", "key-1", "", org.ID, "gpt-4", 100, 50, 150, now.Add(-90*time.Minute))
	insertUsageEventHTTP(t, database, "um-2", "key-1", "", org.ID, "claude-3", 200, 80, 280, now.Add(-60*time.Minute))
	insertUsageEventHTTP(t, database, "um-3", "key-1", "", org.ID, "gpt-4", 50, 20, 70, now.Add(-30*time.Minute))

	req := httptest.NewRequest("GET", usageURL(org.ID, from, to, "model"), nil)
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

	if got["group_by"] != "model" {
		t.Errorf("group_by = %q, want %q", got["group_by"], "model")
	}

	data, ok := got["data"].([]any)
	if !ok {
		t.Fatalf("data is not an array: %v", got["data"])
	}
	if len(data) != 2 {
		t.Errorf("len(data) = %d, want 2 (one row per model)", len(data))
	}
}

func TestGetOrgUsage_MissingFrom_Returns400(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_MissingFrom?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Usage Org", "usage-org-no-from")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	to := now.Add(time.Minute).Format(time.RFC3339)

	req := httptest.NewRequest("GET", usageURL(org.ID, "", to, ""), nil)
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

func TestGetOrgUsage_MissingTo_Returns400(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_MissingTo?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Usage Org", "usage-org-no-to")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	from := now.Add(-time.Hour).Format(time.RFC3339)

	req := httptest.NewRequest("GET", usageURL(org.ID, from, "", ""), nil)
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

func TestGetOrgUsage_FromAfterTo_Returns400(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_FromAfterTo?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Usage Org", "usage-org-bad-range")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	// from is after to.
	from := now.Add(time.Hour).Format(time.RFC3339)
	to := now.Add(-time.Hour).Format(time.RFC3339)

	req := httptest.NewRequest("GET", usageURL(org.ID, from, to, ""), nil)
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

func TestGetOrgUsage_InvalidFromFormat_Returns400(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_InvalidFrom?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Usage Org", "usage-org-bad-from")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	to := now.Add(time.Minute).Format(time.RFC3339)

	req := httptest.NewRequest("GET", usageURL(org.ID, "not-a-date", to, ""), nil)
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

func TestGetOrgUsage_InvalidGroupBy_Returns400(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_InvalidGroupBy?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Usage Org", "usage-org-bad-groupby")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	from := now.Add(-time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	req := httptest.NewRequest("GET", usageURL(org.ID, from, to, "invalid_col"), nil)
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

func TestGetOrgUsage_RangeExceeds90Days_Returns400(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_91Days?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Usage Org", "usage-org-91d")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	from := now.Add(-91 * 24 * time.Hour).Format(time.RFC3339)
	to := now.Format(time.RFC3339)

	req := httptest.NewRequest("GET", usageURL(org.ID, from, to, ""), nil)
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

func TestGetOrgUsage_OrgAdminDifferentOrg_Returns403(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_WrongOrg?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Target Org", "usage-org-target")
	// Key is scoped to a different org.
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "00000000-0000-0000-0000-000000000001")

	now := time.Now().UTC()
	from := now.Add(-time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	req := httptest.NewRequest("GET", usageURL(org.ID, from, to, ""), nil)
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

func TestGetOrgUsage_SystemAdmin_Returns200(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_SysAdmin?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Admin Org", "usage-org-sysadmin")
	// system_admin key with no org binding.
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	now := time.Now().UTC()
	from := now.Add(-time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	req := httptest.NewRequest("GET", usageURL(org.ID, from, to, ""), nil)
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

func TestGetOrgUsage_NoAuth_Returns401(t *testing.T) {
	t.Parallel()

	app, database, _ := setupTestApp(t, "file:TestGetOrgUsage_NoAuth?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Unauth Org", "usage-org-noauth")

	now := time.Now().UTC()
	from := now.Add(-time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	req := httptest.NewRequest("GET", usageURL(org.ID, from, to, ""), nil)
	// No Authorization header.

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

func TestGetOrgUsage_MemberRole_Returns403(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_Member?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Member Org", "usage-org-member")
	testKey := addTestKey(t, keyCache, auth.RoleMember, org.ID)

	now := time.Now().UTC()
	from := now.Add(-time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	req := httptest.NewRequest("GET", usageURL(org.ID, from, to, ""), nil)
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

// TestGetOrgUsage_TableDriven_Validation exercises all validation branches in a
// single table-driven test to avoid repetitive setup.
func TestGetOrgUsage_TableDriven_Validation(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	validFrom := now.Add(-time.Hour).Format(time.RFC3339)
	validTo := now.Add(time.Minute).Format(time.RFC3339)

	tests := []struct {
		name       string
		from       string
		to         string
		groupBy    string
		wantStatus int
	}{
		{
			name:       "valid request no groupBy",
			from:       validFrom,
			to:         validTo,
			groupBy:    "",
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "valid request groupBy model",
			from:       validFrom,
			to:         validTo,
			groupBy:    "model",
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "valid request groupBy team",
			from:       validFrom,
			to:         validTo,
			groupBy:    "team",
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "valid request groupBy key",
			from:       validFrom,
			to:         validTo,
			groupBy:    "key",
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "valid request groupBy day",
			from:       validFrom,
			to:         validTo,
			groupBy:    "day",
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "missing from",
			from:       "",
			to:         validTo,
			groupBy:    "",
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "missing to",
			from:       validFrom,
			to:         "",
			groupBy:    "",
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "from equals to",
			from:       validFrom,
			to:         validFrom,
			groupBy:    "",
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "invalid from format",
			from:       "2024-01-01",
			to:         validTo,
			groupBy:    "",
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "invalid to format",
			from:       validFrom,
			to:         "yesterday",
			groupBy:    "",
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "invalid group_by value",
			from:       validFrom,
			to:         validTo,
			groupBy:    "unknown",
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "range exactly 90 days is allowed",
			from:       now.Add(-90 * 24 * time.Hour).Format(time.RFC3339),
			to:         now.Format(time.RFC3339),
			groupBy:    "",
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "range over 90 days is rejected",
			from:       now.Add(-91 * 24 * time.Hour).Format(time.RFC3339),
			to:         now.Format(time.RFC3339),
			groupBy:    "",
			wantStatus: fiber.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			safeName := strings.ReplaceAll(tc.name, " ", "_")
			dsn := fmt.Sprintf("file:TestGetOrgUsage_TD_%s?mode=memory&cache=private", safeName)
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "Val Org", "val-org-"+safeName)
			testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

			req := httptest.NewRequest("GET", usageURL(org.ID, tc.from, tc.to, tc.groupBy), nil)
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, body)
			}
		})
	}
}

// TestGetOrgUsage_ResponseShape verifies all expected JSON fields are present
// and have the correct types.
func TestGetOrgUsage_ResponseShape(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_Shape?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Shape Org", "usage-org-shape")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now.Add(time.Minute)

	insertUsageEventHTTP(t, database, "shape-1", "key-shape", "", org.ID, "gpt-4",
		100, 50, 150, now.Add(-30*time.Minute))

	req := httptest.NewRequest("GET", usageURL(org.ID, from.Format(time.RFC3339), to.Format(time.RFC3339), ""), nil)
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

	var envelope struct {
		OrgID   string `json:"org_id"`
		From    string `json:"from"`
		To      string `json:"to"`
		GroupBy string `json:"group_by"`
		Data    []struct {
			GroupKey         string  `json:"group_key"`
			TotalRequests    int64   `json:"total_requests"`
			PromptTokens     int64   `json:"prompt_tokens"`
			CompletionTokens int64   `json:"completion_tokens"`
			TotalTokens      int64   `json:"total_tokens"`
			CostEstimate     float64 `json:"cost_estimate"`
			AvgDurationMS    float64 `json:"avg_duration_ms"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if envelope.OrgID != org.ID {
		t.Errorf("org_id = %q, want %q", envelope.OrgID, org.ID)
	}
	if envelope.From == "" {
		t.Error("from is empty")
	}
	if envelope.To == "" {
		t.Error("to is empty")
	}
	if len(envelope.Data) == 0 {
		t.Fatal("data is empty, want at least one row")
	}

	row := envelope.Data[0]
	if row.TotalRequests != 1 {
		t.Errorf("total_requests = %d, want 1", row.TotalRequests)
	}
	if row.PromptTokens != 100 {
		t.Errorf("prompt_tokens = %d, want 100", row.PromptTokens)
	}
	if row.CompletionTokens != 50 {
		t.Errorf("completion_tokens = %d, want 50", row.CompletionTokens)
	}
	if row.TotalTokens != 150 {
		t.Errorf("total_tokens = %d, want 150", row.TotalTokens)
	}
}

// TestGetOrgUsage_EventsOutsideRange ensures only events within [from, to] are
// counted when querying via the HTTP handler.
func TestGetOrgUsage_EventsOutsideRange_NotCounted(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_Range?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Range Org", "usage-org-range")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now.Add(-10 * time.Minute)

	// In range.
	insertUsageEventHTTP(t, database, "range-in", "key-range", "", org.ID, "gpt-4",
		100, 50, 150, now.Add(-30*time.Minute))
	// Before range.
	insertUsageEventHTTP(t, database, "range-before", "key-range", "", org.ID, "gpt-4",
		9999, 9999, 9999, now.Add(-2*time.Hour))
	// After range.
	insertUsageEventHTTP(t, database, "range-after", "key-range", "", org.ID, "gpt-4",
		9999, 9999, 9999, now.Add(-5*time.Minute))

	req := httptest.NewRequest("GET", usageURL(org.ID, from.Format(time.RFC3339), to.Format(time.RFC3339), ""), nil)
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

	data, ok := got["data"].([]any)
	if !ok || len(data) == 0 {
		t.Fatal("data is empty or wrong type")
	}

	row := data[0].(map[string]any)
	totalRequests := row["total_requests"].(float64)
	if totalRequests != 1 {
		t.Errorf("total_requests = %v, want 1 (only in-range event)", totalRequests)
	}
	totalTokens := row["total_tokens"].(float64)
	if totalTokens != 150 {
		t.Errorf("total_tokens = %v, want 150", totalTokens)
	}
}
