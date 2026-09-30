package mcp_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// ---- Request parsing --------------------------------------------------------

func TestRequest_Unmarshal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		wantErr    bool
		wantJRPC   string
		wantID     string // string representation of the raw JSON ID field
		wantMethod string
	}{
		{
			name:       "valid request with numeric id",
			raw:        `{"jsonrpc":"2.0","id":1,"method":"ping","params":{}}`,
			wantJRPC:   "2.0",
			wantID:     "1",
			wantMethod: "ping",
		},
		{
			name:       "valid request with string id",
			raw:        `{"jsonrpc":"2.0","id":"abc-123","method":"tools/list"}`,
			wantJRPC:   "2.0",
			wantID:     `"abc-123"`,
			wantMethod: "tools/list",
		},
		{
			name:       "valid notification with null id",
			raw:        `{"jsonrpc":"2.0","id":null,"method":"notifications/initialized"}`,
			wantJRPC:   "2.0",
			wantID:     "null",
			wantMethod: "notifications/initialized",
		},
		{
			name:       "notification without id field",
			raw:        `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			wantJRPC:   "2.0",
			wantID:     "",
			wantMethod: "notifications/initialized",
		},
		{
			name:       "missing jsonrpc field parses without error",
			raw:        `{"id":1,"method":"ping"}`,
			wantJRPC:   "",
			wantID:     "1",
			wantMethod: "ping",
		},
		{
			name:    "invalid JSON returns unmarshal error",
			raw:     `{"jsonrpc":"2.0",`,
			wantErr: true,
		},
		{
			name:    "empty string returns unmarshal error",
			raw:     ``,
			wantErr: true,
		},
		{
			name:    "null document returns no-field struct",
			raw:     `null`,
			wantErr: false, // json.Unmarshal of null into a struct is a no-op
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var req mcp.Request
			err := json.Unmarshal([]byte(tc.raw), &req)

			if tc.wantErr {
				if err == nil {
					t.Errorf("expected unmarshal error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected unmarshal error: %v", err)
			}
			if req.JSONRPC != tc.wantJRPC {
				t.Errorf("JSONRPC = %q, want %q", req.JSONRPC, tc.wantJRPC)
			}
			if string(req.ID) != tc.wantID {
				t.Errorf("ID = %q, want %q", string(req.ID), tc.wantID)
			}
			if req.Method != tc.wantMethod {
				t.Errorf("Method = %q, want %q", req.Method, tc.wantMethod)
			}
		})
	}
}

// ---- IsNotification ---------------------------------------------------------

func TestRequest_IsNotification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "absent ID is notification",
			raw:  `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			want: true,
		},
		{
			name: "null ID is notification",
			raw:  `{"jsonrpc":"2.0","id":null,"method":"notifications/initialized"}`,
			want: true,
		},
		{
			name: "numeric ID is not notification",
			raw:  `{"jsonrpc":"2.0","id":42,"method":"ping"}`,
			want: false,
		},
		{
			name: "string ID is not notification",
			raw:  `{"jsonrpc":"2.0","id":"req-1","method":"ping"}`,
			want: false,
		},
		{
			name: "zero numeric ID is not notification",
			raw:  `{"jsonrpc":"2.0","id":0,"method":"ping"}`,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var req mcp.Request
			if err := json.Unmarshal([]byte(tc.raw), &req); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := req.IsNotification()
			if got != tc.want {
				t.Errorf("IsNotification() = %v, want %v (ID bytes=%q)", got, tc.want, string(req.ID))
			}
		})
	}
}

// ---- TextResult -------------------------------------------------------------

func TestTextResult(t *testing.T) {
	t.Parallel()

	const text = "hello world"
	r := mcp.TextResult(text)

	if r == nil {
		t.Fatal("TextResult returned nil")
	}
	if r.IsError {
		t.Errorf("IsError = true, want false")
	}
	if len(r.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(r.Content))
	}
	if r.Content[0].Type != "text" {
		t.Errorf("Content[0].Type = %q, want %q", r.Content[0].Type, "text")
	}
	if r.Content[0].Text != text {
		t.Errorf("Content[0].Text = %q, want %q", r.Content[0].Text, text)
	}
}

// ---- ErrorResult ------------------------------------------------------------

func TestErrorResult(t *testing.T) {
	t.Parallel()

	const msg = "something went wrong"
	r := mcp.ErrorResult(msg)

	if r == nil {
		t.Fatal("ErrorResult returned nil")
	}
	if !r.IsError {
		t.Errorf("IsError = false, want true")
	}
	if len(r.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(r.Content))
	}
	if r.Content[0].Type != "text" {
		t.Errorf("Content[0].Type = %q, want %q", r.Content[0].Type, "text")
	}
	if r.Content[0].Text != msg {
		t.Errorf("Content[0].Text = %q, want %q", r.Content[0].Text, msg)
	}
}

// ---- NewResponse ------------------------------------------------------------

func TestNewResponse(t *testing.T) {
	t.Parallel()

	id := json.RawMessage(`42`)
	result := map[string]string{"foo": "bar"}
	resp := mcp.NewResponse(id, result)

	if resp.JSONRPC != "2.0" {
		t.Errorf("JSONRPC = %q, want %q", resp.JSONRPC, "2.0")
	}
	if string(resp.ID) != "42" {
		t.Errorf("ID = %q, want %q", string(resp.ID), "42")
	}
	if resp.Error != nil {
		t.Errorf("Error = %+v, want nil", resp.Error)
	}
	got, ok := resp.Result.(map[string]string)
	if !ok || got["foo"] != "bar" {
		t.Errorf("Result = %+v, want map with foo=bar", resp.Result)
	}
}

// ---- NewErrorResponse -------------------------------------------------------

func TestNewErrorResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		id      json.RawMessage
		code    int
		message string
	}{
		{
			name:    "parse error with nil id",
			id:      nil,
			code:    mcp.CodeParseError,
			message: "parse error",
		},
		{
			name:    "method not found with numeric id",
			id:      json.RawMessage(`7`),
			code:    mcp.CodeMethodNotFound,
			message: "method not found: bogus",
		},
		{
			name:    "invalid params with string id",
			id:      json.RawMessage(`"req-xyz"`),
			code:    mcp.CodeInvalidParams,
			message: "invalid params",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp := mcp.NewErrorResponse(tc.id, tc.code, tc.message)

			if resp.JSONRPC != "2.0" {
				t.Errorf("JSONRPC = %q, want %q", resp.JSONRPC, "2.0")
			}
			if string(resp.ID) != string(tc.id) {
				t.Errorf("ID = %q, want %q", string(resp.ID), string(tc.id))
			}
			if resp.Result != nil {
				t.Errorf("Result = %+v, want nil", resp.Result)
			}
			if resp.Error == nil {
				t.Fatal("Error is nil, want non-nil")
			}
			if resp.Error.Code != tc.code {
				t.Errorf("Error.Code = %d, want %d", resp.Error.Code, tc.code)
			}
			if resp.Error.Message != tc.message {
				t.Errorf("Error.Message = %q, want %q", resp.Error.Message, tc.message)
			}
		})
	}
}

// ---- Error code constants ---------------------------------------------------

func TestErrorCodeConstants(t *testing.T) {
	t.Parallel()

	// Verify the JSON-RPC 2.0 spec error codes match expected values.
	codes := map[string]int{
		"CodeParseError":     mcp.CodeParseError,
		"CodeInvalidRequest": mcp.CodeInvalidRequest,
		"CodeMethodNotFound": mcp.CodeMethodNotFound,
		"CodeInvalidParams":  mcp.CodeInvalidParams,
		"CodeInternalError":  mcp.CodeInternalError,
	}
	want := map[string]int{
		"CodeParseError":     -32700,
		"CodeInvalidRequest": -32600,
		"CodeMethodNotFound": -32601,
		"CodeInvalidParams":  -32602,
		"CodeInternalError":  -32603,
	}
	for name, got := range codes {
		if got != want[name] {
			t.Errorf("%s = %d, want %d", name, got, want[name])
		}
	}
}

// ---- JSON round-trips -------------------------------------------------------

func TestRequest_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	original := mcp.Request{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"round-trip-1"`),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"list_models","arguments":{}}`),
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded mcp.Request
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.JSONRPC != original.JSONRPC {
		t.Errorf("JSONRPC mismatch: %q != %q", decoded.JSONRPC, original.JSONRPC)
	}
	if string(decoded.ID) != string(original.ID) {
		t.Errorf("ID mismatch: %q != %q", string(decoded.ID), string(original.ID))
	}
	if decoded.Method != original.Method {
		t.Errorf("Method mismatch: %q != %q", decoded.Method, original.Method)
	}
	if string(decoded.Params) != string(original.Params) {
		t.Errorf("Params mismatch: %q != %q", string(decoded.Params), string(original.Params))
	}
}

func TestResponse_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	original := mcp.Response{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`99`),
		Result:  map[string]any{"status": "ok", "count": float64(3)},
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded mcp.Response
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.JSONRPC != original.JSONRPC {
		t.Errorf("JSONRPC mismatch: %q != %q", decoded.JSONRPC, original.JSONRPC)
	}
	if string(decoded.ID) != string(original.ID) {
		t.Errorf("ID mismatch: %q != %q", string(decoded.ID), string(original.ID))
	}
	if decoded.Error != nil {
		t.Errorf("Error = %+v, want nil", decoded.Error)
	}
}

// TestTool_JSONRoundTrip verifies Tool round-trips through JSON, including its
// InputSchema — now a JSONSchema (raw JSON) built via ObjectSchema instead of
// the removed InputSchema/Property struct pair (SEP-2106, docs/mcp-v2.md
// §8/§11.2). The comparison is semantic (decode into map[string]any) rather
// than byte-for-byte, since JSONSchema's own round-trip fidelity — that it
// marshals as raw JSON, not base64 — is covered by TestJSONSchema_MarshalJSON
// and TestJSONSchema_RoundTrip below; this test only needs to confirm Tool
// carries the schema through unchanged.
func TestTool_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	original := mcp.Tool{
		Name:        "my_tool",
		Description: "Does something useful.",
		InputSchema: mcp.ObjectSchema(map[string]mcp.SchemaProp{
			"query": {Type: "string", Description: "The search query."},
			"limit": {Type: "integer"},
		}, "query"),
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded mcp.Tool
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Name != original.Name {
		t.Errorf("Name mismatch: %q != %q", decoded.Name, original.Name)
	}
	if decoded.Description != original.Description {
		t.Errorf("Description mismatch: %q != %q", decoded.Description, original.Description)
	}

	var wantSchema, gotSchema map[string]any
	if err := json.Unmarshal(original.InputSchema, &wantSchema); err != nil {
		t.Fatalf("unmarshal original schema: %v", err)
	}
	if err := json.Unmarshal(decoded.InputSchema, &gotSchema); err != nil {
		t.Fatalf("unmarshal decoded schema: %v", err)
	}
	if !reflect.DeepEqual(gotSchema, wantSchema) {
		t.Errorf("InputSchema round-trip mismatch:\n got  %+v\n want %+v", gotSchema, wantSchema)
	}
}

func TestToolResult_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		original *mcp.ToolResult
	}{
		{
			name:     "text result",
			original: mcp.TextResult("some output"),
		},
		{
			name:     "error result",
			original: mcp.ErrorResult("something failed"),
		},
		{
			name: "multi-content result",
			original: &mcp.ToolResult{
				Content: []mcp.Content{
					{Type: "text", Text: "first"},
					{Type: "text", Text: "second"},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, err := json.Marshal(tc.original)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			var decoded mcp.ToolResult
			if err := json.Unmarshal(b, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if !reflect.DeepEqual(decoded, *tc.original) {
				t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", decoded, *tc.original)
			}
		})
	}
}

// ---- JSONSchema: raw JSON, NOT base64 ---------------------------------------
//
// JSONSchema is `type JSONSchema jsonx.RawMessage`, i.e. a named []byte type.
// Without an explicit MarshalJSON/UnmarshalJSON pair, encoding/json treats any
// named []byte type as arbitrary binary data and base64-encodes it — this is
// the single most common pitfall of the RawMessage-alias pattern, and exactly
// the bug SEP-2106's "full JSON Schema 2020-12" requirement would reintroduce
// if JSONSchema forgot these methods. These tests lock in that JSONSchema
// marshals as the literal JSON document it holds.

// TestJSONSchema_MarshalJSON_IsRawNotBase64 is the load-bearing regression
// test: it fails loudly (with a visibly wrong base64 string in the error) if
// JSONSchema's MarshalJSON method is ever removed or shadowed.
func TestJSONSchema_MarshalJSON_IsRawNotBase64(t *testing.T) {
	t.Parallel()

	schema := mcp.JSONSchema(`{"type":"string"}`)

	b, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// A base64-encoding bug would produce a quoted string like
	// "eyJ0eXBlIjoic3RyaW5nIn0=" instead of the raw object.
	if string(b) != `{"type":"string"}` {
		t.Errorf("Marshal(JSONSchema) = %s, want raw JSON %s (if this looks like a base64 string, "+
			"JSONSchema's MarshalJSON method has regressed)", b, `{"type":"string"}`)
	}

	var probe map[string]any
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("marshaled JSONSchema is not valid JSON on its own: %v (raw: %s)", err, b)
	}
}

// TestJSONSchema_MarshalJSON_Empty verifies an empty/nil JSONSchema marshals
// to the JSON null literal rather than an empty base64 string or invalid JSON.
func TestJSONSchema_MarshalJSON_Empty(t *testing.T) {
	t.Parallel()

	var schema mcp.JSONSchema
	b, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "null" {
		t.Errorf("Marshal(empty JSONSchema) = %s, want %s", b, "null")
	}
}

// TestJSONSchema_RoundTrip_NestedSchema verifies a JSON Schema 2020-12
// document with enum, nested properties, and $ref survives a full
// marshal/unmarshal round trip byte-for-byte-equivalent (semantically), the
// exact vocabulary SEP-2106 requires VoidLLM to carry through without
// dropping keywords.
func TestJSONSchema_RoundTrip_NestedSchema(t *testing.T) {
	t.Parallel()

	const original = `{
		"type": "object",
		"properties": {
			"status": {
				"type": "string",
				"enum": ["pending", "active", "done"]
			},
			"owner": {
				"type": "object",
				"properties": {
					"id": {"type": "string"},
					"role": {"$ref": "#/definitions/role"}
				},
				"required": ["id"]
			}
		},
		"required": ["status"],
		"definitions": {
			"role": {"type": "string", "enum": ["admin", "member"]}
		}
	}`

	schema := mcp.JSONSchema(original)

	// Round-trip through a Tool, exactly as VoidLLM actually uses JSONSchema.
	tool := mcp.Tool{Name: "assign_role", InputSchema: schema}

	b, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal Tool: %v", err)
	}

	var decodedTool mcp.Tool
	if err := json.Unmarshal(b, &decodedTool); err != nil {
		t.Fatalf("unmarshal Tool: %v", err)
	}

	var want, got map[string]any
	if err := json.Unmarshal([]byte(original), &want); err != nil {
		t.Fatalf("unmarshal original: %v", err)
	}
	if err := json.Unmarshal(decodedTool.InputSchema, &got); err != nil {
		t.Fatalf("unmarshal decoded schema: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nested schema round-trip mismatch:\n got  %+v\n want %+v", got, want)
	}

	// Spot-check the keywords SEP-2106 specifically calls out as needing full
	// fidelity: enum, nested properties, and $ref must all still be present.
	props := got["properties"].(map[string]any)
	status := props["status"].(map[string]any)
	if enum, ok := status["enum"].([]any); !ok || len(enum) != 3 {
		t.Errorf("enum keyword lost or malformed after round-trip: %v", status["enum"])
	}
	owner := props["owner"].(map[string]any)
	ownerProps := owner["properties"].(map[string]any)
	role := ownerProps["role"].(map[string]any)
	if role["$ref"] != "#/definitions/role" {
		t.Errorf("$ref keyword lost after round-trip: %v", role["$ref"])
	}
}

// TestJSONSchema_UnmarshalJSON_NilReceiver verifies UnmarshalJSON guards
// against a nil pointer receiver, mirroring encoding/json.RawMessage's own
// guard, instead of panicking.
func TestJSONSchema_UnmarshalJSON_NilReceiver(t *testing.T) {
	t.Parallel()

	var s *mcp.JSONSchema
	err := s.UnmarshalJSON([]byte(`{}`))
	if err == nil {
		t.Error("UnmarshalJSON on nil receiver: error = nil, want non-nil")
	}
}

// ---- Capabilities: raw JSON, NOT base64 --------------------------------------

// TestCapabilities_MarshalJSON_IsRawNotBase64 mirrors
// TestJSONSchema_MarshalJSON_IsRawNotBase64 for Capabilities, which is the
// same jsonx.RawMessage-alias pattern used to carry client/server capability
// objects — including the open-ended "extensions" key (docs/mcp-v2.md §6) —
// verbatim without VoidLLM needing to model every SDK's capability shape.
func TestCapabilities_MarshalJSON_IsRawNotBase64(t *testing.T) {
	t.Parallel()

	caps := mcp.Capabilities(`{"tools":{},"extensions":{"io.modelcontextprotocol/tasks":{}}}`)

	b, err := json.Marshal(caps)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != string(caps) {
		t.Errorf("Marshal(Capabilities) = %s, want raw JSON %s (if this looks like a base64 string, "+
			"Capabilities' MarshalJSON method has regressed)", b, caps)
	}
}

// TestCapabilities_MarshalJSON_Empty verifies an empty/nil Capabilities
// marshals to the JSON null literal.
func TestCapabilities_MarshalJSON_Empty(t *testing.T) {
	t.Parallel()

	var caps mcp.Capabilities
	b, err := json.Marshal(caps)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "null" {
		t.Errorf("Marshal(empty Capabilities) = %s, want %s", b, "null")
	}
}

// TestCapabilities_RoundTrip verifies Capabilities survives a full
// marshal/unmarshal round trip, including a nested "extensions" object —
// exactly what dialect2026.Decode extracts from
// params._meta["io.modelcontextprotocol/clientCapabilities"] into
// Envelope.ClientCaps.
func TestCapabilities_RoundTrip(t *testing.T) {
	t.Parallel()

	const original = `{"tools":{"listChanged":true},"extensions":{"io.modelcontextprotocol/tasks":{}}}`

	type wrapper struct {
		Caps mcp.Capabilities `json:"caps"`
	}
	w := wrapper{Caps: mcp.Capabilities(original)}

	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded wrapper
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var want, got map[string]any
	if err := json.Unmarshal([]byte(original), &want); err != nil {
		t.Fatalf("unmarshal original: %v", err)
	}
	if err := json.Unmarshal(decoded.Caps, &got); err != nil {
		t.Fatalf("unmarshal decoded: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Capabilities round-trip mismatch:\n got  %+v\n want %+v", got, want)
	}
}

// TestCapabilities_UnmarshalJSON_NilReceiver verifies UnmarshalJSON guards
// against a nil pointer receiver instead of panicking.
func TestCapabilities_UnmarshalJSON_NilReceiver(t *testing.T) {
	t.Parallel()

	var c *mcp.Capabilities
	err := c.UnmarshalJSON([]byte(`{}`))
	if err == nil {
		t.Error("UnmarshalJSON on nil receiver: error = nil, want non-nil")
	}
}
