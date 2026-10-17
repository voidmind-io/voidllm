package admin_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

const (
	bedrockTestModelID = "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"
	bedrockTestRegion  = "eu-central-1"
	bedrockDerivedURL  = "https://bedrock-runtime.eu-central-1.amazonaws.com"
)

type bedrockEnv struct {
	app      *fiber.App
	db       *db.DB
	registry *proxy.Registry
	key      string
}

func newBedrockEnv(t *testing.T) *bedrockEnv {
	t.Helper()
	ctx := context.Background()
	safe := strings.NewReplacer("/", "_", " ", "_", "#", "_").Replace(t.Name())
	database, err := db.Open(ctx, config.DatabaseConfig{
		Driver: "sqlite", DSN: "file:" + safe + "?mode=memory&cache=private",
		MaxOpenConns: 1, MaxIdleConns: 1, ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	registry, err := proxy.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	keyCache := cache.New[string, auth.KeyInfo]()
	h := &admin.Handler{
		DB: database, HMACSecret: testHMACSecret, EncryptionKey: testEncryptionKey, Registry: registry,
		KeyCache: keyCache, License: license.NewHolder(license.Verify("", true)),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	app := fiber.New()
	admin.RegisterRoutes(app, h, keyCache, testHMACSecret, nil)
	return &bedrockEnv{app: app, db: database, registry: registry, key: addTestKey(t, keyCache, auth.RoleSystemAdmin, "")}
}

// call performs an authenticated JSON request and returns status and decoded body.
func (e *bedrockEnv) call(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bodyJSON(t, body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.key)
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if resp.StatusCode != http.StatusNoContent {
		decodeBody(t, resp.Body, &out)
	}
	return resp.StatusCode, out
}

func errMessage(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

func bedrockModelBody(over map[string]any) map[string]any {
	b := map[string]any{
		"name": "claude", "provider": "bedrock", "region": bedrockTestRegion, "bedrock_model_id": bedrockTestModelID,
	}
	for k, v := range over {
		if v == nil {
			delete(b, k)
		} else {
			b[k] = v
		}
	}
	return b
}

func TestCreateModel_Bedrock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       map[string]any
		wantStatus int
		wantErr    string
		check      func(t *testing.T, e *bedrockEnv, got map[string]any)
	}{
		{
			name:       "no base_url derives regional endpoint in registry",
			body:       bedrockModelBody(nil),
			wantStatus: 201,
			check: func(t *testing.T, e *bedrockEnv, got map[string]any) {
				if got["region"] != bedrockTestRegion || got["bedrock_model_id"] != bedrockTestModelID {
					t.Errorf("response = %v", got)
				}
				if v, ok := got["base_url"]; ok && v != "" {
					t.Errorf("base_url = %v, want empty (derived, never stored)", v)
				}
				m, err := e.registry.Resolve("claude")
				if err != nil {
					t.Fatalf("model not in registry: %v", err)
				}
				if m.BaseURL != bedrockDerivedURL || m.Region != bedrockTestRegion || m.BedrockModelID != bedrockTestModelID || m.Provider != "bedrock" {
					t.Errorf("registry model = %+v", m)
				}
				stored, err := e.db.GetModelByName(context.Background(), "claude")
				if err != nil {
					t.Fatal(err)
				}
				if stored.BaseURL != "" || stored.Region != bedrockTestRegion || stored.BedrockModelID != bedrockTestModelID {
					t.Errorf("stored = base %q region %q id %q", stored.BaseURL, stored.Region, stored.BedrockModelID)
				}
			},
		},
		{
			name:       "explicit base_url kept",
			body:       bedrockModelBody(map[string]any{"base_url": "https://bedrock-runtime-fips.eu-central-1.amazonaws.com"}),
			wantStatus: 201,
			check: func(t *testing.T, e *bedrockEnv, got map[string]any) {
				if got["base_url"] != "https://bedrock-runtime-fips.eu-central-1.amazonaws.com" {
					t.Errorf("base_url = %v", got["base_url"])
				}
				m, _ := e.registry.Resolve("claude")
				if m.BaseURL != "https://bedrock-runtime-fips.eu-central-1.amazonaws.com" {
					t.Errorf("registry BaseURL = %q", m.BaseURL)
				}
			},
		},
		{
			name:       "api key is encrypted and not returned",
			body:       bedrockModelBody(map[string]any{"api_key": "ABSK-secret-bearer"}),
			wantStatus: 201,
			check: func(t *testing.T, e *bedrockEnv, got map[string]any) {
				for k, v := range got {
					if s, ok := v.(string); ok && strings.Contains(s, "ABSK-secret-bearer") {
						t.Errorf("api key leaked in field %q", k)
					}
				}
				m, _ := e.registry.Resolve("claude")
				if m.APIKey != "ABSK-secret-bearer" {
					t.Errorf("registry did not receive the decrypted key")
				}
			},
		},
		{
			name:       "type chat accepted",
			body:       bedrockModelBody(map[string]any{"type": "chat"}),
			wantStatus: 201,
		},
		{name: "region missing", body: bedrockModelBody(map[string]any{"region": nil}), wantStatus: 400, wantErr: "region is required for bedrock"},
		{name: "region empty", body: bedrockModelBody(map[string]any{"region": ""}), wantStatus: 400, wantErr: "region is required for bedrock"},
		{name: "region injection", body: bedrockModelBody(map[string]any{"region": "eu-central-1.evil.com"}), wantStatus: 400, wantErr: "region is not a valid AWS region"},
		{name: "region uppercase", body: bedrockModelBody(map[string]any{"region": "EU-CENTRAL-1"}), wantStatus: 400, wantErr: "region is not a valid AWS region"},
		{name: "model id missing", body: bedrockModelBody(map[string]any{"bedrock_model_id": nil}), wantStatus: 400, wantErr: "bedrock_model_id is required for bedrock"},
		{name: "model id invalid", body: bedrockModelBody(map[string]any{"bedrock_model_id": "a b/../?"}), wantStatus: 400, wantErr: "bedrock_model_id is invalid"},
		{name: "model id too long", body: bedrockModelBody(map[string]any{"bedrock_model_id": strings.Repeat("a", 2049)}), wantStatus: 400, wantErr: "bedrock_model_id is invalid"},
		{name: "embedding type rejected", body: bedrockModelBody(map[string]any{"type": "embedding"}), wantStatus: 400, wantErr: "bedrock supports only chat models"},
		{name: "image type rejected", body: bedrockModelBody(map[string]any{"type": "image"}), wantStatus: 400, wantErr: "bedrock supports only chat models"},
		{name: "base_url bad scheme", body: bedrockModelBody(map[string]any{"base_url": "ftp://x"}), wantStatus: 400, wantErr: "base_url must begin with http:// or https://"},
		{
			name:       "openai still requires base_url",
			body:       map[string]any{"name": "gpt", "provider": "openai"},
			wantStatus: 400, wantErr: "base_url is required",
		},
		{
			name:       "bedrock fields on other provider accepted but unused",
			body:       map[string]any{"name": "gpt", "provider": "openai", "base_url": "https://api.openai.com/v1", "region": "whatever"},
			wantStatus: 201,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newBedrockEnv(t)
			status, out := e.call(t, "POST", modelURL(), tc.body)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body %v", status, tc.wantStatus, out)
			}
			if tc.wantErr != "" && errMessage(out) != tc.wantErr {
				t.Errorf("error = %q, want %q", errMessage(out), tc.wantErr)
			}
			if tc.wantStatus >= 400 {
				if list, err := e.db.ListModels(context.Background(), "", 10, true); err != nil || len(list) != 0 {
					t.Errorf("rejected request persisted a model: %v %v", list, err)
				}
			}
			if tc.check != nil {
				tc.check(t, e, out)
			}
		})
	}
}

func TestUpdateModel_Bedrock(t *testing.T) {
	t.Parallel()

	const openaiURL = "https://api.openai.com/v1"
	tests := []struct {
		name        string
		create      map[string]any
		update      map[string]any
		wantStatus  int
		wantErr     string
		wantBaseURL string // registry base URL when 200
		check       func(t *testing.T, out map[string]any)
	}{
		{
			name: "change region only", create: bedrockModelBody(nil), update: map[string]any{"region": "us-east-1"},
			wantStatus: 200, wantBaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com",
			check: func(t *testing.T, out map[string]any) {
				if out["region"] != "us-east-1" || out["bedrock_model_id"] != bedrockTestModelID {
					t.Errorf("response = %v", out)
				}
			},
		},
		{
			name: "change model id only", create: bedrockModelBody(nil), update: map[string]any{"bedrock_model_id": "us.anthropic.claude-3-haiku-20240307-v1:0"},
			wantStatus: 200, wantBaseURL: bedrockDerivedURL,
			check: func(t *testing.T, out map[string]any) {
				if out["region"] != bedrockTestRegion || out["bedrock_model_id"] != "us.anthropic.claude-3-haiku-20240307-v1:0" {
					t.Errorf("response = %v", out)
				}
			},
		},
		{
			name: "unrelated update keeps bedrock fields", create: bedrockModelBody(nil), update: map[string]any{"timeout": "45s"},
			wantStatus: 200, wantBaseURL: bedrockDerivedURL,
			check: func(t *testing.T, out map[string]any) {
				if out["region"] != bedrockTestRegion || out["bedrock_model_id"] != bedrockTestModelID {
					t.Errorf("response = %v", out)
				}
			},
		},
		{
			name: "set base_url override", create: bedrockModelBody(nil), update: map[string]any{"base_url": "https://vpce.example.com"},
			wantStatus: 200, wantBaseURL: "https://vpce.example.com",
		},
		{
			name:   "clear base_url on bedrock returns to regional endpoint",
			create: bedrockModelBody(map[string]any{"base_url": "https://vpce.example.com"}),
			update: map[string]any{"base_url": ""}, wantStatus: 200, wantBaseURL: bedrockDerivedURL,
		},
		{
			name: "clear base_url on openai rejected", create: map[string]any{"name": "claude", "provider": "openai", "base_url": openaiURL},
			update: map[string]any{"base_url": ""}, wantStatus: 400, wantErr: "base_url must not be empty in single-endpoint mode",
		},
		{
			name: "invalid region rejected", create: bedrockModelBody(nil), update: map[string]any{"region": "eu-central-1.evil.com"},
			wantStatus: 400, wantErr: "region is not a valid AWS region",
		},
		{
			name: "clearing region rejected", create: bedrockModelBody(nil), update: map[string]any{"region": ""},
			wantStatus: 400, wantErr: "region is required for bedrock",
		},
		{
			name: "clearing model id rejected", create: bedrockModelBody(nil), update: map[string]any{"bedrock_model_id": ""},
			wantStatus: 400, wantErr: "bedrock_model_id is required for bedrock",
		},
		{
			name: "invalid model id rejected", create: bedrockModelBody(nil), update: map[string]any{"bedrock_model_id": "bad id"},
			wantStatus: 400, wantErr: "bedrock_model_id is invalid",
		},
		{
			name: "non-chat type rejected", create: bedrockModelBody(nil), update: map[string]any{"type": "embedding"},
			wantStatus: 400, wantErr: "bedrock supports only chat models",
		},
		{
			name: "bad base_url scheme rejected", create: bedrockModelBody(nil), update: map[string]any{"base_url": "gopher://x"},
			wantStatus: 400, wantErr: "base_url must begin with http:// or https://",
		},
		{
			name:   "switch openai to bedrock without region rejected",
			create: map[string]any{"name": "claude", "provider": "openai", "base_url": openaiURL},
			update: map[string]any{"provider": "bedrock"}, wantStatus: 400, wantErr: "region is required for bedrock",
		},
		{
			name:       "switch openai to bedrock with fields accepted",
			create:     map[string]any{"name": "claude", "provider": "openai", "base_url": openaiURL},
			update:     map[string]any{"provider": "bedrock", "region": bedrockTestRegion, "bedrock_model_id": bedrockTestModelID, "base_url": ""},
			wantStatus: 200, wantBaseURL: bedrockDerivedURL,
		},
		{
			name:       "switch embedding openai to bedrock rejected",
			create:     map[string]any{"name": "claude", "provider": "openai", "base_url": openaiURL, "type": "embedding"},
			update:     map[string]any{"provider": "bedrock", "region": bedrockTestRegion, "bedrock_model_id": bedrockTestModelID},
			wantStatus: 400, wantErr: "bedrock supports only chat models",
		},
		{
			name:   "switch bedrock to openai with empty base_url rejected",
			create: bedrockModelBody(nil), update: map[string]any{"provider": "openai"},
			wantStatus: 400, wantErr: "base_url is required",
		},
		{
			name:   "switch bedrock to openai with base_url accepted",
			create: bedrockModelBody(nil), update: map[string]any{"provider": "openai", "base_url": openaiURL},
			wantStatus: 200, wantBaseURL: openaiURL,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newBedrockEnv(t)
			status, created := e.call(t, "POST", modelURL(), tc.create)
			if status != 201 {
				t.Fatalf("setup create: %d %v", status, created)
			}
			id := created["id"].(string)
			status, out := e.call(t, "PATCH", modelItemURL(id), tc.update)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body %v", status, tc.wantStatus, out)
			}
			if tc.wantErr != "" && errMessage(out) != tc.wantErr {
				t.Errorf("error = %q, want %q", errMessage(out), tc.wantErr)
			}
			if tc.wantStatus == 200 {
				m, err := e.registry.Resolve("claude")
				if err != nil {
					t.Fatalf("registry: %v", err)
				}
				if m.BaseURL != tc.wantBaseURL {
					t.Errorf("registry BaseURL = %q, want %q", m.BaseURL, tc.wantBaseURL)
				}
			} else {
				// A rejected update must leave the stored model untouched.
				stored, err := e.db.GetModel(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Provider != created["provider"] {
					t.Errorf("rejected update modified model: %+v", stored)
				}
			}
			if tc.check != nil {
				tc.check(t, out)
			}
		})
	}
}

func TestGetModel_BedrockFieldsInResponses(t *testing.T) {
	t.Parallel()
	e := newBedrockEnv(t)
	status, created := e.call(t, "POST", modelURL(), bedrockModelBody(nil))
	if status != 201 {
		t.Fatalf("create: %d %v", status, created)
	}
	status, got := e.call(t, "GET", modelItemURL(created["id"].(string)), nil)
	if status != 200 || got["region"] != bedrockTestRegion || got["bedrock_model_id"] != bedrockTestModelID {
		t.Errorf("GET = %d %v", status, got)
	}
	status, list := e.call(t, "GET", modelURL(), nil)
	if status != 200 {
		t.Fatalf("list: %d", status)
	}
	items := list["data"].([]any)
	first := items[0].(map[string]any)
	if first["region"] != bedrockTestRegion || first["bedrock_model_id"] != bedrockTestModelID {
		t.Errorf("list item = %v", first)
	}

	// Non-bedrock models omit the fields entirely.
	status, plain := e.call(t, "POST", modelURL(), map[string]any{"name": "gpt", "provider": "openai", "base_url": "https://api.openai.com/v1"})
	if status != 201 {
		t.Fatal(plain)
	}
	if _, ok := plain["region"]; ok {
		t.Error("region must be omitted for non-bedrock models")
	}
	if _, ok := plain["bedrock_model_id"]; ok {
		t.Error("bedrock_model_id must be omitted for non-bedrock models")
	}
}

func TestDeployments_Bedrock(t *testing.T) {
	t.Parallel()

	newParent := func(t *testing.T, e *bedrockEnv, modelType string) string {
		t.Helper()
		typ := modelType
		m, err := e.db.CreateModel(context.Background(), db.CreateModelParams{
			Name: "multi", Provider: "openai", BaseURL: "https://api.openai.com/v1", Source: "api", ModelType: &typ,
		})
		if err != nil {
			t.Fatalf("create parent: %v", err)
		}
		return m.ID
	}
	bedrockDep := func(over map[string]any) map[string]any {
		b := map[string]any{
			"name": "bedrock", "provider": "bedrock", "region": bedrockTestRegion, "bedrock_model_id": bedrockTestModelID, "weight": 1,
		}
		for k, v := range over {
			if v == nil {
				delete(b, k)
			} else {
				b[k] = v
			}
		}
		return b
	}

	t.Run("create without base_url", func(t *testing.T) {
		t.Parallel()
		e := newBedrockEnv(t)
		id := newParent(t, e, "chat")
		status, out := e.call(t, "POST", deploymentURL(id), bedrockDep(nil))
		if status != 201 {
			t.Fatalf("status %d: %v", status, out)
		}
		if out["region"] != bedrockTestRegion || out["bedrock_model_id"] != bedrockTestModelID {
			t.Errorf("response = %v", out)
		}
		if v, ok := out["base_url"]; ok && v != "" {
			t.Errorf("base_url = %v", v)
		}
		stored, err := e.db.GetDeployment(context.Background(), out["id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if stored.BaseURL != "" || stored.Region != bedrockTestRegion || stored.BedrockModelID != bedrockTestModelID {
			t.Errorf("stored = %+v", stored)
		}
	})

	t.Run("create with base_url override", func(t *testing.T) {
		t.Parallel()
		e := newBedrockEnv(t)
		id := newParent(t, e, "chat")
		status, out := e.call(t, "POST", deploymentURL(id), bedrockDep(map[string]any{"base_url": "https://vpce.example.com"}))
		if status != 201 || out["base_url"] != "https://vpce.example.com" {
			t.Errorf("status %d body %v", status, out)
		}
	})

	createRejects := []struct {
		name    string
		body    map[string]any
		parent  string
		wantErr string
	}{
		{"region missing", bedrockDep(map[string]any{"region": nil}), "chat", "region is required for bedrock"},
		{"region invalid", bedrockDep(map[string]any{"region": "eu-central-1/x"}), "chat", "region is not a valid AWS region"},
		{"model id missing", bedrockDep(map[string]any{"bedrock_model_id": nil}), "chat", "bedrock_model_id is required for bedrock"},
		{"model id invalid", bedrockDep(map[string]any{"bedrock_model_id": "a b"}), "chat", "bedrock_model_id is invalid"},
		{"parent embedding", bedrockDep(nil), "embedding", "bedrock supports only chat models"},
		{"bad base_url scheme", bedrockDep(map[string]any{"base_url": "ftp://x"}), "chat", ""},
		{"openai without base_url", map[string]any{"name": "o", "provider": "openai", "weight": 1}, "chat", "base_url is required"},
	}
	for _, tc := range createRejects {
		t.Run("create rejects "+tc.name, func(t *testing.T) {
			t.Parallel()
			e := newBedrockEnv(t)
			id := newParent(t, e, tc.parent)
			status, out := e.call(t, "POST", deploymentURL(id), tc.body)
			if status != 400 {
				t.Fatalf("status = %d, want 400; body %v", status, out)
			}
			if tc.wantErr != "" && errMessage(out) != tc.wantErr {
				t.Errorf("error = %q, want %q", errMessage(out), tc.wantErr)
			}
			deps, err := e.db.ListDeployments(context.Background(), id)
			if err != nil || len(deps) != 0 {
				t.Errorf("rejected request persisted deployments: %v %v", deps, err)
			}
		})
	}

	updateCases := []struct {
		name       string
		create     map[string]any
		update     map[string]any
		wantStatus int
		wantErr    string
		check      func(t *testing.T, out map[string]any)
	}{
		{
			name: "change region", create: bedrockDep(nil), update: map[string]any{"region": "us-east-1"}, wantStatus: 200,
			check: func(t *testing.T, out map[string]any) {
				if out["region"] != "us-east-1" || out["bedrock_model_id"] != bedrockTestModelID {
					t.Errorf("response = %v", out)
				}
			},
		},
		{
			name: "change model id", create: bedrockDep(nil), update: map[string]any{"bedrock_model_id": "us.m-v1:0"}, wantStatus: 200,
			check: func(t *testing.T, out map[string]any) {
				if out["bedrock_model_id"] != "us.m-v1:0" || out["region"] != bedrockTestRegion {
					t.Errorf("response = %v", out)
				}
			},
		},
		{
			name: "clear base_url on bedrock", create: bedrockDep(map[string]any{"base_url": "https://vpce.example.com"}),
			update: map[string]any{"base_url": ""}, wantStatus: 200,
		},
		{
			name:   "clear base_url on openai rejected",
			create: map[string]any{"name": "bedrock", "provider": "openai", "base_url": "https://api.openai.com/v1", "weight": 1},
			update: map[string]any{"base_url": ""}, wantStatus: 400, wantErr: "base_url is required",
		},
		{
			name: "invalid region", create: bedrockDep(nil), update: map[string]any{"region": "nope"}, wantStatus: 400,
			wantErr: "region is not a valid AWS region",
		},
		{
			name: "clear region", create: bedrockDep(nil), update: map[string]any{"region": ""}, wantStatus: 400,
			wantErr: "region is required for bedrock",
		},
		{
			name: "invalid model id", create: bedrockDep(nil), update: map[string]any{"bedrock_model_id": "a b"}, wantStatus: 400,
			wantErr: "bedrock_model_id is invalid",
		},
		{
			name:   "switch openai to bedrock without fields",
			create: map[string]any{"name": "bedrock", "provider": "openai", "base_url": "https://api.openai.com/v1", "weight": 1},
			update: map[string]any{"provider": "bedrock"}, wantStatus: 400, wantErr: "region is required for bedrock",
		},
		{
			name:       "switch openai to bedrock with fields",
			create:     map[string]any{"name": "bedrock", "provider": "openai", "base_url": "https://api.openai.com/v1", "weight": 1},
			update:     map[string]any{"provider": "bedrock", "region": bedrockTestRegion, "bedrock_model_id": bedrockTestModelID, "base_url": ""},
			wantStatus: 200,
		},
		{
			name: "unrelated update keeps fields", create: bedrockDep(nil), update: map[string]any{"priority": 5}, wantStatus: 200,
			check: func(t *testing.T, out map[string]any) {
				if out["region"] != bedrockTestRegion || out["bedrock_model_id"] != bedrockTestModelID {
					t.Errorf("response = %v", out)
				}
			},
		},
	}
	for _, tc := range updateCases {
		t.Run("update "+tc.name, func(t *testing.T) {
			t.Parallel()
			e := newBedrockEnv(t)
			id := newParent(t, e, "chat")
			status, created := e.call(t, "POST", deploymentURL(id), tc.create)
			if status != 201 {
				t.Fatalf("setup: %d %v", status, created)
			}
			depID := created["id"].(string)
			status, out := e.call(t, "PATCH", deploymentItemURL(id, depID), tc.update)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body %v", status, tc.wantStatus, out)
			}
			if tc.wantErr != "" && errMessage(out) != tc.wantErr {
				t.Errorf("error = %q, want %q", errMessage(out), tc.wantErr)
			}
			if tc.check != nil {
				tc.check(t, out)
			}
			if tc.wantStatus >= 400 {
				stored, err := e.db.GetDeployment(context.Background(), depID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Provider != created["provider"] {
					t.Errorf("rejected update changed provider to %q", stored.Provider)
				}
			}
		})
	}
}

func TestTestConnection_Bedrock(t *testing.T) {
	t.Parallel()

	type seenReq struct{ method, uri, auth, body string }
	var mu sync.Mutex
	var seen []seenReq
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, seenReq{r.Method, r.RequestURI, r.Header.Get("Authorization"), string(b)})
		st := status
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		_, _ = io.WriteString(w, `{"output":{"message":{"content":[{"text":"p"}]}},"stopReason":"max_tokens"}`)
	}))
	t.Cleanup(srv.Close)

	setStatus := func(s int) {
		mu.Lock()
		status = s
		mu.Unlock()
	}

	t.Run("override base_url probes converse with bearer", func(t *testing.T) {
		e := newBedrockEnv(t)
		setStatus(200)
		mu.Lock()
		seen = nil
		mu.Unlock()
		code, out := e.call(t, "POST", modelTestConnectionURL(), map[string]any{
			"provider": "bedrock", "base_url": srv.URL, "api_key": "bk", "region": bedrockTestRegion, "bedrock_model_id": bedrockTestModelID,
		})
		if code != 200 || out["success"] != true {
			t.Fatalf("status %d body %v", code, out)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(seen) != 1 {
			t.Fatalf("upstream requests = %v", seen)
		}
		r := seen[0]
		if r.method != "POST" || r.uri != "/model/eu.anthropic.claude-sonnet-4-5-20250929-v1%3A0/converse" || r.auth != "Bearer bk" {
			t.Errorf("probe = %+v", r)
		}
		if !strings.Contains(r.body, `"maxTokens":1`) {
			t.Errorf("probe body = %s", r.body)
		}
	})

	t.Run("upstream 403 reports authentication failure", func(t *testing.T) {
		e := newBedrockEnv(t)
		setStatus(403)
		code, out := e.call(t, "POST", modelTestConnectionURL(), map[string]any{
			"provider": "bedrock", "base_url": srv.URL, "api_key": "bk", "region": bedrockTestRegion, "bedrock_model_id": bedrockTestModelID,
		})
		if code != 200 || out["success"] != false || !strings.Contains(out["message"].(string), "authentication failed (HTTP 403)") {
			t.Errorf("status %d body %v", code, out)
		}
	})

	identity := []struct {
		name    string
		body    map[string]any
		wantMsg string
	}{
		{"region missing with override", map[string]any{"provider": "bedrock", "base_url": srv.URL, "bedrock_model_id": bedrockTestModelID}, "region is required for bedrock"},
		{"region invalid with override", map[string]any{"provider": "bedrock", "base_url": srv.URL, "region": "x.evil.com", "bedrock_model_id": bedrockTestModelID}, "region is not a valid AWS region"},
		{"model id missing with override", map[string]any{"provider": "bedrock", "base_url": srv.URL, "region": bedrockTestRegion}, "bedrock_model_id is required for bedrock"},
		{"model id invalid with override", map[string]any{"provider": "bedrock", "base_url": srv.URL, "region": bedrockTestRegion, "bedrock_model_id": "a b"}, "bedrock_model_id is invalid"},
		{"no base_url region missing", map[string]any{"provider": "bedrock", "bedrock_model_id": bedrockTestModelID}, "region is required for bedrock"},
		{"no base_url region invalid", map[string]any{"provider": "bedrock", "region": "nope", "bedrock_model_id": bedrockTestModelID}, "region is not a valid AWS region"},
		{"no base_url model id missing", map[string]any{"provider": "bedrock", "region": bedrockTestRegion}, "bedrock_model_id is required for bedrock"},
	}
	for _, tc := range identity {
		t.Run(tc.name, func(t *testing.T) {
			e := newBedrockEnv(t)
			mu.Lock()
			seen = nil
			mu.Unlock()
			code, out := e.call(t, "POST", modelTestConnectionURL(), tc.body)
			if code != 200 || out["success"] != false || out["message"] != tc.wantMsg {
				t.Errorf("status %d body %v, want message %q", code, out, tc.wantMsg)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 0 {
				t.Errorf("no upstream call expected, got %v", seen)
			}
		})
	}

	t.Run("non-bedrock without base_url still 400", func(t *testing.T) {
		e := newBedrockEnv(t)
		code, out := e.call(t, "POST", modelTestConnectionURL(), map[string]any{"provider": "openai", "region": bedrockTestRegion})
		if code != 400 || errMessage(out) != "base_url is required" {
			t.Errorf("status %d body %v", code, out)
		}
	})
}

func TestUpdateModel_LoadBalancedBedrockRejectsNonChat(t *testing.T) {
	t.Parallel()

	e := newBedrockEnv(t)
	strategy := "round-robin"
	chat := "chat"
	m, err := e.db.CreateModel(context.Background(), db.CreateModelParams{
		Name: "lb", Provider: "openai", BaseURL: "https://api.openai.com/v1", Source: "api",
		ModelType: &chat, Strategy: &strategy,
	})
	if err != nil {
		t.Fatalf("create model: %v", err)
	}
	status, out := e.call(t, "POST", deploymentURL(m.ID), map[string]any{
		"name": "bedrock", "provider": "bedrock", "region": bedrockTestRegion,
		"bedrock_model_id": bedrockTestModelID, "weight": 1,
	})
	if status != 201 {
		t.Fatalf("create deployment: %d %v", status, out)
	}

	status, out = e.call(t, "PATCH", modelItemURL(m.ID), map[string]any{"type": "embedding"})
	if status != 400 || !strings.Contains(errMessage(out), "bedrock supports only chat models") {
		t.Errorf("status %d body %v, want 400 bedrock chat-only", status, out)
	}
}
