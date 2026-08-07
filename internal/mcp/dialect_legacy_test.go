package mcp_test

import (
	"encoding/json"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// ---- Version -------------------------------------------------------------

func TestLegacyDialect_Version(t *testing.T) {
	t.Parallel()

	for _, v := range []mcp.Version{mcp.V20250326, mcp.V20250618, mcp.V20251125} {
		t.Run(string(v), func(t *testing.T) {
			t.Parallel()

			d := mcp.NewLegacyDialect(v)
			if got := d.Version(); got != v {
				t.Errorf("Version() = %q, want %q", got, v)
			}
		})
	}
}

// ---- Decode ---------------------------------------------------------------

func TestLegacyDialect_Decode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		raw            string
		wantMethod     string
		wantName       string
		wantNotify     bool
		wantVersion    mcp.Version
		wantClientName string
	}{
		{
			name:        "tools/list carries no name",
			raw:         `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			wantMethod:  "tools/list",
			wantVersion: mcp.V20250326,
		},
		{
			name:        "tools/call extracts params.name",
			raw:         `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}`,
			wantMethod:  "tools/call",
			wantName:    "echo",
			wantVersion: mcp.V20250326,
		},
		{
			name: "initialize extracts clientInfo and mirrors a recognized protocolVersion",
			raw: `{"jsonrpc":"2.0","id":3,"method":"initialize","params":{
				"protocolVersion":"2025-11-25",
				"clientInfo":{"name":"cursor","version":"1.0"}
			}}`,
			wantMethod:     "initialize",
			wantVersion:    mcp.V20251125,
			wantClientName: "cursor",
		},
		{
			name: "initialize with unrecognized protocolVersion falls back to the dialect's negotiated version",
			raw: `{"jsonrpc":"2.0","id":4,"method":"initialize","params":{
				"protocolVersion":"bogus-version"
			}}`,
			wantMethod:  "initialize",
			wantVersion: mcp.V20250326,
		},
		{
			name:        "notification (no id) is flagged IsNotification",
			raw:         `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			wantMethod:  "notifications/initialized",
			wantNotify:  true,
			wantVersion: mcp.V20250326,
		},
		{
			name:        "explicit null id is a notification",
			raw:         `{"jsonrpc":"2.0","id":null,"method":"ping"}`,
			wantMethod:  "ping",
			wantNotify:  true,
			wantVersion: mcp.V20250326,
		},
	}

	d := mcp.NewLegacyDialect(mcp.V20250326)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env, err := d.Decode([]byte(tc.raw), mcp.MapHeader{})
			if err != nil {
				t.Fatalf("Decode() unexpected error: %+v", err)
			}
			if env.Method != tc.wantMethod {
				t.Errorf("Method = %q, want %q", env.Method, tc.wantMethod)
			}
			if env.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", env.Name, tc.wantName)
			}
			if env.IsNotification != tc.wantNotify {
				t.Errorf("IsNotification = %v, want %v", env.IsNotification, tc.wantNotify)
			}
			if env.Version != tc.wantVersion {
				t.Errorf("Version = %q, want %q", env.Version, tc.wantVersion)
			}
			if tc.wantClientName != "" && env.ClientInfo.Name != tc.wantClientName {
				t.Errorf("ClientInfo.Name = %q, want %q", env.ClientInfo.Name, tc.wantClientName)
			}
			// Legacy requests carry no per-request log level.
			if env.LogLevel != "" {
				t.Errorf("LogLevel = %q, want empty (legacy has no per-request log level)", env.LogLevel)
			}
		})
	}
}

func TestLegacyDialect_Decode_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		raw      string
		wantCode int
	}{
		{"invalid JSON", `{not valid`, mcp.CodeParseError},
		{"wrong jsonrpc version", `{"jsonrpc":"1.0","id":1,"method":"ping"}`, mcp.CodeInvalidRequest},
	}

	d := mcp.NewLegacyDialect(mcp.V20250326)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := d.Decode([]byte(tc.raw), mcp.MapHeader{})
			if err == nil {
				t.Fatal("Decode() error = nil, want non-nil")
			}
			if err.Code != tc.wantCode {
				t.Errorf("Decode() error code = %d, want %d", err.Code, tc.wantCode)
			}
		})
	}
}

// ---- EncodeResult -----------------------------------------------------------

// TestLegacyDialect_EncodeResult_NoCacheableResultFields verifies that legacy
// results carry the payload verbatim under "result", with NO resultType,
// ttlMs, or cacheScope wrapper — those are 2026-07-28 CacheableResult concepts
// legacy revisions predate (docs/mcp-v2.md §5, §8).
func TestLegacyDialect_EncodeResult_NoCacheableResultFields(t *testing.T) {
	t.Parallel()

	d := mcp.NewLegacyDialect(mcp.V20250326)

	result := &mcp.Result{
		Payload: map[string]any{"tools": []any{}},
		// Even though Cache carries a hint here, the legacy encoder must ignore
		// it entirely — legacy has no on-wire representation for it.
		Cache: mcp.CacheHint{TTLMs: 60000, Scope: mcp.CacheScopePrivate},
	}

	out, encErr := d.EncodeResult(json.RawMessage(`7`), result)
	if encErr != nil {
		t.Fatalf("EncodeResult() unexpected error: %v", encErr)
	}

	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal encoded result: %v", err)
	}

	if decoded["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want \"2.0\"", decoded["jsonrpc"])
	}
	if _, ok := decoded["error"]; ok {
		t.Errorf("error field present on a success response: %v", decoded["error"])
	}

	resultField, ok := decoded["result"].(map[string]any)
	if !ok {
		t.Fatalf("result field type = %T, want map[string]any", decoded["result"])
	}
	if _, ok := resultField["resultType"]; ok {
		t.Errorf("legacy result must not carry \"resultType\", got: %v", resultField)
	}
	if _, ok := resultField["ttlMs"]; ok {
		t.Errorf("legacy result must not carry \"ttlMs\", got: %v", resultField)
	}
	if _, ok := resultField["cacheScope"]; ok {
		t.Errorf("legacy result must not carry \"cacheScope\", got: %v", resultField)
	}
	if _, ok := resultField["tools"]; !ok {
		t.Errorf("legacy result must still carry the payload verbatim, got: %v", resultField)
	}
}

// TestLegacyDialect_EncodeResult_NilResult_ReturnsError verifies FIX A's
// defensive rejection of a nil *Result: EncodeResult must return a non-nil
// error rather than silently producing a response with neither "result" nor
// "error" (which is not valid JSON-RPC 2.0). A nil *Result reaching
// EncodeResult is always a dispatch-path programming error — every
// legitimate zero-payload result (e.g. dispatchLegacy's "ping") returns a
// non-nil *Result wrapping an empty payload, never a nil *Result itself.
func TestLegacyDialect_EncodeResult_NilResult_ReturnsError(t *testing.T) {
	t.Parallel()

	d := mcp.NewLegacyDialect(mcp.V20250326)
	out, encErr := d.EncodeResult(json.RawMessage(`1`), nil)
	if encErr == nil {
		t.Fatal("EncodeResult(nil) error = nil, want non-nil")
	}
	if out != nil {
		t.Errorf("EncodeResult(nil) body = %s, want nil alongside a non-nil error", out)
	}
}

// ---- EncodeError --------------------------------------------------------------

func TestLegacyDialect_EncodeError(t *testing.T) {
	t.Parallel()

	d := mcp.NewLegacyDialect(mcp.V20250326)
	out, encErr := d.EncodeError(json.RawMessage(`9`), &mcp.Error{Code: mcp.CodeMethodNotFound, Message: "method not found: bogus"})
	if encErr != nil {
		t.Fatalf("EncodeError() unexpected error: %v", encErr)
	}

	var decoded mcp.Response
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal encoded error: %v", err)
	}
	if decoded.Result != nil {
		t.Errorf("Result = %v, want nil on an error response", decoded.Result)
	}
	if decoded.Error == nil {
		t.Fatal("Error is nil, want non-nil")
	}
	if decoded.Error.Code != mcp.CodeMethodNotFound {
		t.Errorf("Error.Code = %d, want %d", decoded.Error.Code, mcp.CodeMethodNotFound)
	}
	if string(decoded.ID) != "9" {
		t.Errorf("ID = %q, want %q", string(decoded.ID), "9")
	}
}

// ---- Decode → EncodeResult round trip ----------------------------------------

// TestLegacyDialect_RoundTrip_EchoesID verifies that a request ID decoded from
// the wire is echoed unchanged in the encoded result — the invariant that
// makes JSON-RPC response correlation possible.
func TestLegacyDialect_RoundTrip_EchoesID(t *testing.T) {
	t.Parallel()

	d := mcp.NewLegacyDialect(mcp.V20250326)

	env, err := d.Decode([]byte(`{"jsonrpc":"2.0","id":"req-42","method":"ping"}`), mcp.MapHeader{})
	if err != nil {
		t.Fatalf("Decode() error: %+v", err)
	}

	out, encErr := d.EncodeResult(env.ID, &mcp.Result{Payload: map[string]any{}})
	if encErr != nil {
		t.Fatalf("EncodeResult() unexpected error: %v", encErr)
	}

	var decoded mcp.Response
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(decoded.ID) != `"req-42"` {
		t.Errorf("ID = %q, want %q", string(decoded.ID), `"req-42"`)
	}
}
