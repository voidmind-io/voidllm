package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/db"
)

const (
	iamTestRole  = "arn:aws:iam::123456789012:role/voidllm"
	iamTestRole2 = "arn:aws:iam::123456789012:role/other"

	msgIAMWithKey    = "api_key must be empty when bedrock_auth is iam"
	msgKeyRequired   = "api_key is required for bedrock when bedrock_auth is api_key"
	msgRoleNeedsIAM  = "bedrock_role_arn requires bedrock_auth iam"
	msgRoleInvalid   = "bedrock_role_arn is invalid"
	msgBadAuth       = "bedrock_auth must be api_key or iam"
	msgNonBedrock    = "bedrock_auth and bedrock_role_arn are only valid for the bedrock provider"
	msgIAMEndpoint   = "base_url must be a Bedrock Runtime endpoint for the configured region when bedrock_auth is iam"
	msgQueryURL      = "base_url must not contain a query string or fragment"
	msgClearStoredIt = "bedrock_auth iam requires clearing the stored api_key: send api_key as an empty string"
)

// assertNoKeyLeak fails if the JSON form of out carries an api_key field or
// the secret value used by the tests.
func assertNoKeyLeak(t *testing.T, out map[string]any) {
	t.Helper()
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out["api_key"]; ok || strings.Contains(string(b), `"bk"`) || strings.Contains(string(b), "api_key_encrypted") {
		t.Errorf("response leaks api key: %s", b)
	}
}

func TestCreateModel_BedrockAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       map[string]any
		wantStatus int
		wantErr    string
		wantAuth   string
		wantRole   string
		wantKey    bool
	}{
		{name: "default mode is api_key", body: bedrockModelBody(nil), wantStatus: 201, wantAuth: "api_key", wantKey: true},
		{name: "explicit api_key", body: bedrockModelBody(map[string]any{"bedrock_auth": "api_key"}), wantStatus: 201, wantAuth: "api_key", wantKey: true},
		{name: "iam without key", body: bedrockModelBody(map[string]any{"bedrock_auth": "iam", "api_key": nil}), wantStatus: 201, wantAuth: "iam"},
		{name: "iam with empty key", body: bedrockModelBody(map[string]any{"bedrock_auth": "iam", "api_key": ""}), wantStatus: 201, wantAuth: "iam"},
		{name: "iam with role", body: bedrockModelBody(map[string]any{"bedrock_auth": "iam", "api_key": nil, "bedrock_role_arn": iamTestRole}), wantStatus: 201, wantAuth: "iam", wantRole: iamTestRole},
		{name: "iam with key rejected", body: bedrockModelBody(map[string]any{"bedrock_auth": "iam"}), wantStatus: 400, wantErr: msgIAMWithKey},
		{name: "iam non-AWS base_url", body: bedrockModelBody(map[string]any{"bedrock_auth": "iam", "api_key": nil, "base_url": "https://evil.example.com"}), wantStatus: 400, wantErr: msgIAMEndpoint},
		{name: "iam http base_url", body: bedrockModelBody(map[string]any{"bedrock_auth": "iam", "api_key": nil, "base_url": "http://bedrock-runtime.eu-central-1.amazonaws.com"}), wantStatus: 400, wantErr: msgIAMEndpoint},
		{name: "iam region mismatch base_url", body: bedrockModelBody(map[string]any{"bedrock_auth": "iam", "api_key": nil, "base_url": "https://bedrock-runtime.us-east-1.amazonaws.com"}), wantStatus: 400, wantErr: msgIAMEndpoint},
		{name: "iam other aws service", body: bedrockModelBody(map[string]any{"bedrock_auth": "iam", "api_key": nil, "base_url": "https://xyz.execute-api.eu-central-1.amazonaws.com"}), wantStatus: 400, wantErr: msgIAMEndpoint},
		{name: "api_key mode query in base_url", body: bedrockModelBody(map[string]any{"base_url": "https://h.example?x=1"}), wantStatus: 400, wantErr: msgQueryURL},
		{name: "api_key mode fragment in base_url", body: bedrockModelBody(map[string]any{"base_url": "https://h.example#f"}), wantStatus: 400, wantErr: msgQueryURL},
		{name: "iam vpce base_url", body: bedrockModelBody(map[string]any{"bedrock_auth": "iam", "api_key": nil, "base_url": "https://vpce-0abc.bedrock-runtime.eu-central-1.vpce.amazonaws.com"}), wantStatus: 201, wantAuth: "iam"},
		{name: "api_key mode without key rejected", body: bedrockModelBody(map[string]any{"api_key": nil}), wantStatus: 400, wantErr: msgKeyRequired},
		{name: "bad role arn", body: bedrockModelBody(map[string]any{"bedrock_auth": "iam", "api_key": nil, "bedrock_role_arn": "nope"}), wantStatus: 400, wantErr: msgRoleInvalid},
		{name: "role arn in api_key mode", body: bedrockModelBody(map[string]any{"bedrock_role_arn": iamTestRole}), wantStatus: 400, wantErr: msgRoleNeedsIAM},
		{name: "unknown auth", body: bedrockModelBody(map[string]any{"bedrock_auth": "sso"}), wantStatus: 400, wantErr: msgBadAuth},
		{name: "bedrock_auth on openai", body: map[string]any{"name": "gpt", "provider": "openai", "base_url": "https://api.openai.com/v1", "bedrock_auth": "iam"}, wantStatus: 400, wantErr: msgNonBedrock},
		{name: "role arn on openai", body: map[string]any{"name": "gpt", "provider": "openai", "base_url": "https://api.openai.com/v1", "bedrock_role_arn": iamTestRole}, wantStatus: 400, wantErr: msgNonBedrock},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newBedrockEnv(t)
			status, out := e.call(t, "POST", modelURL(), tc.body)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body %v", status, tc.wantStatus, out)
			}
			if tc.wantErr != "" {
				if errMessage(out) != tc.wantErr {
					t.Errorf("error = %q, want %q", errMessage(out), tc.wantErr)
				}
				if _, err := e.registry.Resolve("claude"); err == nil {
					t.Error("rejected model must not enter the registry")
				}
				return
			}
			if out["bedrock_auth"] != tc.wantAuth {
				t.Errorf("response bedrock_auth = %v, want %q", out["bedrock_auth"], tc.wantAuth)
			}
			if tc.wantRole == "" {
				if _, ok := out["bedrock_role_arn"]; ok {
					t.Errorf("bedrock_role_arn present without role: %v", out["bedrock_role_arn"])
				}
			} else if out["bedrock_role_arn"] != tc.wantRole {
				t.Errorf("response bedrock_role_arn = %v", out["bedrock_role_arn"])
			}
			assertNoKeyLeak(t, out)

			stored, err := e.db.GetModel(context.Background(), out["id"].(string))
			if err != nil {
				t.Fatal(err)
			}
			if stored.BedrockAuth != tc.wantAuth || stored.BedrockRoleARN != tc.wantRole || (stored.APIKeyEncrypted != nil) != tc.wantKey {
				t.Errorf("stored = auth %q role %q key=%v", stored.BedrockAuth, stored.BedrockRoleARN, stored.APIKeyEncrypted != nil)
			}
			m, err := e.registry.Resolve("claude")
			if err != nil {
				t.Fatal(err)
			}
			if m.BedrockAuth != tc.wantAuth || m.BedrockRoleARN != tc.wantRole {
				t.Errorf("registry = auth %q role %q", m.BedrockAuth, m.BedrockRoleARN)
			}
			if tc.wantAuth == "iam" && m.APIKey != "" {
				t.Errorf("iam registry model carries APIKey")
			}
			if tc.wantAuth == "api_key" && m.APIKey != "bk" {
				t.Errorf("api_key registry model APIKey = %q", m.APIKey)
			}
		})
	}
}

func TestUpdateModel_BedrockAuth(t *testing.T) {
	t.Parallel()

	const openaiURL = "https://api.openai.com/v1"
	iamCreate := bedrockModelBody(map[string]any{"bedrock_auth": "iam", "api_key": nil})
	tests := []struct {
		name       string
		create     map[string]any
		update     map[string]any
		wantStatus int
		wantErr    string
		wantAuth   string
		wantRole   string
		wantKey    bool
	}{
		{name: "api_key to iam clearing key", create: bedrockModelBody(nil), update: map[string]any{"bedrock_auth": "iam", "api_key": ""}, wantStatus: 200, wantAuth: "iam"},
		{name: "api_key to iam with key omitted", create: bedrockModelBody(nil), update: map[string]any{"bedrock_auth": "iam"}, wantStatus: 400, wantErr: msgClearStoredIt, wantAuth: "api_key", wantKey: true},
		{name: "api_key to iam sending a key", create: bedrockModelBody(nil), update: map[string]any{"bedrock_auth": "iam", "api_key": "new"}, wantStatus: 400, wantErr: msgIAMWithKey, wantAuth: "api_key", wantKey: true},
		{name: "api_key to iam with role", create: bedrockModelBody(nil), update: map[string]any{"bedrock_auth": "iam", "api_key": "", "bedrock_role_arn": iamTestRole}, wantStatus: 200, wantAuth: "iam", wantRole: iamTestRole},
		{name: "iam back to api_key without key", create: iamCreate, update: map[string]any{"bedrock_auth": "api_key"}, wantStatus: 400, wantErr: msgKeyRequired, wantAuth: "iam"},
		{name: "iam back to api_key with key", create: iamCreate, update: map[string]any{"bedrock_auth": "api_key", "api_key": "k2"}, wantStatus: 200, wantAuth: "api_key", wantKey: true},
		{name: "iam back to api_key with empty key", create: iamCreate, update: map[string]any{"bedrock_auth": "api_key", "api_key": ""}, wantStatus: 400, wantErr: msgKeyRequired, wantAuth: "iam"},
		{name: "empty auth normalized to api_key", create: iamCreate, update: map[string]any{"bedrock_auth": "", "api_key": "k2"}, wantStatus: 200, wantAuth: "api_key", wantKey: true},
		{name: "iam back to api_key keeping role", create: bedrockModelBody(map[string]any{"bedrock_auth": "iam", "api_key": nil, "bedrock_role_arn": iamTestRole}), update: map[string]any{"bedrock_auth": "api_key", "api_key": "k2"}, wantStatus: 400, wantErr: msgRoleNeedsIAM, wantAuth: "iam", wantRole: iamTestRole},
		{name: "iam back to api_key unsetting role", create: bedrockModelBody(map[string]any{"bedrock_auth": "iam", "api_key": nil, "bedrock_role_arn": iamTestRole}), update: map[string]any{"bedrock_auth": "api_key", "api_key": "k2", "bedrock_role_arn": ""}, wantStatus: 200, wantAuth: "api_key", wantKey: true},
		{name: "iam model to non-AWS base_url", create: iamCreate, update: map[string]any{"base_url": "https://evil.example.com"}, wantStatus: 400, wantErr: msgIAMEndpoint, wantAuth: "iam"},
		{name: "api_key to iam with non-AWS base_url", create: bedrockModelBody(map[string]any{"base_url": "http://localhost:9999"}), update: map[string]any{"bedrock_auth": "iam", "api_key": ""}, wantStatus: 400, wantErr: msgIAMEndpoint, wantAuth: "api_key", wantKey: true},
		{name: "api_key to iam resetting base_url", create: bedrockModelBody(map[string]any{"base_url": "http://localhost:9999"}), update: map[string]any{"bedrock_auth": "iam", "api_key": "", "base_url": ""}, wantStatus: 200, wantAuth: "iam"},
		{name: "update base_url with query", create: bedrockModelBody(nil), update: map[string]any{"base_url": "https://h.example/?x=1"}, wantStatus: 400, wantErr: msgQueryURL, wantAuth: "api_key", wantKey: true},
		{name: "set role on iam model", create: iamCreate, update: map[string]any{"bedrock_role_arn": iamTestRole}, wantStatus: 200, wantAuth: "iam", wantRole: iamTestRole},
		{name: "change role", create: bedrockModelBody(map[string]any{"bedrock_auth": "iam", "api_key": nil, "bedrock_role_arn": iamTestRole}), update: map[string]any{"bedrock_role_arn": iamTestRole2}, wantStatus: 200, wantAuth: "iam", wantRole: iamTestRole2},
		{name: "unset role", create: bedrockModelBody(map[string]any{"bedrock_auth": "iam", "api_key": nil, "bedrock_role_arn": iamTestRole}), update: map[string]any{"bedrock_role_arn": ""}, wantStatus: 200, wantAuth: "iam"},
		{name: "invalid role", create: iamCreate, update: map[string]any{"bedrock_role_arn": "bad"}, wantStatus: 400, wantErr: msgRoleInvalid, wantAuth: "iam"},
		{name: "role on api_key model", create: bedrockModelBody(nil), update: map[string]any{"bedrock_role_arn": iamTestRole}, wantStatus: 400, wantErr: msgRoleNeedsIAM, wantAuth: "api_key", wantKey: true},
		{name: "unknown auth", create: bedrockModelBody(nil), update: map[string]any{"bedrock_auth": "sso"}, wantStatus: 400, wantErr: msgBadAuth, wantAuth: "api_key", wantKey: true},
		{name: "api key on iam model rejected", create: iamCreate, update: map[string]any{"api_key": "k"}, wantStatus: 400, wantErr: msgIAMWithKey, wantAuth: "iam"},
		{name: "rotate key on api_key model", create: bedrockModelBody(nil), update: map[string]any{"api_key": "rotated"}, wantStatus: 200, wantAuth: "api_key", wantKey: true},
		{name: "unrelated edit leaves iam alone", create: iamCreate, update: map[string]any{"timeout": "45s"}, wantStatus: 200, wantAuth: "iam"},
		{name: "bedrock_auth on openai model", create: map[string]any{"name": "claude", "provider": "openai", "base_url": openaiURL}, update: map[string]any{"bedrock_auth": "iam"}, wantStatus: 400, wantErr: msgNonBedrock},
		{name: "switch openai to bedrock iam", create: map[string]any{"name": "claude", "provider": "openai", "base_url": openaiURL}, update: map[string]any{"provider": "bedrock", "bedrock_auth": "iam", "region": bedrockTestRegion, "bedrock_model_id": bedrockTestModelID, "base_url": ""}, wantStatus: 200, wantAuth: "iam"},
		{name: "switch openai to bedrock api_key requires key", create: map[string]any{"name": "claude", "provider": "openai", "base_url": openaiURL}, update: map[string]any{"provider": "bedrock", "region": bedrockTestRegion, "bedrock_model_id": bedrockTestModelID, "base_url": ""}, wantStatus: 400, wantErr: msgKeyRequired},
		{name: "leaving bedrock resets auth", create: bedrockModelBody(map[string]any{"bedrock_auth": "iam", "api_key": nil, "bedrock_role_arn": iamTestRole}), update: map[string]any{"provider": "openai", "base_url": openaiURL}, wantStatus: 200, wantAuth: "api_key"},
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
			if status == 200 {
				assertNoKeyLeak(t, out)
			}
			stored, err := e.db.GetModel(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantAuth != "" {
				if stored.BedrockAuth != tc.wantAuth || stored.BedrockRoleARN != tc.wantRole || (stored.APIKeyEncrypted != nil) != tc.wantKey {
					t.Errorf("stored = auth %q role %q key=%v; want %q %q key=%v", stored.BedrockAuth, stored.BedrockRoleARN,
						stored.APIKeyEncrypted != nil, tc.wantAuth, tc.wantRole, tc.wantKey)
				}
			}
			if status == 200 && stored.Provider == "bedrock" {
				if out["bedrock_auth"] != tc.wantAuth {
					t.Errorf("response bedrock_auth = %v, want %q", out["bedrock_auth"], tc.wantAuth)
				}
				m, err := e.registry.Resolve("claude")
				if err != nil {
					t.Fatal(err)
				}
				if m.BedrockAuth != tc.wantAuth || m.BedrockRoleARN != tc.wantRole {
					t.Errorf("registry = %q %q", m.BedrockAuth, m.BedrockRoleARN)
				}
				if tc.wantAuth == "iam" && m.APIKey != "" {
					t.Error("iam registry model carries APIKey")
				}
			}
			if status == 200 && stored.Provider != "bedrock" {
				if _, ok := out["bedrock_auth"]; ok {
					t.Errorf("non-bedrock response exposes bedrock_auth: %v", out["bedrock_auth"])
				}
				if stored.BedrockRoleARN != "" {
					t.Errorf("role not reset on leaving bedrock: %q", stored.BedrockRoleARN)
				}
			}
		})
	}
}

// TestUpdateModel_LegacyBedrockRowWithoutKey covers a bedrock row stored before
// the auth mode existed and without a key: edits unrelated to auth must keep
// working.
func TestUpdateModel_LegacyBedrockRowWithoutKey(t *testing.T) {
	t.Parallel()
	e := newBedrockEnv(t)
	m, err := e.db.CreateModel(context.Background(), db.CreateModelParams{
		Name: "legacy", Provider: "bedrock", Region: bedrockTestRegion, BedrockModelID: bedrockTestModelID, Source: "api",
	})
	if err != nil {
		t.Fatal(err)
	}

	status, out := e.call(t, "PATCH", modelItemURL(m.ID), map[string]any{"name": "renamed"})
	if status != 200 {
		t.Fatalf("rename-only: %d %v", status, out)
	}
	if out["name"] != "renamed" || out["bedrock_auth"] != "api_key" {
		t.Errorf("response = %v", out)
	}

	// Touching the auth mode on such a row enforces the full rules.
	status, out = e.call(t, "PATCH", modelItemURL(m.ID), map[string]any{"bedrock_auth": "api_key"})
	if status != 400 || errMessage(out) != msgKeyRequired {
		t.Errorf("explicit api_key without key: %d %v", status, out)
	}
	status, out = e.call(t, "PATCH", modelItemURL(m.ID), map[string]any{"bedrock_auth": "iam"})
	if status != 200 || out["bedrock_auth"] != "iam" {
		t.Errorf("legacy to iam: %d %v", status, out)
	}
}

func TestGetModel_BedrockAuthInResponses(t *testing.T) {
	t.Parallel()
	e := newBedrockEnv(t)
	status, created := e.call(t, "POST", modelURL(), bedrockModelBody(map[string]any{"bedrock_auth": "iam", "api_key": nil, "bedrock_role_arn": iamTestRole}))
	if status != 201 {
		t.Fatal(created)
	}
	status, got := e.call(t, "GET", modelItemURL(created["id"].(string)), nil)
	if status != 200 || got["bedrock_auth"] != "iam" || got["bedrock_role_arn"] != iamTestRole {
		t.Errorf("GET = %d %v", status, got)
	}
	assertNoKeyLeak(t, got)

	status, plain := e.call(t, "POST", modelURL(), map[string]any{"name": "gpt", "provider": "openai", "base_url": "https://api.openai.com/v1"})
	if status != 201 {
		t.Fatal(plain)
	}
	for _, k := range []string{"bedrock_auth", "bedrock_role_arn"} {
		if _, ok := plain[k]; ok {
			t.Errorf("%s must be omitted for non-bedrock models", k)
		}
	}
	status, list := e.call(t, "GET", modelURL(), nil)
	if status != 200 {
		t.Fatal(list)
	}
	for _, it := range list["data"].([]any) {
		item := it.(map[string]any)
		_, has := item["bedrock_auth"]
		if (item["provider"] == "bedrock") != has {
			t.Errorf("list item %v: bedrock_auth presence = %v", item["name"], has)
		}
		assertNoKeyLeak(t, item)
	}
}

func TestDeployments_BedrockAuth(t *testing.T) {
	t.Parallel()

	newParent := func(t *testing.T, e *bedrockEnv) string {
		t.Helper()
		m, err := e.db.CreateModel(context.Background(), db.CreateModelParams{
			Name: "multi", Provider: "openai", BaseURL: "https://api.openai.com/v1", Source: "api",
		})
		if err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	dep := func(over map[string]any) map[string]any {
		b := map[string]any{
			"name": "bedrock", "provider": "bedrock", "api_key": "bk", "region": bedrockTestRegion, "bedrock_model_id": bedrockTestModelID, "weight": 1,
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

	createTests := []struct {
		name       string
		body       map[string]any
		wantStatus int
		wantErr    string
		wantAuth   string
		wantRole   string
		wantKey    bool
	}{
		{name: "default api_key", body: dep(nil), wantStatus: 201, wantAuth: "api_key", wantKey: true},
		{name: "iam", body: dep(map[string]any{"bedrock_auth": "iam", "api_key": nil}), wantStatus: 201, wantAuth: "iam"},
		{name: "iam with role", body: dep(map[string]any{"bedrock_auth": "iam", "api_key": nil, "bedrock_role_arn": iamTestRole}), wantStatus: 201, wantAuth: "iam", wantRole: iamTestRole},
		{name: "iam with key", body: dep(map[string]any{"bedrock_auth": "iam"}), wantStatus: 400, wantErr: msgIAMWithKey},
		{name: "api_key mode query in base_url", body: dep(map[string]any{"base_url": "https://h.example?x=1"}), wantStatus: 400, wantErr: msgQueryURL},
		{name: "iam non-AWS base_url", body: dep(map[string]any{"bedrock_auth": "iam", "api_key": nil, "base_url": "https://evil.example.com"}), wantStatus: 400, wantErr: msgIAMEndpoint},
		{name: "iam http base_url", body: dep(map[string]any{"bedrock_auth": "iam", "api_key": nil, "base_url": "http://bedrock-runtime.eu-central-1.amazonaws.com"}), wantStatus: 400, wantErr: msgIAMEndpoint},
		{name: "api_key without key", body: dep(map[string]any{"api_key": nil}), wantStatus: 400, wantErr: msgKeyRequired},
		{name: "bad role", body: dep(map[string]any{"bedrock_auth": "iam", "api_key": nil, "bedrock_role_arn": "x"}), wantStatus: 400, wantErr: msgRoleInvalid},
		{name: "role in api_key mode", body: dep(map[string]any{"bedrock_role_arn": iamTestRole}), wantStatus: 400, wantErr: msgRoleNeedsIAM},
		{name: "unknown auth", body: dep(map[string]any{"bedrock_auth": "x"}), wantStatus: 400, wantErr: msgBadAuth},
		{name: "bedrock_auth on openai", body: map[string]any{"name": "o", "provider": "openai", "base_url": "https://api.openai.com/v1", "bedrock_auth": "iam", "weight": 1}, wantStatus: 400, wantErr: msgNonBedrock},
	}
	for _, tc := range createTests {
		t.Run("create "+tc.name, func(t *testing.T) {
			t.Parallel()
			e := newBedrockEnv(t)
			id := newParent(t, e)
			status, out := e.call(t, "POST", deploymentURL(id), tc.body)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body %v", status, tc.wantStatus, out)
			}
			if tc.wantErr != "" {
				if errMessage(out) != tc.wantErr {
					t.Errorf("error = %q, want %q", errMessage(out), tc.wantErr)
				}
				return
			}
			if out["bedrock_auth"] != tc.wantAuth {
				t.Errorf("bedrock_auth = %v", out["bedrock_auth"])
			}
			if tc.wantRole != "" && out["bedrock_role_arn"] != tc.wantRole {
				t.Errorf("bedrock_role_arn = %v", out["bedrock_role_arn"])
			}
			assertNoKeyLeak(t, out)
			stored, err := e.db.GetDeployment(context.Background(), out["id"].(string))
			if err != nil {
				t.Fatal(err)
			}
			if stored.BedrockAuth != tc.wantAuth || stored.BedrockRoleARN != tc.wantRole || (stored.APIKeyEncrypted != nil) != tc.wantKey {
				t.Errorf("stored = %q %q key=%v", stored.BedrockAuth, stored.BedrockRoleARN, stored.APIKeyEncrypted != nil)
			}
		})
	}

	updateTests := []struct {
		name       string
		create     map[string]any
		update     map[string]any
		wantStatus int
		wantErr    string
		wantAuth   string
		wantRole   string
		wantKey    bool
	}{
		{name: "api_key to iam clearing key", create: dep(nil), update: map[string]any{"bedrock_auth": "iam", "api_key": ""}, wantStatus: 200, wantAuth: "iam"},
		{name: "api_key to iam key omitted", create: dep(nil), update: map[string]any{"bedrock_auth": "iam"}, wantStatus: 400, wantErr: msgClearStoredIt, wantAuth: "api_key", wantKey: true},
		{name: "iam to api_key without key", create: dep(map[string]any{"bedrock_auth": "iam", "api_key": nil}), update: map[string]any{"bedrock_auth": "api_key"}, wantStatus: 400, wantErr: msgKeyRequired, wantAuth: "iam"},
		{name: "iam to api_key with key", create: dep(map[string]any{"bedrock_auth": "iam", "api_key": nil}), update: map[string]any{"bedrock_auth": "api_key", "api_key": "k"}, wantStatus: 200, wantAuth: "api_key", wantKey: true},
		{name: "iam deployment to non-AWS base_url", create: dep(map[string]any{"bedrock_auth": "iam", "api_key": nil}), update: map[string]any{"base_url": "https://evil.example.com"}, wantStatus: 400, wantErr: msgIAMEndpoint, wantAuth: "iam"},
		{name: "api_key to iam with non-AWS base_url", create: dep(map[string]any{"base_url": "http://localhost:9999"}), update: map[string]any{"bedrock_auth": "iam", "api_key": ""}, wantStatus: 400, wantErr: msgIAMEndpoint, wantAuth: "api_key", wantKey: true},
		{name: "update deployment base_url with fragment", create: dep(nil), update: map[string]any{"base_url": "https://h.example#f"}, wantStatus: 400, wantErr: msgQueryURL, wantAuth: "api_key", wantKey: true},
		{name: "set role", create: dep(map[string]any{"bedrock_auth": "iam", "api_key": nil}), update: map[string]any{"bedrock_role_arn": iamTestRole}, wantStatus: 200, wantAuth: "iam", wantRole: iamTestRole},
		{name: "unset role", create: dep(map[string]any{"bedrock_auth": "iam", "api_key": nil, "bedrock_role_arn": iamTestRole}), update: map[string]any{"bedrock_role_arn": ""}, wantStatus: 200, wantAuth: "iam"},
		{name: "role on api_key deployment", create: dep(nil), update: map[string]any{"bedrock_role_arn": iamTestRole}, wantStatus: 400, wantErr: msgRoleNeedsIAM, wantAuth: "api_key", wantKey: true},
		{name: "invalid role", create: dep(map[string]any{"bedrock_auth": "iam", "api_key": nil}), update: map[string]any{"bedrock_role_arn": "x"}, wantStatus: 400, wantErr: msgRoleInvalid, wantAuth: "iam"},
		{name: "unknown auth", create: dep(nil), update: map[string]any{"bedrock_auth": "x"}, wantStatus: 400, wantErr: msgBadAuth, wantAuth: "api_key", wantKey: true},
		{name: "rename only", create: dep(map[string]any{"bedrock_auth": "iam", "api_key": nil}), update: map[string]any{"name": "renamed"}, wantStatus: 200, wantAuth: "iam"},
		{name: "bedrock_auth on openai deployment", create: map[string]any{"name": "bedrock", "provider": "openai", "base_url": "https://api.openai.com/v1", "weight": 1}, update: map[string]any{"bedrock_auth": "iam"}, wantStatus: 400, wantErr: msgNonBedrock},
		{name: "leaving bedrock resets auth", create: dep(map[string]any{"bedrock_auth": "iam", "api_key": nil, "bedrock_role_arn": iamTestRole}), update: map[string]any{"provider": "openai", "base_url": "https://api.openai.com/v1"}, wantStatus: 200, wantAuth: "api_key"},
	}
	for _, tc := range updateTests {
		t.Run("update "+tc.name, func(t *testing.T) {
			t.Parallel()
			e := newBedrockEnv(t)
			pid := newParent(t, e)
			status, created := e.call(t, "POST", deploymentURL(pid), tc.create)
			if status != 201 {
				t.Fatalf("setup create: %d %v", status, created)
			}
			did := created["id"].(string)
			status, out := e.call(t, "PATCH", deploymentItemURL(pid, did), tc.update)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body %v", status, tc.wantStatus, out)
			}
			if tc.wantErr != "" && errMessage(out) != tc.wantErr {
				t.Errorf("error = %q, want %q", errMessage(out), tc.wantErr)
			}
			if status == 200 {
				assertNoKeyLeak(t, out)
			}
			if tc.wantAuth == "" {
				return
			}
			stored, err := e.db.GetDeployment(context.Background(), did)
			if err != nil {
				t.Fatal(err)
			}
			if stored.BedrockAuth != tc.wantAuth || stored.BedrockRoleARN != tc.wantRole || (stored.APIKeyEncrypted != nil) != tc.wantKey {
				t.Errorf("stored = %q %q key=%v; want %q %q key=%v", stored.BedrockAuth, stored.BedrockRoleARN,
					stored.APIKeyEncrypted != nil, tc.wantAuth, tc.wantRole, tc.wantKey)
			}
			if status == 200 && stored.Provider == "bedrock" && out["bedrock_auth"] != tc.wantAuth {
				t.Errorf("response bedrock_auth = %v", out["bedrock_auth"])
			}
			if status == 200 && stored.Provider != "bedrock" {
				if _, ok := out["bedrock_auth"]; ok {
					t.Errorf("non-bedrock deployment exposes bedrock_auth")
				}
			}
		})
	}

	t.Run("legacy deployment without key can be renamed", func(t *testing.T) {
		t.Parallel()
		e := newBedrockEnv(t)
		pid := newParent(t, e)
		d, err := e.db.CreateDeployment(context.Background(), db.CreateDeploymentParams{
			ModelID: pid, Name: "legacy", Provider: "bedrock", Region: bedrockTestRegion, BedrockModelID: bedrockTestModelID, Weight: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		status, out := e.call(t, "PATCH", deploymentItemURL(pid, d.ID), map[string]any{"name": "renamed"})
		if status != 200 || out["name"] != "renamed" {
			t.Errorf("rename: %d %v", status, out)
		}
	})
}

// recSigner is a fake proxy.BedrockSigner.
type recSigner struct {
	mu    sync.Mutex
	calls []string // region|role
	err   error
}

func (s *recSigner) Sign(_ context.Context, req *http.Request, _ []byte, region, roleARN string) error {
	s.mu.Lock()
	s.calls = append(s.calls, region+"|"+roleARN)
	s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=FAKE/20260101/"+region+"/bedrock/aws4_request")
	return nil
}

func TestTestConnection_BedrockIAM(t *testing.T) {
	t.Parallel()

	type seenReq struct{ method, uri, auth string }
	run := func(t *testing.T, signer *recSigner, body map[string]any) (int, map[string]any, []seenReq) {
		t.Helper()
		var mu sync.Mutex
		var seen []seenReq
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			mu.Lock()
			seen = append(seen, seenReq{r.Method, r.RequestURI, r.Header.Get("Authorization")})
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"output":{"message":{"content":[{"text":"p"}]}},"stopReason":"max_tokens"}`)
		}))
		t.Cleanup(srv.Close)
		body["base_url"] = admin.AWSTestURL(srv.URL)
		var e *bedrockEnv
		if signer == nil {
			e = newBedrockEnv(t)
		} else {
			e = newBedrockEnvWithSigner(t, signer)
		}
		code, out := e.call(t, "POST", modelTestConnectionURL(), body)
		mu.Lock()
		defer mu.Unlock()
		return code, out, append([]seenReq(nil), seen...)
	}
	base := func(over map[string]any) map[string]any {
		b := map[string]any{"provider": "bedrock", "region": bedrockTestRegion, "bedrock_model_id": bedrockTestModelID, "bedrock_auth": "iam"}
		for k, v := range over {
			b[k] = v
		}
		return b
	}

	t.Run("iam probe is signed and carries no bearer", func(t *testing.T) {
		t.Parallel()
		s := &recSigner{}
		code, out, seen := run(t, s, base(map[string]any{"bedrock_role_arn": iamTestRole}))
		if code != 200 || out["success"] != true {
			t.Fatalf("status %d body %v", code, out)
		}
		if len(seen) != 1 || !strings.HasPrefix(seen[0].auth, "AWS4-HMAC-SHA256") || strings.Contains(seen[0].auth, "Bearer") {
			t.Fatalf("upstream saw %+v", seen)
		}
		if len(s.calls) != 1 || s.calls[0] != bedrockTestRegion+"|"+iamTestRole {
			t.Errorf("sign calls = %v", s.calls)
		}
	})

	t.Run("api_key mode never calls the signer", func(t *testing.T) {
		t.Parallel()
		s := &recSigner{}
		code, out, seen := run(t, s, base(map[string]any{"bedrock_auth": "api_key", "api_key": "bk"}))
		if code != 200 || out["success"] != true {
			t.Fatalf("status %d body %v", code, out)
		}
		if len(s.calls) != 0 {
			t.Errorf("signer called %v", s.calls)
		}
		if len(seen) != 1 || seen[0].auth != "Bearer bk" {
			t.Errorf("upstream saw %+v", seen)
		}
	})

	t.Run("empty auth defaults to api_key", func(t *testing.T) {
		t.Parallel()
		s := &recSigner{}
		_, _, seen := run(t, s, base(map[string]any{"bedrock_auth": "", "api_key": "bk"}))
		if len(s.calls) != 0 || len(seen) != 1 || seen[0].auth != "Bearer bk" {
			t.Errorf("calls %v seen %+v", s.calls, seen)
		}
	})

	t.Run("no signer configured", func(t *testing.T) {
		t.Parallel()
		code, out, seen := run(t, nil, base(nil))
		if code != 200 || out["success"] != false || out["message"] != "Bedrock IAM credentials unavailable" {
			t.Errorf("status %d body %v", code, out)
		}
		if len(seen) != 0 {
			t.Errorf("request sent without signature: %+v", seen)
		}
	})

	t.Run("signer failure is generic", func(t *testing.T) {
		t.Parallel()
		s := &recSigner{err: errors.New("sts: SECRET-FROM-SDK")}
		code, out, seen := run(t, s, base(nil))
		if code != 200 || out["success"] != false || out["message"] != "Bedrock IAM credentials unavailable" {
			t.Errorf("status %d body %v", code, out)
		}
		if b, _ := json.Marshal(out); strings.Contains(string(b), "SECRET") {
			t.Errorf("leak: %s", b)
		}
		if len(seen) != 0 {
			t.Errorf("request sent after signing failure: %+v", seen)
		}
	})

	t.Run("create-style auth rules apply before any probe", func(t *testing.T) {
		t.Parallel()
		for name, tc := range map[string]struct {
			over map[string]any
			msg  string
		}{
			"iam with key":         {map[string]any{"api_key": "bk"}, msgIAMWithKey},
			"api_key without key":  {map[string]any{"bedrock_auth": "api_key"}, msgKeyRequired},
			"role in api_key mode": {map[string]any{"bedrock_auth": "api_key", "api_key": "bk", "bedrock_role_arn": iamTestRole}, msgRoleNeedsIAM},
		} {
			s := &recSigner{}
			code, out, seen := run(t, s, base(tc.over))
			if code != 400 || errMessage(out) != tc.msg {
				t.Errorf("%s: status %d body %v", name, code, out)
			}
			if len(seen) != 0 || len(s.calls) != 0 {
				t.Errorf("%s: probe was sent", name)
			}
		}
	})

	t.Run("query or fragment rejected in any auth mode", func(t *testing.T) {
		t.Parallel()
		for _, bad := range []string{"https://h.example?x=1", "https://h.example#f"} {
			s := &recSigner{}
			e := newBedrockEnvWithSigner(t, s)
			code, out := e.call(t, "POST", modelTestConnectionURL(), base(map[string]any{"bedrock_auth": "api_key", "api_key": "bk", "base_url": bad}))
			if code != 400 || errMessage(out) != msgQueryURL {
				t.Errorf("%s: status %d body %v", bad, code, out)
			}
		}
	})

	t.Run("iam rejects non-AWS endpoints before any probe", func(t *testing.T) {
		t.Parallel()
		for _, bad := range []string{"https://evil.example.com", "http://bedrock-runtime.eu-central-1.amazonaws.com", "https://127.0.0.1:8443"} {
			s := &recSigner{}
			e := newBedrockEnvWithSigner(t, s)
			body := base(map[string]any{"base_url": bad})
			code, out := e.call(t, "POST", modelTestConnectionURL(), body)
			if code != 400 || errMessage(out) != msgIAMEndpoint {
				t.Errorf("%s: status %d body %v", bad, code, out)
			}
			if len(s.calls) != 0 {
				t.Errorf("%s: signer called", bad)
			}
		}
	})

	t.Run("identity validation", func(t *testing.T) {
		t.Parallel()
		for name, tc := range map[string]struct {
			over map[string]any
			msg  string
		}{
			"unknown auth": {map[string]any{"bedrock_auth": "sso"}, "bedrock_auth must be api_key or iam"},
			"bad role":     {map[string]any{"bedrock_role_arn": "nope"}, "bedrock_role_arn is invalid"},
		} {
			s := &recSigner{}
			code, out, seen := run(t, s, base(tc.over))
			if code != 200 || out["success"] != false || out["message"] != tc.msg {
				t.Errorf("%s: status %d body %v", name, code, out)
			}
			if len(seen) != 0 || len(s.calls) != 0 {
				t.Errorf("%s: probe was sent", name)
			}
		}
	})
}

// TestUpdate_EmptyAPIKeyClearsStoredKey verifies that PATCH with "api_key": ""
// clears the stored key for non-bedrock models and deployments, and that the
// registry no longer carries the key.
func TestUpdate_EmptyAPIKeyClearsStoredKey(t *testing.T) {
	t.Parallel()

	t.Run("model", func(t *testing.T) {
		t.Parallel()
		e := newBedrockEnv(t)
		status, created := e.call(t, "POST", modelURL(), map[string]any{
			"name": "gpt", "provider": "openai", "base_url": "https://api.openai.com/v1", "api_key": "sk-secret",
		})
		if status != 201 {
			t.Fatalf("create: %d %v", status, created)
		}
		id := created["id"].(string)
		stored, err := e.db.GetModel(context.Background(), id)
		if err != nil || stored.APIKeyEncrypted == nil {
			t.Fatalf("key not stored: %v %v", err, stored)
		}
		if m, err := e.registry.Resolve("gpt"); err != nil || m.APIKey != "sk-secret" {
			t.Fatalf("registry before = %v %v", m, err)
		}

		status, out := e.call(t, "PATCH", modelItemURL(id), map[string]any{"api_key": ""})
		if status != 200 {
			t.Fatalf("patch: %d %v", status, out)
		}
		stored, err = e.db.GetModel(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if stored.APIKeyEncrypted != nil {
			t.Error("APIKeyEncrypted not cleared")
		}
		m, err := e.registry.Resolve("gpt")
		if err != nil {
			t.Fatal(err)
		}
		if m.APIKey != "" {
			t.Error("registry still carries the API key")
		}
	})

	t.Run("deployment", func(t *testing.T) {
		t.Parallel()
		e := newBedrockEnv(t)
		parent, err := e.db.CreateModel(context.Background(), db.CreateModelParams{
			Name: "multi", Provider: "openai", BaseURL: "https://api.openai.com/v1", Source: "api",
		})
		if err != nil {
			t.Fatal(err)
		}
		status, created := e.call(t, "POST", deploymentURL(parent.ID), map[string]any{
			"name": "d1", "provider": "openai", "base_url": "https://api.openai.com/v1", "api_key": "sk-secret", "weight": 1,
		})
		if status != 201 {
			t.Fatalf("create: %d %v", status, created)
		}
		did := created["id"].(string)
		stored, err := e.db.GetDeployment(context.Background(), did)
		if err != nil || stored.APIKeyEncrypted == nil {
			t.Fatalf("key not stored: %v", err)
		}

		status, out := e.call(t, "PATCH", deploymentItemURL(parent.ID, did), map[string]any{"api_key": ""})
		if status != 200 {
			t.Fatalf("patch: %d %v", status, out)
		}
		stored, err = e.db.GetDeployment(context.Background(), did)
		if err != nil {
			t.Fatal(err)
		}
		if stored.APIKeyEncrypted != nil {
			t.Error("APIKeyEncrypted not cleared")
		}
	})
}
