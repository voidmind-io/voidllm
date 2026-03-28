package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// buildVoidLLMServer returns a Server with all VoidLLM tools registered using
// the provided deps.
func buildVoidLLMServer(deps mcp.VoidLLMDeps) *mcp.Server {
	s := mcp.NewServer("voidllm-test", "0.1.0")
	mcp.RegisterVoidLLMTools(s, deps)
	return s
}

// callTool sends a tools/call request and returns the decoded ToolResult.
// It fatals if the response has a JSON-RPC protocol error.
func callTool(t *testing.T, s *mcp.Server, ctx context.Context, toolName string, args map[string]any) *mcp.ToolResult {
	t.Helper()

	argsBytes, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	params := map[string]any{
		"name":      toolName,
		"arguments": json.RawMessage(argsBytes),
	}
	paramsBytes, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  json.RawMessage(paramsBytes),
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	raw := s.Handle(ctx, reqBytes)
	if raw == nil {
		t.Fatalf("server returned nil for tools/call")
	}

	var resp mcp.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Error != nil {
		t.Fatalf("unexpected protocol error: code=%d msg=%q", resp.Error.Code, resp.Error.Message)
	}

	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("re-marshal result: %v", err)
	}
	var tr mcp.ToolResult
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatalf("decode ToolResult: %v", err)
	}
	return &tr
}

// defaultDeps returns a minimal valid VoidLLMDeps with all fields populated.
func defaultDeps() mcp.VoidLLMDeps {
	return mcp.VoidLLMDeps{
		ListModels: func(_ context.Context) ([]map[string]any, error) {
			return []map[string]any{
				{"name": "gpt-4o", "provider": "openai", "type": "chat"},
				{"name": "claude-3", "provider": "anthropic", "type": "chat"},
				{"name": "llama3", "provider": "vllm", "type": "chat"},
			}, nil
		},
		ListAvailableModels: func(_ context.Context) ([]map[string]any, error) {
			return []map[string]any{
				{"name": "gpt-4o", "type": "chat"},
				{"name": "llama3", "type": "chat"},
			}, nil
		},
		GetAllHealth: func() []map[string]any {
			return []map[string]any{
				{"name": "gpt-4o", "status": "healthy", "latency_ms": float64(42)},
				{"name": "llama3", "status": "unhealthy", "latency_ms": float64(0)},
			}
		},
		GetHealth: func(key string) (map[string]any, bool) {
			db := map[string]map[string]any{
				"gpt-4o":   {"name": "gpt-4o", "status": "healthy", "latency_ms": float64(42)},
				"claude-3": {"name": "claude-3", "status": "healthy", "latency_ms": float64(88)},
			}
			h, ok := db[key]
			return h, ok
		},
		GetUsage: func(_ context.Context, from, to, groupBy, orgID, keyID string) (any, error) {
			return map[string]any{
				"rows": []map[string]any{
					{"model": "gpt-4o", "tokens": float64(1000)},
				},
				"from":    from,
				"to":      to,
				"groupBy": groupBy,
				"orgID":   orgID,
				"keyID":   keyID,
			}, nil
		},
		ListKeys: func(_ context.Context, orgID, role string) ([]map[string]any, error) {
			return []map[string]any{
				{"id": "key-1", "name": "dev-key", "org_id": orgID, "role": role},
			}, nil
		},
		CreateKey: func(_ context.Context, orgID, userID, name string, expiresIn time.Duration) (map[string]any, error) {
			return map[string]any{
				"id":         "new-key-id",
				"key":        "vl_uk_newkeyplaintext",
				"name":       name,
				"org_id":     orgID,
				"user_id":    userID,
				"expires_in": expiresIn.String(),
			}, nil
		},
		ListDeployments: func(_ context.Context, modelID string) ([]map[string]any, error) {
			return []map[string]any{
				{"id": "dep-1", "model_id": modelID, "name": "primary"},
				{"id": "dep-2", "model_id": modelID, "name": "fallback"},
			}, nil
		},
	}
}

// ---- list_models ------------------------------------------------------------

func TestVoidLLM_ListModels(t *testing.T) {
	t.Parallel()

	s := buildVoidLLMServer(defaultDeps())
	// system_admin context — expects full model list with health data.
	ctx := mcp.WithKeyIdentity(context.Background(), mcp.KeyIdentity{Role: "system_admin"})
	tr := callTool(t, s, ctx, "list_models", nil)

	if tr.IsError {
		t.Fatalf("unexpected error result: %s", tr.Content[0].Text)
	}
	if len(tr.Content) == 0 {
		t.Fatal("empty content")
	}

	text := tr.Content[0].Text
	// Should contain all three model names.
	for _, model := range []string{"gpt-4o", "claude-3", "llama3"} {
		if !strings.Contains(text, model) {
			t.Errorf("output missing model %q\ngot: %s", model, text)
		}
	}

	// Health data should be merged in.
	if !strings.Contains(text, "healthy") {
		t.Errorf("output missing health status\ngot: %s", text)
	}
	if !strings.Contains(text, "unhealthy") {
		t.Errorf("output missing unhealthy status\ngot: %s", text)
	}
}

func TestVoidLLM_ListModels_MemberRole(t *testing.T) {
	t.Parallel()

	s := buildVoidLLMServer(defaultDeps())
	// member context — expects limited list (name + type only) from ListAvailableModels.
	ctx := mcp.WithKeyIdentity(context.Background(), mcp.KeyIdentity{Role: "member"})
	tr := callTool(t, s, ctx, "list_models", nil)

	if tr.IsError {
		t.Fatalf("unexpected error result: %s", tr.Content[0].Text)
	}
	text := tr.Content[0].Text
	// Accessible models should appear.
	for _, model := range []string{"gpt-4o", "llama3"} {
		if !strings.Contains(text, model) {
			t.Errorf("output missing model %q\ngot: %s", model, text)
		}
	}
	// Provider details must not appear — only name and type are returned.
	if strings.Contains(text, "openai") || strings.Contains(text, "anthropic") {
		t.Errorf("provider info must not be included for member role\ngot: %s", text)
	}
}

func TestVoidLLM_ListModels_NilHealth(t *testing.T) {
	t.Parallel()

	deps := defaultDeps()
	deps.GetAllHealth = func() []map[string]any { return nil }

	s := buildVoidLLMServer(deps)
	// org_admin context — uses the full model path; nil health is allowed.
	ctx := mcp.WithKeyIdentity(context.Background(), mcp.KeyIdentity{Role: "org_admin"})
	tr := callTool(t, s, ctx, "list_models", nil)

	if tr.IsError {
		t.Fatalf("unexpected error: %s", tr.Content[0].Text)
	}
	// Models should still appear.
	if !strings.Contains(tr.Content[0].Text, "gpt-4o") {
		t.Errorf("expected gpt-4o in output\ngot: %s", tr.Content[0].Text)
	}
}

func TestVoidLLM_ListModels_Empty(t *testing.T) {
	t.Parallel()

	deps := defaultDeps()
	deps.ListModels = func(_ context.Context) ([]map[string]any, error) {
		return []map[string]any{}, nil
	}
	deps.GetAllHealth = func() []map[string]any { return nil }

	s := buildVoidLLMServer(deps)
	// system_admin context — uses the full model path; empty registry returns [].
	ctx := mcp.WithKeyIdentity(context.Background(), mcp.KeyIdentity{Role: "system_admin"})
	tr := callTool(t, s, ctx, "list_models", nil)

	if tr.IsError {
		t.Fatalf("unexpected error: %s", tr.Content[0].Text)
	}
	// Result should be an empty JSON array.
	if strings.TrimSpace(tr.Content[0].Text) != "[]" {
		t.Errorf("expected empty array, got: %s", tr.Content[0].Text)
	}
}

func TestVoidLLM_ListModels_DepError(t *testing.T) {
	t.Parallel()

	deps := defaultDeps()
	deps.ListModels = func(_ context.Context) ([]map[string]any, error) {
		return nil, errors.New("db unavailable")
	}

	s := buildVoidLLMServer(deps)

	// system_admin context routes through ListModels; dep error → isError=true in ToolResult.
	adminCtx := mcp.WithKeyIdentity(context.Background(), mcp.KeyIdentity{Role: "system_admin"})
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "list_models",
			"arguments": map[string]any{},
		},
	}
	reqBytes, _ := json.Marshal(req)
	raw := s.Handle(adminCtx, reqBytes)

	var resp mcp.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", resp.Error)
	}

	b, _ := json.Marshal(resp.Result)
	var tr mcp.ToolResult
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatalf("decode ToolResult: %v", err)
	}
	if !tr.IsError {
		t.Errorf("IsError = false; expected dep error to be surfaced as tool-level error")
	}
}

func TestVoidLLM_ListModels_AdminSeesStrategy(t *testing.T) {
	t.Parallel()

	deps := defaultDeps()
	deps.ListModels = func(_ context.Context) ([]map[string]any, error) {
		return []map[string]any{
			{
				"name":             "gpt-4o",
				"provider":         "openai",
				"type":             "chat",
				"strategy":         "round-robin",
				"deployment_count": float64(3),
			},
		}, nil
	}
	deps.GetAllHealth = func() []map[string]any { return nil }

	s := buildVoidLLMServer(deps)
	ctx := mcp.WithKeyIdentity(context.Background(), mcp.KeyIdentity{Role: "system_admin"})
	tr := callTool(t, s, ctx, "list_models", nil)

	if tr.IsError {
		t.Fatalf("unexpected error: %s", tr.Content[0].Text)
	}
	text := tr.Content[0].Text
	if !strings.Contains(text, "round-robin") {
		t.Errorf("expected strategy=round-robin in admin output\ngot: %s", text)
	}
	if !strings.Contains(text, "3") {
		t.Errorf("expected deployment_count=3 in admin output\ngot: %s", text)
	}
}

func TestVoidLLM_ListModels_MemberNoStrategy(t *testing.T) {
	t.Parallel()

	deps := defaultDeps()
	// ListAvailableModels returns only name and type — no strategy or
	// deployment_count fields — as the member-facing path requires.
	deps.ListAvailableModels = func(_ context.Context) ([]map[string]any, error) {
		return []map[string]any{
			{"name": "gpt-4o", "type": "chat"},
			{"name": "llama3", "type": "chat"},
		}, nil
	}

	s := buildVoidLLMServer(deps)
	ctx := mcp.WithKeyIdentity(context.Background(), mcp.KeyIdentity{Role: "member"})
	tr := callTool(t, s, ctx, "list_models", nil)

	if tr.IsError {
		t.Fatalf("unexpected error: %s", tr.Content[0].Text)
	}
	text := tr.Content[0].Text
	if strings.Contains(text, "strategy") {
		t.Errorf("strategy must not appear in member output\ngot: %s", text)
	}
	if strings.Contains(text, "deployment_count") {
		t.Errorf("deployment_count must not appear in member output\ngot: %s", text)
	}
	// Accessible model names should still appear.
	if !strings.Contains(text, "gpt-4o") {
		t.Errorf("expected gpt-4o in member output\ngot: %s", text)
	}
}

// ---- get_model_health -------------------------------------------------------

func TestVoidLLM_GetModelHealth_Found(t *testing.T) {
	t.Parallel()

	s := buildVoidLLMServer(defaultDeps())
	tr := callTool(t, s, context.Background(), "get_model_health", map[string]any{"model": "gpt-4o"})

	if tr.IsError {
		t.Fatalf("unexpected error: %s", tr.Content[0].Text)
	}
	if !strings.Contains(tr.Content[0].Text, "healthy") {
		t.Errorf("expected health data in output\ngot: %s", tr.Content[0].Text)
	}
}

func TestVoidLLM_GetModelHealth_NotFound(t *testing.T) {
	t.Parallel()

	s := buildVoidLLMServer(defaultDeps())
	tr := callTool(t, s, context.Background(), "get_model_health", map[string]any{"model": "nonexistent-model"})

	if !tr.IsError {
		t.Errorf("expected IsError=true for unknown model")
	}
	if !strings.Contains(tr.Content[0].Text, "no health data") {
		t.Errorf("expected 'no health data' in error, got: %s", tr.Content[0].Text)
	}
}

func TestVoidLLM_GetModelHealth_MissingParam(t *testing.T) {
	t.Parallel()

	s := buildVoidLLMServer(defaultDeps())
	// Pass empty args object — model field absent.
	tr := callTool(t, s, context.Background(), "get_model_health", map[string]any{})

	if !tr.IsError {
		t.Errorf("expected IsError=true when model param is missing")
	}
	if !strings.Contains(tr.Content[0].Text, "model parameter is required") {
		t.Errorf("expected 'model parameter is required', got: %s", tr.Content[0].Text)
	}
}

func TestVoidLLM_GetModelHealth_EmptyModel(t *testing.T) {
	t.Parallel()

	s := buildVoidLLMServer(defaultDeps())
	tr := callTool(t, s, context.Background(), "get_model_health", map[string]any{"model": ""})

	if !tr.IsError {
		t.Errorf("expected IsError=true for empty model string")
	}
	if !strings.Contains(tr.Content[0].Text, "model parameter is required") {
		t.Errorf("expected 'model parameter is required', got: %s", tr.Content[0].Text)
	}
}

// ---- get_usage --------------------------------------------------------------

func TestVoidLLM_GetUsage(t *testing.T) {
	t.Parallel()

	var capturedFrom, capturedTo, capturedGroupBy, capturedOrgID, capturedKeyID string

	deps := defaultDeps()
	deps.GetUsage = func(_ context.Context, from, to, groupBy, orgID, keyID string) (any, error) {
		capturedFrom = from
		capturedTo = to
		capturedGroupBy = groupBy
		capturedOrgID = orgID
		capturedKeyID = keyID
		return map[string]any{"rows": []any{}}, nil
	}

	s := buildVoidLLMServer(deps)

	identity := mcp.KeyIdentity{
		OrgID: "org-abc",
		KeyID: "key-xyz",
		Role:  "org_admin",
	}
	ctx := mcp.WithKeyIdentity(context.Background(), identity)

	callTool(t, s, ctx, "get_usage", map[string]any{
		"from":     "2026-01-01T00:00:00Z",
		"to":       "2026-01-31T23:59:59Z",
		"group_by": "model",
	})

	if capturedFrom != "2026-01-01T00:00:00Z" {
		t.Errorf("from = %q, want %q", capturedFrom, "2026-01-01T00:00:00Z")
	}
	if capturedTo != "2026-01-31T23:59:59Z" {
		t.Errorf("to = %q, want %q", capturedTo, "2026-01-31T23:59:59Z")
	}
	if capturedGroupBy != "model" {
		t.Errorf("groupBy = %q, want %q", capturedGroupBy, "model")
	}
	if capturedOrgID != "org-abc" {
		t.Errorf("orgID = %q, want %q", capturedOrgID, "org-abc")
	}
	if capturedKeyID != "key-xyz" {
		t.Errorf("keyID = %q, want %q", capturedKeyID, "key-xyz")
	}
}

func TestVoidLLM_GetUsage_NoArgs(t *testing.T) {
	t.Parallel()

	called := false
	deps := defaultDeps()
	deps.GetUsage = func(_ context.Context, from, to, groupBy, orgID, keyID string) (any, error) {
		called = true
		return map[string]any{}, nil
	}

	s := buildVoidLLMServer(deps)

	// nil args map still calls the dep without error.
	tr := callTool(t, s, context.Background(), "get_usage", nil)

	if tr.IsError {
		t.Errorf("unexpected error: %s", tr.Content[0].Text)
	}
	if !called {
		t.Errorf("expected GetUsage to be called")
	}
}

// ---- list_keys --------------------------------------------------------------

func TestVoidLLM_ListKeys(t *testing.T) {
	t.Parallel()

	var capturedOrgID, capturedRole string

	deps := defaultDeps()
	deps.ListKeys = func(_ context.Context, orgID, role string) ([]map[string]any, error) {
		capturedOrgID = orgID
		capturedRole = role
		return []map[string]any{
			{"id": "k1", "name": "production"},
		}, nil
	}

	s := buildVoidLLMServer(deps)

	identity := mcp.KeyIdentity{
		OrgID: "org-test",
		Role:  "org_admin",
	}
	ctx := mcp.WithKeyIdentity(context.Background(), identity)
	tr := callTool(t, s, ctx, "list_keys", nil)

	if tr.IsError {
		t.Fatalf("unexpected error: %s", tr.Content[0].Text)
	}
	if capturedOrgID != "org-test" {
		t.Errorf("orgID passed to ListKeys = %q, want %q", capturedOrgID, "org-test")
	}
	if capturedRole != "org_admin" {
		t.Errorf("role passed to ListKeys = %q, want %q", capturedRole, "org_admin")
	}
	if !strings.Contains(tr.Content[0].Text, "production") {
		t.Errorf("expected key name in output\ngot: %s", tr.Content[0].Text)
	}
}

func TestVoidLLM_ListKeys_NoIdentity(t *testing.T) {
	t.Parallel()

	var capturedOrgID, capturedRole string

	deps := defaultDeps()
	deps.ListKeys = func(_ context.Context, orgID, role string) ([]map[string]any, error) {
		capturedOrgID = orgID
		capturedRole = role
		return []map[string]any{}, nil
	}

	s := buildVoidLLMServer(deps)
	// No identity in context — zero values should be passed, not panic.
	tr := callTool(t, s, context.Background(), "list_keys", nil)

	if tr.IsError {
		t.Fatalf("unexpected error: %s", tr.Content[0].Text)
	}
	if capturedOrgID != "" {
		t.Errorf("expected empty orgID, got %q", capturedOrgID)
	}
	if capturedRole != "" {
		t.Errorf("expected empty role, got %q", capturedRole)
	}
}

// ---- create_key -------------------------------------------------------------

func TestVoidLLM_CreateKey_Success(t *testing.T) {
	t.Parallel()

	var capturedName string
	deps := defaultDeps()
	deps.CreateKey = func(_ context.Context, orgID, userID, name string, expiresIn time.Duration) (map[string]any, error) {
		capturedName = name
		return map[string]any{"id": "new-id", "key": "vl_uk_plaintext", "name": name}, nil
	}

	s := buildVoidLLMServer(deps)
	ctx := mcp.WithKeyIdentity(context.Background(), mcp.KeyIdentity{OrgID: "org-1", UserID: "user-1"})

	tr := callTool(t, s, ctx, "create_key", map[string]any{"name": "ci-key"})

	if tr.IsError {
		t.Fatalf("unexpected error: %s", tr.Content[0].Text)
	}
	if capturedName != "ci-key" {
		t.Errorf("name passed = %q, want %q", capturedName, "ci-key")
	}
	if !strings.Contains(tr.Content[0].Text, "vl_uk_plaintext") {
		t.Errorf("expected plaintext key in output\ngot: %s", tr.Content[0].Text)
	}
}

func TestVoidLLM_CreateKey_MissingName(t *testing.T) {
	t.Parallel()

	s := buildVoidLLMServer(defaultDeps())
	tr := callTool(t, s, context.Background(), "create_key", map[string]any{})

	if !tr.IsError {
		t.Errorf("expected IsError=true when name is missing")
	}
	if !strings.Contains(tr.Content[0].Text, "name parameter is required") {
		t.Errorf("expected 'name parameter is required', got: %s", tr.Content[0].Text)
	}
}

func TestVoidLLM_CreateKey_WithExpiresIn(t *testing.T) {
	t.Parallel()

	var capturedExpiry time.Duration
	deps := defaultDeps()
	deps.CreateKey = func(_ context.Context, _, _, _ string, expiresIn time.Duration) (map[string]any, error) {
		capturedExpiry = expiresIn
		return map[string]any{"id": "x", "key": "vl_uk_x"}, nil
	}

	s := buildVoidLLMServer(deps)
	tr := callTool(t, s, context.Background(), "create_key", map[string]any{
		"name":       "temp-key",
		"expires_in": "24h",
	})

	if tr.IsError {
		t.Fatalf("unexpected error: %s", tr.Content[0].Text)
	}
	if capturedExpiry != 24*time.Hour {
		t.Errorf("expiresIn = %v, want %v", capturedExpiry, 24*time.Hour)
	}
}

func TestVoidLLM_CreateKey_InvalidDuration(t *testing.T) {
	t.Parallel()

	s := buildVoidLLMServer(defaultDeps())
	tr := callTool(t, s, context.Background(), "create_key", map[string]any{
		"name":       "bad-key",
		"expires_in": "7d", // Go's time.ParseDuration does not support 'd'.
	})

	if !tr.IsError {
		t.Errorf("expected IsError=true for invalid duration")
	}
	if !strings.Contains(tr.Content[0].Text, "invalid expires_in") {
		t.Errorf("expected 'invalid expires_in' in error, got: %s", tr.Content[0].Text)
	}
}

func TestVoidLLM_CreateKey_DepError(t *testing.T) {
	t.Parallel()

	deps := defaultDeps()
	deps.CreateKey = func(_ context.Context, _, _, _ string, _ time.Duration) (map[string]any, error) {
		return nil, errors.New("vault unavailable: connection refused")
	}

	s := buildVoidLLMServer(deps)
	ctx := mcp.WithKeyIdentity(context.Background(), mcp.KeyIdentity{OrgID: "org-1", UserID: "user-1"})

	// Use the raw request path so we can check both the protocol layer and
	// the tool result without callTool fataling on a protocol error.
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "create_key",
			"arguments": map[string]any{"name": "ci-key"},
		},
	}
	reqBytes, _ := json.Marshal(req)
	raw := s.Handle(ctx, reqBytes)

	var resp mcp.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", resp.Error)
	}

	b, _ := json.Marshal(resp.Result)
	var tr mcp.ToolResult
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatalf("decode ToolResult: %v", err)
	}
	if !tr.IsError {
		t.Errorf("IsError = false; expected dep error to surface as tool-level error")
	}
	if len(tr.Content) == 0 {
		t.Fatal("Content is empty")
	}
	text := tr.Content[0].Text
	if !strings.Contains(text, "internal error") {
		t.Errorf("expected %q to contain %q", text, "internal error")
	}
	if strings.Contains(text, "vault unavailable") {
		t.Errorf("raw dep error must not be leaked to caller: %q", text)
	}
}

func TestVoidLLM_ListKeys_DepError(t *testing.T) {
	t.Parallel()

	deps := defaultDeps()
	deps.ListKeys = func(_ context.Context, _, _ string) ([]map[string]any, error) {
		return nil, errors.New("db query failed: no such table: api_keys")
	}

	s := buildVoidLLMServer(deps)
	ctx := mcp.WithKeyIdentity(context.Background(), mcp.KeyIdentity{OrgID: "org-1", Role: "org_admin"})

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "list_keys",
			"arguments": map[string]any{},
		},
	}
	reqBytes, _ := json.Marshal(req)
	raw := s.Handle(ctx, reqBytes)

	var resp mcp.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", resp.Error)
	}

	b, _ := json.Marshal(resp.Result)
	var tr mcp.ToolResult
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatalf("decode ToolResult: %v", err)
	}
	if !tr.IsError {
		t.Errorf("IsError = false; expected dep error to surface as tool-level error")
	}
	if len(tr.Content) == 0 {
		t.Fatal("Content is empty")
	}
	text := tr.Content[0].Text
	if !strings.Contains(text, "internal error") {
		t.Errorf("expected %q to contain %q", text, "internal error")
	}
	if strings.Contains(text, "no such table") {
		t.Errorf("raw dep error must not be leaked to caller: %q", text)
	}
}

// ---- list_deployments -------------------------------------------------------

func TestVoidLLM_ListDeployments_Success(t *testing.T) {
	t.Parallel()

	var capturedModelID string
	deps := defaultDeps()
	deps.ListDeployments = func(_ context.Context, modelID string) ([]map[string]any, error) {
		capturedModelID = modelID
		return []map[string]any{
			{"id": "dep-1", "name": "primary", "model_id": modelID},
		}, nil
	}

	s := buildVoidLLMServer(deps)
	ctx := mcp.WithKeyIdentity(context.Background(), mcp.KeyIdentity{Role: "system_admin"})

	tr := callTool(t, s, ctx, "list_deployments", map[string]any{"model_id": "model-uuid-123"})

	if tr.IsError {
		t.Fatalf("unexpected error: %s", tr.Content[0].Text)
	}
	if capturedModelID != "model-uuid-123" {
		t.Errorf("model_id passed = %q, want %q", capturedModelID, "model-uuid-123")
	}
	if !strings.Contains(tr.Content[0].Text, "primary") {
		t.Errorf("expected deployment name in output\ngot: %s", tr.Content[0].Text)
	}
}

func TestVoidLLM_ListDeployments_NotAdmin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		role string
	}{
		{"member role", "member"},
		{"org_admin role", "org_admin"},
		{"team_admin role", "team_admin"},
		{"empty role", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := buildVoidLLMServer(defaultDeps())
			ctx := mcp.WithKeyIdentity(context.Background(), mcp.KeyIdentity{Role: tc.role})

			tr := callTool(t, s, ctx, "list_deployments", map[string]any{"model_id": "any-model-id"})

			if !tr.IsError {
				t.Errorf("expected IsError=true for role %q", tc.role)
			}
			if !strings.Contains(tr.Content[0].Text, "system_admin role required") {
				t.Errorf("expected 'system_admin role required', got: %s", tr.Content[0].Text)
			}
		})
	}
}

func TestVoidLLM_ListDeployments_MissingModelID(t *testing.T) {
	t.Parallel()

	s := buildVoidLLMServer(defaultDeps())
	ctx := mcp.WithKeyIdentity(context.Background(), mcp.KeyIdentity{Role: "system_admin"})

	tr := callTool(t, s, ctx, "list_deployments", map[string]any{})

	if !tr.IsError {
		t.Errorf("expected IsError=true when model_id is missing")
	}
	if !strings.Contains(tr.Content[0].Text, "model_id parameter is required") {
		t.Errorf("expected 'model_id parameter is required', got: %s", tr.Content[0].Text)
	}
}

// ---- KeyIdentity context round-trip -----------------------------------------

// TestKeyIdentityFromCtx_Exported verifies the exported KeyIdentityFromCtx
// wrapper returns the same value as WithKeyIdentity stored.
func TestKeyIdentityFromCtx_Exported(t *testing.T) {
	t.Parallel()

	want := mcp.KeyIdentity{
		OrgID:  "org-exported",
		TeamID: "team-e",
		KeyID:  "key-e",
		UserID: "user-e",
		Role:   "org_admin",
	}
	ctx := mcp.WithKeyIdentity(context.Background(), want)
	got := mcp.KeyIdentityFromCtx(ctx)

	if got != want {
		t.Errorf("KeyIdentityFromCtx() = %+v, want %+v", got, want)
	}
}

func TestKeyIdentityFromCtx_Empty(t *testing.T) {
	t.Parallel()

	got := mcp.KeyIdentityFromCtx(context.Background())
	if got != (mcp.KeyIdentity{}) {
		t.Errorf("KeyIdentityFromCtx(empty ctx) = %+v, want zero value", got)
	}
}

func TestKeyIdentity_ContextRoundTrip(t *testing.T) {
	t.Parallel()

	want := mcp.KeyIdentity{
		OrgID:  "org-roundtrip",
		KeyID:  "key-rt",
		UserID: "user-rt",
		Role:   "org_admin",
	}

	// Verify the round-trip by having a tool read the identity.
	var got mcp.KeyIdentity
	s := mcp.NewServer("rt", "0.1.0")
	s.RegisterTool(mcp.Tool{
		Name:        "read_identity",
		InputSchema: mcp.InputSchema{Type: "object"},
	}, func(ctx context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		// We cannot call the private keyIdentityFromCtx, but we can confirm
		// that a tool receiving a context from WithKeyIdentity sees the values
		// by proxying them through get_usage-style logic in the test.
		// Instead we use the exported WithKeyIdentity and verify via list_keys dep.
		return mcp.TextResult("ok"), nil
	})

	// Register list_keys to capture the identity.
	deps := defaultDeps()
	deps.ListKeys = func(_ context.Context, orgID, role string) ([]map[string]any, error) {
		got.OrgID = orgID
		got.Role = role
		return nil, nil
	}
	deps.GetUsage = func(_ context.Context, _, _, _, orgID, keyID string) (any, error) {
		got.KeyID = keyID
		return map[string]any{}, nil
	}

	s2 := buildVoidLLMServer(deps)
	ctx := mcp.WithKeyIdentity(context.Background(), want)

	callTool(t, s2, ctx, "list_keys", nil)
	callTool(t, s2, ctx, "get_usage", nil)

	if got.OrgID != want.OrgID {
		t.Errorf("OrgID = %q, want %q", got.OrgID, want.OrgID)
	}
	if got.Role != want.Role {
		t.Errorf("Role = %q, want %q", got.Role, want.Role)
	}
	if got.KeyID != want.KeyID {
		t.Errorf("KeyID = %q, want %q", got.KeyID, want.KeyID)
	}
}

func TestKeyIdentity_MissingFromContext(t *testing.T) {
	t.Parallel()

	// When no identity is in the context the tools receive zero-value fields
	// (empty strings). No panic should occur.
	var capturedOrgID string

	deps := defaultDeps()
	deps.ListKeys = func(_ context.Context, orgID, _ string) ([]map[string]any, error) {
		capturedOrgID = orgID
		return []map[string]any{}, nil
	}

	s := buildVoidLLMServer(deps)
	tr := callTool(t, s, context.Background(), "list_keys", nil)

	if tr.IsError {
		t.Fatalf("unexpected error: %s", tr.Content[0].Text)
	}
	if capturedOrgID != "" {
		t.Errorf("expected empty OrgID from zero-value identity, got %q", capturedOrgID)
	}
}

// ---- RegisterVoidLLMTools — all tools appear in list -----------------------

func TestRegisterVoidLLMTools_AllToolsListed(t *testing.T) {
	t.Parallel()

	s := buildVoidLLMServer(defaultDeps())

	resp := callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	assertNoError(t, resp)

	m := resultMap(t, resp)
	tools, _ := m["tools"].([]any)

	wantTools := []string{
		"list_models",
		"get_model_health",
		"get_usage",
		"list_keys",
		"create_key",
		"list_deployments",
	}

	names := make(map[string]bool, len(tools))
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		names[name] = true
	}

	for _, want := range wantTools {
		if !names[want] {
			t.Errorf("tool %q not found in tools/list", want)
		}
	}

	if len(tools) != len(wantTools) {
		t.Errorf("tools/list length = %d, want %d", len(tools), len(wantTools))
	}
}

// ---- Code Mode tools not registered when ExecuteCode is nil -----------------

func TestRegisterVoidLLMTools_CodeModeAbsent_WhenExecuteCodeNil(t *testing.T) {
	t.Parallel()

	// Build deps with all Code Mode fields nil.
	deps := defaultDeps()
	deps.ExecuteCode = nil
	deps.ListAccessibleMCPServers = nil
	deps.SearchMCPTools = nil

	s := buildVoidLLMServer(deps)

	resp := callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	assertNoError(t, resp)

	m := resultMap(t, resp)
	tools, _ := m["tools"].([]any)

	names := make(map[string]bool, len(tools))
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		names[name] = true
	}

	codeModeTools := []string{"list_servers", "search_tools", "execute_code"}
	for _, cm := range codeModeTools {
		if names[cm] {
			t.Errorf("tool %q should not be registered when ExecuteCode is nil", cm)
		}
	}
}

// ---- list_servers -----------------------------------------------------------

func TestVoidLLM_ListServers_Success(t *testing.T) {
	t.Parallel()

	servers := []map[string]any{
		{"alias": "weather", "name": "Weather MCP", "tool_count": float64(5)},
		{"alias": "calendar", "name": "Calendar MCP", "tool_count": float64(3)},
	}

	var capturedCodeModeOnly bool
	deps := defaultDeps()
	deps.ListAccessibleMCPServers = func(_ context.Context, codeModeOnly bool) ([]map[string]any, error) {
		capturedCodeModeOnly = codeModeOnly
		return servers, nil
	}
	deps.ExecuteCode = func(_ context.Context, _ string, _ []string) (*mcp.ExecuteResult, error) {
		return &mcp.ExecuteResult{Result: json.RawMessage(`"ok"`)}, nil
	}

	s := buildVoidLLMServer(deps)
	tr := callTool(t, s, context.Background(), "list_servers", nil)

	if tr.IsError {
		t.Fatalf("unexpected error: %s", tr.Content[0].Text)
	}
	if !capturedCodeModeOnly {
		t.Errorf("ListAccessibleMCPServers called with codeModeOnly=false, want true")
	}
	text := tr.Content[0].Text
	for _, srv := range []string{"weather", "calendar"} {
		if !strings.Contains(text, srv) {
			t.Errorf("output missing server %q\ngot: %s", srv, text)
		}
	}
}

func TestVoidLLM_ListServers_Empty(t *testing.T) {
	t.Parallel()

	deps := defaultDeps()
	deps.ListAccessibleMCPServers = func(_ context.Context, _ bool) ([]map[string]any, error) {
		return []map[string]any{}, nil
	}
	deps.ExecuteCode = func(_ context.Context, _ string, _ []string) (*mcp.ExecuteResult, error) {
		return &mcp.ExecuteResult{Result: json.RawMessage(`"ok"`)}, nil
	}

	s := buildVoidLLMServer(deps)
	tr := callTool(t, s, context.Background(), "list_servers", nil)

	if tr.IsError {
		t.Fatalf("unexpected error: %s", tr.Content[0].Text)
	}
	if strings.TrimSpace(tr.Content[0].Text) != "[]" {
		t.Errorf("expected [], got: %s", tr.Content[0].Text)
	}
}

func TestVoidLLM_ListServers_DepError(t *testing.T) {
	t.Parallel()

	deps := defaultDeps()
	deps.ListAccessibleMCPServers = func(_ context.Context, _ bool) ([]map[string]any, error) {
		return nil, errors.New("db error")
	}
	deps.ExecuteCode = func(_ context.Context, _ string, _ []string) (*mcp.ExecuteResult, error) {
		return &mcp.ExecuteResult{Result: json.RawMessage(`"ok"`)}, nil
	}

	s := buildVoidLLMServer(deps)

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "list_servers",
			"arguments": map[string]any{},
		},
	}
	reqBytes, _ := json.Marshal(req)
	raw := s.Handle(context.Background(), reqBytes)

	var resp mcp.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", resp.Error)
	}

	b, _ := json.Marshal(resp.Result)
	var tr mcp.ToolResult
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatalf("decode ToolResult: %v", err)
	}
	if !tr.IsError {
		t.Errorf("IsError = false, want true for dep error")
	}
}

// ---- search_tools -----------------------------------------------------------

func TestVoidLLM_SearchTools_Success(t *testing.T) {
	t.Parallel()

	var capturedQuery string
	var capturedAliases []string

	tools := []map[string]any{
		{"name": "get_weather", "description": "Get current weather", "server": "weather"},
	}

	deps := defaultDeps()
	deps.SearchMCPTools = func(_ context.Context, query string, serverAliases []string) ([]map[string]any, error) {
		capturedQuery = query
		capturedAliases = serverAliases
		return tools, nil
	}
	deps.ExecuteCode = func(_ context.Context, _ string, _ []string) (*mcp.ExecuteResult, error) {
		return &mcp.ExecuteResult{Result: json.RawMessage(`"ok"`)}, nil
	}

	s := buildVoidLLMServer(deps)
	tr := callTool(t, s, context.Background(), "search_tools", map[string]any{
		"query": "weather",
	})

	if tr.IsError {
		t.Fatalf("unexpected error: %s", tr.Content[0].Text)
	}
	if capturedQuery != "weather" {
		t.Errorf("query = %q, want %q", capturedQuery, "weather")
	}
	if len(capturedAliases) != 0 {
		t.Errorf("serverAliases = %v, want nil (no server filter)", capturedAliases)
	}
	if !strings.Contains(tr.Content[0].Text, "get_weather") {
		t.Errorf("output missing 'get_weather'\ngot: %s", tr.Content[0].Text)
	}
}

func TestVoidLLM_SearchTools_WithServerFilter(t *testing.T) {
	t.Parallel()

	var capturedAliases []string

	deps := defaultDeps()
	deps.SearchMCPTools = func(_ context.Context, _ string, serverAliases []string) ([]map[string]any, error) {
		capturedAliases = serverAliases
		return []map[string]any{}, nil
	}
	deps.ExecuteCode = func(_ context.Context, _ string, _ []string) (*mcp.ExecuteResult, error) {
		return &mcp.ExecuteResult{Result: json.RawMessage(`"ok"`)}, nil
	}

	s := buildVoidLLMServer(deps)
	callTool(t, s, context.Background(), "search_tools", map[string]any{
		"query":  "calendar",
		"server": "cal-server",
	})

	if len(capturedAliases) != 1 || capturedAliases[0] != "cal-server" {
		t.Errorf("serverAliases = %v, want [\"cal-server\"]", capturedAliases)
	}
}

func TestVoidLLM_SearchTools_MissingQuery(t *testing.T) {
	t.Parallel()

	deps := defaultDeps()
	deps.SearchMCPTools = func(_ context.Context, _ string, _ []string) ([]map[string]any, error) {
		return nil, nil
	}
	deps.ExecuteCode = func(_ context.Context, _ string, _ []string) (*mcp.ExecuteResult, error) {
		return &mcp.ExecuteResult{Result: json.RawMessage(`"ok"`)}, nil
	}

	s := buildVoidLLMServer(deps)
	tr := callTool(t, s, context.Background(), "search_tools", map[string]any{})

	if !tr.IsError {
		t.Errorf("expected IsError=true when query is missing")
	}
	if !strings.Contains(tr.Content[0].Text, "query parameter is required") {
		t.Errorf("expected 'query parameter is required', got: %s", tr.Content[0].Text)
	}
}

func TestVoidLLM_SearchTools_DepError(t *testing.T) {
	t.Parallel()

	deps := defaultDeps()
	deps.SearchMCPTools = func(_ context.Context, _ string, _ []string) ([]map[string]any, error) {
		return nil, errors.New("search index unavailable")
	}
	deps.ExecuteCode = func(_ context.Context, _ string, _ []string) (*mcp.ExecuteResult, error) {
		return &mcp.ExecuteResult{Result: json.RawMessage(`"ok"`)}, nil
	}

	s := buildVoidLLMServer(deps)

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "search_tools",
			"arguments": map[string]any{"query": "anything"},
		},
	}
	reqBytes, _ := json.Marshal(req)
	raw := s.Handle(context.Background(), reqBytes)

	var resp mcp.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b, _ := json.Marshal(resp.Result)
	var tr mcp.ToolResult
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatalf("decode ToolResult: %v", err)
	}
	if !tr.IsError {
		t.Errorf("IsError = false, want true for dep error")
	}
	if strings.Contains(tr.Content[0].Text, "search index") {
		t.Errorf("raw dep error must not be leaked: %q", tr.Content[0].Text)
	}
}

// ---- execute_code -----------------------------------------------------------

func TestVoidLLM_ExecuteCode_Success(t *testing.T) {
	t.Parallel()

	var capturedCode string
	var capturedServers []string

	deps := defaultDeps()
	deps.ExecuteCode = func(_ context.Context, code string, serverAliases []string) (*mcp.ExecuteResult, error) {
		capturedCode = code
		capturedServers = serverAliases
		return &mcp.ExecuteResult{
			Result:     json.RawMessage(`{"answer":42}`),
			ToolCalls:  []mcp.ToolCallLog{},
			DurationMS: 7,
		}, nil
	}

	s := buildVoidLLMServer(deps)
	tr := callTool(t, s, context.Background(), "execute_code", map[string]any{
		"code":    "const x = 42; JSON.stringify({answer: x})",
		"servers": []string{"weather", "calendar"},
	})

	if tr.IsError {
		t.Fatalf("unexpected error: %s", tr.Content[0].Text)
	}
	if capturedCode != "const x = 42; JSON.stringify({answer: x})" {
		t.Errorf("code = %q, want the JS source", capturedCode)
	}
	if len(capturedServers) != 2 {
		t.Errorf("serverAliases = %v, want [weather calendar]", capturedServers)
	}
	if !strings.Contains(tr.Content[0].Text, "42") {
		t.Errorf("output missing result value\ngot: %s", tr.Content[0].Text)
	}
}

func TestVoidLLM_ExecuteCode_NoServers(t *testing.T) {
	t.Parallel()

	var capturedServers []string

	deps := defaultDeps()
	deps.ExecuteCode = func(_ context.Context, _ string, serverAliases []string) (*mcp.ExecuteResult, error) {
		capturedServers = serverAliases
		return &mcp.ExecuteResult{
			Result:    json.RawMessage(`"done"`),
			ToolCalls: []mcp.ToolCallLog{},
		}, nil
	}

	s := buildVoidLLMServer(deps)
	callTool(t, s, context.Background(), "execute_code", map[string]any{
		"code": "1 + 1",
	})

	if capturedServers != nil {
		t.Errorf("serverAliases = %v, want nil when servers field absent", capturedServers)
	}
}

func TestVoidLLM_ExecuteCode_MissingCode(t *testing.T) {
	t.Parallel()

	deps := defaultDeps()
	deps.ExecuteCode = func(_ context.Context, _ string, _ []string) (*mcp.ExecuteResult, error) {
		return &mcp.ExecuteResult{}, nil
	}

	s := buildVoidLLMServer(deps)
	tr := callTool(t, s, context.Background(), "execute_code", map[string]any{})

	if !tr.IsError {
		t.Errorf("expected IsError=true when code is missing")
	}
	if !strings.Contains(tr.Content[0].Text, "code parameter is required") {
		t.Errorf("expected 'code parameter is required', got: %s", tr.Content[0].Text)
	}
}

func TestVoidLLM_ExecuteCode_ScriptError(t *testing.T) {
	t.Parallel()

	deps := defaultDeps()
	deps.ExecuteCode = func(_ context.Context, _ string, _ []string) (*mcp.ExecuteResult, error) {
		return &mcp.ExecuteResult{
			Error:     "SyntaxError: unexpected token",
			ToolCalls: []mcp.ToolCallLog{},
		}, nil
	}

	s := buildVoidLLMServer(deps)
	tr := callTool(t, s, context.Background(), "execute_code", map[string]any{
		"code": "invalid @@ syntax",
	})

	if !tr.IsError {
		t.Errorf("expected IsError=true when script returns an error")
	}
	if !strings.Contains(tr.Content[0].Text, "SyntaxError") {
		t.Errorf("expected script error message, got: %s", tr.Content[0].Text)
	}
}

func TestVoidLLM_ExecuteCode_DepError(t *testing.T) {
	t.Parallel()

	deps := defaultDeps()
	deps.ExecuteCode = func(_ context.Context, _ string, _ []string) (*mcp.ExecuteResult, error) {
		return nil, errors.New("sandbox crashed")
	}

	s := buildVoidLLMServer(deps)

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "execute_code",
			"arguments": map[string]any{"code": "1+1"},
		},
	}
	reqBytes, _ := json.Marshal(req)
	raw := s.Handle(context.Background(), reqBytes)

	var resp mcp.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b, _ := json.Marshal(resp.Result)
	var tr mcp.ToolResult
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatalf("decode ToolResult: %v", err)
	}
	if !tr.IsError {
		t.Errorf("IsError = false, want true for dep error")
	}
	if strings.Contains(tr.Content[0].Text, "sandbox crashed") {
		t.Errorf("raw dep error must not be leaked: %q", tr.Content[0].Text)
	}
}

// ---- Code Mode tools appear in tools/list when registered -------------------

func TestRegisterVoidLLMTools_CodeModeToolsListed(t *testing.T) {
	t.Parallel()

	deps := defaultDeps()
	deps.ExecuteCode = func(_ context.Context, _ string, _ []string) (*mcp.ExecuteResult, error) {
		return &mcp.ExecuteResult{Result: json.RawMessage(`"ok"`)}, nil
	}
	deps.ListAccessibleMCPServers = func(_ context.Context, _ bool) ([]map[string]any, error) {
		return nil, nil
	}
	deps.SearchMCPTools = func(_ context.Context, _ string, _ []string) ([]map[string]any, error) {
		return nil, nil
	}

	s := buildVoidLLMServer(deps)

	resp := callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	assertNoError(t, resp)

	m := resultMap(t, resp)
	tools, _ := m["tools"].([]any)

	names := make(map[string]bool, len(tools))
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		names[name] = true
	}

	for _, want := range []string{"list_servers", "search_tools", "execute_code"} {
		if !names[want] {
			t.Errorf("Code Mode tool %q not found in tools/list", want)
		}
	}
}
