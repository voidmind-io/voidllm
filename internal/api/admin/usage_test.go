package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/license"
	"github.com/voidmind-io/voidllm/internal/proxy"
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

// insertUsageEventWithCacheHTTP inserts a single usage_events row with
// explicit cached_read_tokens/cache_write_tokens values, for tests that
// verify the cache-token fields surface through the HTTP usage endpoints.
func insertUsageEventWithCacheHTTP(t *testing.T, database *db.DB, id, keyID, teamID, orgID, modelName string,
	promptTokens, compTokens, totalTokens, cachedReadTokens, cacheWriteTokens int64, createdAt time.Time) {
	t.Helper()

	teamVal := "NULL"
	if teamID != "" {
		teamVal = fmt.Sprintf("'%s'", teamID)
	}
	query := fmt.Sprintf(
		`INSERT INTO usage_events
			(id, key_id, key_type, org_id, team_id, model_name,
			 prompt_tokens, completion_tokens, total_tokens,
			 cached_read_tokens, cache_write_tokens, status_code, created_at)
		 VALUES
			('%s', '%s', 'user_key', '%s', %s, '%s',
			 %d, %d, %d,
			 %d, %d, 200, '%s')`,
		id, keyID, orgID, teamVal, modelName,
		promptTokens, compTokens, totalTokens,
		cachedReadTokens, cacheWriteTokens,
		createdAt.UTC().Format(time.RFC3339),
	)
	if _, err := database.SQL().ExecContext(context.Background(), query); err != nil {
		t.Fatalf("insertUsageEventWithCacheHTTP id=%q: %v", id, err)
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

// TestGetOrgUsage_ResponseShape_CachedTokens verifies that
// cached_read_tokens and cache_write_tokens (added for #179) appear in the
// GET /api/v1/orgs/:org_id/usage JSON response and carry the summed values
// from the underlying usage_events rows.
func TestGetOrgUsage_ResponseShape_CachedTokens(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_ShapeCached?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Shape Cached Org", "usage-org-shape-cached")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now.Add(time.Minute)

	insertUsageEventWithCacheHTTP(t, database, "shape-cache-1", "key-shape-cache", "", org.ID, "claude-3-5-sonnet",
		100, 50, 150, 40, 15, now.Add(-30*time.Minute))

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
		Data []struct {
			TotalTokens      int64 `json:"total_tokens"`
			CachedReadTokens int64 `json:"cached_read_tokens"`
			CacheWriteTokens int64 `json:"cache_write_tokens"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(envelope.Data) == 0 {
		t.Fatal("data is empty, want at least one row")
	}

	row := envelope.Data[0]
	if row.TotalTokens != 150 {
		t.Errorf("total_tokens = %d, want 150", row.TotalTokens)
	}
	if row.CachedReadTokens != 40 {
		t.Errorf("cached_read_tokens = %d, want 40", row.CachedReadTokens)
	}
	if row.CacheWriteTokens != 15 {
		t.Errorf("cache_write_tokens = %d, want 15", row.CacheWriteTokens)
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

// insertUsageEventWithUserHTTP inserts a single usage_events row that includes
// a user_id column so that group_by=user label resolution can be tested.
func insertUsageEventWithUserHTTP(t *testing.T, database *db.DB, id, keyID, userID, orgID, modelName string, totalTokens int64, createdAt time.Time) {
	t.Helper()
	query := fmt.Sprintf(
		`INSERT INTO usage_events
			(id, key_id, key_type, org_id, user_id, model_name,
			 prompt_tokens, completion_tokens, total_tokens, status_code, created_at)
		 VALUES
			('%s', '%s', 'user_key', '%s', '%s', '%s',
			 0, %d, %d, 200, '%s')`,
		id, keyID, orgID, userID, modelName,
		totalTokens, totalTokens,
		createdAt.UTC().Format(time.RFC3339),
	)
	if _, err := database.SQL().ExecContext(context.Background(), query); err != nil {
		t.Fatalf("insertUsageEventWithUserHTTP id=%q: %v", id, err)
	}
}

// ---- group_label enrichment tests -------------------------------------------

// TestGetOrgUsage_GroupByKey_HasGroupLabel verifies that when group_by=key and
// the key exists in api_keys, each data point carries a populated group_label.
func TestGetOrgUsage_GroupByKey_HasGroupLabel(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_GBKey_Label?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Key Label Org", "key-label-org")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	// Create a real user + api_key so ResolveGroupLabels can find it.
	creator, err := database.CreateUser(context.Background(), db.CreateUserParams{
		Email:       "key-label-creator@example.com",
		DisplayName: "Creator",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	apiKey, err := database.CreateAPIKey(context.Background(), db.CreateAPIKeyParams{
		KeyHash:   "dummy-hash-key-label-test",
		KeyHint:   "vl_uk_...test",
		KeyType:   "user_key",
		Name:      "My Labeled Key",
		OrgID:     org.ID,
		CreatedBy: creator.ID,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	// Insert a usage event whose key_id matches the real api_key row.
	insertUsageEventHTTP(t, database, "gl-key-1", apiKey.ID, "", org.ID, "gpt-4",
		100, 50, 150, now.Add(-30*time.Minute))

	req := httptest.NewRequest("GET", usageURL(org.ID, from, to, "key"), nil)
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
		Data []struct {
			GroupKey   string `json:"group_key"`
			GroupLabel string `json:"group_label"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(envelope.Data) == 0 {
		t.Fatal("data is empty, want at least one row")
	}

	row := envelope.Data[0]
	if row.GroupKey != apiKey.ID {
		t.Errorf("group_key = %q, want %q", row.GroupKey, apiKey.ID)
	}
	if row.GroupLabel != "My Labeled Key" {
		t.Errorf("group_label = %q, want %q", row.GroupLabel, "My Labeled Key")
	}
}

// TestGetOrgUsage_GroupByUser_HasGroupLabel verifies group_label is set for
// group_by=user when the user exists in the users table.
func TestGetOrgUsage_GroupByUser_HasGroupLabel(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_GBUser_Label?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "User Label Org", "user-label-org")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	// Create a real user whose ID we'll embed in usage_events.user_id.
	user, err := database.CreateUser(context.Background(), db.CreateUserParams{
		Email:       "labeled-user@example.com",
		DisplayName: "Labeled Display Name",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	insertUsageEventWithUserHTTP(t, database, "gl-user-1", "key-gl-user", user.ID, org.ID, "gpt-4",
		200, now.Add(-30*time.Minute))

	req := httptest.NewRequest("GET", usageURL(org.ID, from, to, "user"), nil)
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
		Data []struct {
			GroupKey   string `json:"group_key"`
			GroupLabel string `json:"group_label"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(envelope.Data) == 0 {
		t.Fatal("data is empty, want at least one row")
	}

	row := envelope.Data[0]
	if row.GroupKey != user.ID {
		t.Errorf("group_key = %q, want %q", row.GroupKey, user.ID)
	}
	if row.GroupLabel != "Labeled Display Name" {
		t.Errorf("group_label = %q, want %q", row.GroupLabel, "Labeled Display Name")
	}
}

// TestGetOrgUsage_GroupByModel_NoGroupLabel verifies that group_by=model data
// points do NOT carry a group_label field (non-resolvable dimension; omitempty
// means the JSON key must be absent entirely).
func TestGetOrgUsage_GroupByModel_NoGroupLabel(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_GBModel_NoLabel?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Model Label Org", "model-label-org")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	insertUsageEventHTTP(t, database, "gl-model-1", "key-model", "", org.ID, "gpt-4o",
		100, 50, 150, now.Add(-30*time.Minute))

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

	// Decode into a raw map so we can detect the absence of "group_label".
	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(envelope.Data) == 0 {
		t.Fatal("data is empty, want at least one row")
	}

	row := envelope.Data[0]
	if _, hasLabel := row["group_label"]; hasLabel {
		t.Errorf("group_label must be absent for group_by=model (omitempty), but it is present: %v", row["group_label"])
	}
}

// TestGetOrgUsage_GroupByDay_NoGroupLabel verifies that group_by=day data
// points also do not carry a group_label (omitempty absent in JSON).
func TestGetOrgUsage_GroupByDay_NoGroupLabel(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_GBDay_NoLabel?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Day Label Org", "day-label-org")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	day := time.Date(2024, 4, 1, 12, 0, 0, 0, time.UTC)
	from := time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	to := time.Date(2024, 4, 1, 23, 59, 59, 0, time.UTC).Format(time.RFC3339)

	insertUsageEventHTTP(t, database, "gl-day-1", "key-day", "", org.ID, "gpt-4",
		50, 20, 70, day)

	req := httptest.NewRequest("GET", usageURL(org.ID, from, to, "day"), nil)
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
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(envelope.Data) == 0 {
		t.Fatal("data is empty, want at least one row")
	}

	row := envelope.Data[0]
	if _, hasLabel := row["group_label"]; hasLabel {
		t.Errorf("group_label must be absent for group_by=day (omitempty), but present: %v", row["group_label"])
	}
}

// TestGetOrgUsage_GroupByKey_UnknownKeyID_NoGroupLabel verifies that when the
// key_id in usage_events does not exist in api_keys, the data point has no
// group_label (absent due to omitempty) and the request succeeds.
func TestGetOrgUsage_GroupByKey_UnknownKeyID_NoGroupLabel(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetOrgUsage_GBKey_Unknown?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Unknown Key Org", "unknown-key-org")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	// Insert event whose key_id does not correspond to any api_keys row.
	insertUsageEventHTTP(t, database, "gl-unk-1", "nonexistent-key-id", "", org.ID, "gpt-4",
		100, 50, 150, now.Add(-30*time.Minute))

	req := httptest.NewRequest("GET", usageURL(org.ID, from, to, "key"), nil)
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
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(envelope.Data) == 0 {
		t.Fatal("data is empty, want at least one row")
	}

	row := envelope.Data[0]
	// group_key must be the raw ID; group_label must be absent (no entity found).
	if row["group_key"] != "nonexistent-key-id" {
		t.Errorf("group_key = %v, want %q", row["group_key"], "nonexistent-key-id")
	}
	if _, hasLabel := row["group_label"]; hasLabel {
		t.Errorf("group_label must be absent when key is unknown, but present: %v", row["group_label"])
	}
}

// ---- cache savings -----------------------------------------------------------

func TestCacheSavings(t *testing.T) {
	t.Parallel()

	f := func(v float64) *float64 { return &v }

	tests := []struct {
		name        string
		pricing     config.PricingConfig
		prompt      int64
		read, write int64
		want        *float64
	}{
		{"cached price unset falls back to input", config.PricingConfig{InputPer1M: 3}, 5000, 1000, 0, f(0)},
		{"write price unset has no premium", config.PricingConfig{InputPer1M: 3, CachedInputPer1M: 0.3}, 5000, 1000, 500, f(0.0027)},
		{"write premium makes savings negative", config.PricingConfig{InputPer1M: 3, CacheWritePer1M: 3.75}, 5000, 0, 1000, f(-0.00075)},
		{"read and write combined", config.PricingConfig{InputPer1M: 3, CachedInputPer1M: 0.3, CacheWritePer1M: 3.75}, 5000, 1000, 200, f(0.00255)},
		{"no input price", config.PricingConfig{CachedInputPer1M: 0.3}, 5000, 1000, 200, nil},
		{"fresh clamped at zero", config.PricingConfig{InputPer1M: 10, CachedInputPer1M: 5, CacheWritePer1M: 8}, 100, 80, 50, f(0.0002)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := admin.CacheSavings(tt.pricing, tt.prompt, tt.read, tt.write)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil, want %v", *tt.want)
			}
			if math.Abs(*got-*tt.want) > 1e-12 {
				t.Errorf("got %v, want %v", *got, *tt.want)
			}
		})
	}
}

// TestCacheSavings_ConsistentWithHandlerCost verifies that savings equal the
// no-cache cost of the prompt minus the cost computed with the proxy handler's
// formula (fresh*in + read*rr + write*wr + completion*out, fresh clamped at
// zero). The proxy computes cost inline in logUsageEvent and no shared pure
// function exists, so the formula is restated explicitly here, for both a
// normal and a clamped case.
func TestCacheSavings_ConsistentWithHandlerCost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                            string
		p                               config.PricingConfig
		prompt, completion, read, write int64
	}{
		{"normal", config.PricingConfig{InputPer1M: 3, OutputPer1M: 15, CachedInputPer1M: 0.3, CacheWritePer1M: 3.75}, 5000, 700, 1000, 200},
		{"clamped", config.PricingConfig{InputPer1M: 10, OutputPer1M: 20, CachedInputPer1M: 5, CacheWritePer1M: 8}, 100, 30, 80, 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := tt.p
			fresh := tt.prompt - tt.read - tt.write
			if fresh < 0 {
				fresh = 0
			}
			cost := (float64(fresh)*p.InputPer1M + float64(tt.read)*p.CachedInputPer1M +
				float64(tt.write)*p.CacheWritePer1M + float64(tt.completion)*p.OutputPer1M) / 1e6
			noCache := (float64(tt.prompt)*p.InputPer1M + float64(tt.completion)*p.OutputPer1M) / 1e6

			got := admin.CacheSavings(p, tt.prompt, tt.read, tt.write)
			if got == nil {
				t.Fatal("got nil savings")
			}
			if want := noCache - cost; math.Abs(*got-want) > 1e-12 {
				t.Errorf("savings = %v, want %v", *got, want)
			}
		})
	}
}

// setupUsageAppWithRegistry builds an app whose handler uses the given model
// registry (which may be nil).
func setupUsageAppWithRegistry(t *testing.T, dsn string, registry *proxy.Registry) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo]) {
	t.Helper()

	ctx := context.Background()
	database, err := db.Open(ctx, config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             dsn,
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	keyCache := cache.New[string, auth.KeyInfo]()
	handler := &admin.Handler{
		DB:         database,
		HMACSecret: testHMACSecret,
		KeyCache:   keyCache,
		Registry:   registry,
		License:    license.NewHolder(license.Verify("", true)),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)
	return app, database, keyCache
}

// fetchCacheSavings seeds one cached usage event per model, queries the org
// usage endpoint with groupBy and returns cache_savings keyed by group_key (or
// "" for ungrouped rows). A nil value means the field was absent.
func fetchCacheSavings(t *testing.T, registry *proxy.Registry, dsn, groupBy string, models ...string) map[string]*float64 {
	t.Helper()

	app, database, keyCache := setupUsageAppWithRegistry(t, dsn, registry)
	org := mustCreateOrg(t, database, "Savings Org", "savings-"+strings.ToLower(t.Name()))
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	now := time.Now().UTC()
	for i, m := range models {
		insertUsageEventWithCacheHTTP(t, database, fmt.Sprintf("sav-%d", i), "key-sav", "", org.ID, m,
			5000, 100, 5100, 1000, 200, now.Add(-30*time.Minute))
	}

	req := httptest.NewRequest("GET", usageURL(org.ID, now.Add(-time.Hour).Format(time.RFC3339), now.Add(time.Minute).Format(time.RFC3339), groupBy), nil)
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
		Data []struct {
			GroupKey     string   `json:"group_key"`
			CacheSavings *float64 `json:"cache_savings"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(envelope.Data) == 0 {
		t.Fatal("data is empty")
	}
	out := make(map[string]*float64, len(envelope.Data))
	for _, d := range envelope.Data {
		out[d.GroupKey] = d.CacheSavings
	}
	return out
}

func newSavingsRegistry(t *testing.T) *proxy.Registry {
	t.Helper()
	reg, err := proxy.NewRegistry([]config.ModelConfig{
		{
			Name: "priced", Provider: "anthropic", BaseURL: "https://api.anthropic.com",
			Pricing: config.PricingConfig{InputPer1M: 3, OutputPer1M: 15, CachedInputPer1M: 0.3, CacheWritePer1M: 3.75},
		},
		{Name: "unpriced", Provider: "openai", BaseURL: "https://api.openai.com/v1"},
	})
	if err != nil {
		t.Fatalf("proxy.NewRegistry: %v", err)
	}
	return reg
}

func TestGetOrgUsage_CacheSavings_GroupByModel(t *testing.T) {
	t.Parallel()

	got := fetchCacheSavings(t, newSavingsRegistry(t), "file:TestUsageSavingsModel?mode=memory&cache=private",
		"model", "priced", "unpriced", "unknown-model")

	if v := got["priced"]; v == nil {
		t.Error("priced: cache_savings absent, want 0.00255")
	} else if math.Abs(*v-0.00255) > 1e-9 {
		t.Errorf("priced: cache_savings = %v, want 0.00255", *v)
	}
	for _, m := range []string{"unpriced", "unknown-model"} {
		if got[m] != nil {
			t.Errorf("%s: cache_savings = %v, want absent", m, *got[m])
		}
	}
}

func TestGetOrgUsage_CacheSavings_AbsentForDayGrouping(t *testing.T) {
	t.Parallel()

	got := fetchCacheSavings(t, newSavingsRegistry(t), "file:TestUsageSavingsDay?mode=memory&cache=private",
		"day", "priced")
	for k, v := range got {
		if v != nil {
			t.Errorf("group %q: cache_savings = %v, want absent", k, *v)
		}
	}
}

func TestGetOrgUsage_CacheSavings_AbsentWithNilRegistry(t *testing.T) {
	t.Parallel()

	got := fetchCacheSavings(t, nil, "file:TestUsageSavingsNilReg?mode=memory&cache=private",
		"model", "priced")
	if got["priced"] != nil {
		t.Errorf("cache_savings = %v, want absent", *got["priced"])
	}
}
