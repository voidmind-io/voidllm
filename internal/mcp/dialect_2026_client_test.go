package mcp_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// ---- Warmup: the defining property of the stateless core -------------------

// panicRoundTripper is an mcp.RoundTripper whose RoundTrip fails the test
// outright if it is ever invoked — used to prove the modern dialect's
// Warmup performs NO I/O at all (docs/mcp-v2.md §2, §3.2), rather than
// merely asserting "no error" (which a no-op AND a successful real call
// would both satisfy).
type panicRoundTripper struct{ t *testing.T }

func (p panicRoundTripper) RoundTrip(_ context.Context, raw []byte, _ mcp.MapHeader) (*mcp.RoundTripResult, error) {
	p.t.Fatalf("dialect2026Client.Warmup performed network I/O it must not perform; request: %s", raw)
	return nil, nil
}

func TestDialect2026Client_Warmup_PerformsNoIO(t *testing.T) {
	t.Parallel()

	d := mcp.NewDialect2026Client(mcp.V20260728, mcp.ClientInfo{Name: "voidllm-test", Version: "1.0"})
	st := &mcp.UpstreamState{}

	if err := d.Warmup(context.Background(), panicRoundTripper{t: t}, st); err != nil {
		t.Fatalf("Warmup() error = %v, want nil", err)
	}
	if got := st.SessionID(); got != "" {
		t.Errorf("SessionID() = %q, want empty — the modern era has no session concept", got)
	}
}

// TestDialect2026Client_Warmup_NilRoundTripperAndState further confirms no
// I/O and no state mutation occur: passing nil for both rt and st must not
// panic, since a genuine no-op never dereferences either.
func TestDialect2026Client_Warmup_NilRoundTripperAndState(t *testing.T) {
	t.Parallel()

	d := mcp.NewDialect2026Client(mcp.V20260728, mcp.ClientInfo{})
	if err := d.Warmup(context.Background(), nil, nil); err != nil {
		t.Fatalf("Warmup(nil, nil) error = %v, want nil", err)
	}
}

// ---- Prepare: standard request headers + _meta injection -------------------

// metaValue extracts params._meta[key] from a prepared modern-era request
// body as a string.
func metaValue(t *testing.T, prepared []byte, key string) string {
	t.Helper()
	var req struct {
		Params struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(prepared, &req); err != nil {
		t.Fatalf("unmarshal prepared body: %v; body: %s", err, prepared)
	}
	raw, ok := req.Params.Meta[key]
	if !ok {
		t.Fatalf("params._meta[%q] missing entirely; body: %s", key, prepared)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("params._meta[%q] is not a JSON string: %s", key, raw)
	}
	return s
}

func TestDialect2026Client_Prepare_InjectsMeta(t *testing.T) {
	t.Parallel()

	clientInfo := mcp.ClientInfo{Name: "voidllm", Version: "0.1.0"}
	d := mcp.NewDialect2026Client(mcp.V20260728, clientInfo)

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	prepared, hdr, err := d.Prepare(&mcp.CallRequest{Raw: raw}, &mcp.UpstreamState{})
	if err != nil {
		t.Fatalf("Prepare() error = %v, want nil", err)
	}

	if got := metaValue(t, prepared, "io.modelcontextprotocol/protocolVersion"); got != string(mcp.V20260728) {
		t.Errorf("_meta.protocolVersion = %q, want %q", got, mcp.V20260728)
	}

	var req struct {
		Params struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(prepared, &req); err != nil {
		t.Fatalf("unmarshal prepared body: %v", err)
	}
	capsRaw, ok := req.Params.Meta["io.modelcontextprotocol/clientCapabilities"]
	if !ok {
		t.Fatalf("_meta.clientCapabilities missing; body: %s", prepared)
	}
	var caps map[string]any
	if err := json.Unmarshal(capsRaw, &caps); err != nil {
		t.Fatalf("_meta.clientCapabilities is not a JSON object: %s", capsRaw)
	}

	var ci mcp.ClientInfo
	infoRaw, ok := req.Params.Meta["io.modelcontextprotocol/clientInfo"]
	if !ok {
		t.Fatalf("_meta.clientInfo missing; body: %s", prepared)
	}
	if err := json.Unmarshal(infoRaw, &ci); err != nil {
		t.Fatalf("unmarshal _meta.clientInfo: %v", err)
	}
	if ci != clientInfo {
		t.Errorf("_meta.clientInfo = %+v, want %+v", ci, clientInfo)
	}

	// MCP-Protocol-Version is a standard request header on every POST.
	if got := hdr[mcp.HeaderProtocolVersion]; got != string(mcp.V20260728) {
		t.Errorf("%s header = %q, want %q", mcp.HeaderProtocolVersion, got, mcp.V20260728)
	}
}

// TestDialect2026Client_Prepare_PreservesExistingParams verifies Prepare
// merges _meta into an existing params object rather than clobbering the
// caller's own fields.
func TestDialect2026Client_Prepare_PreservesExistingParams(t *testing.T) {
	t.Parallel()

	d := mcp.NewDialect2026Client(mcp.V20260728, mcp.ClientInfo{})
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`)

	prepared, _, err := d.Prepare(&mcp.CallRequest{Raw: raw}, &mcp.UpstreamState{})
	if err != nil {
		t.Fatalf("Prepare() error = %v, want nil", err)
	}

	var req struct {
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(prepared, &req); err != nil {
		t.Fatalf("unmarshal prepared body: %v; body: %s", err, prepared)
	}
	if req.Params.Name != "echo" {
		t.Errorf("params.name = %q, want %q (caller's field must survive _meta injection)", req.Params.Name, "echo")
	}
	if req.Params.Arguments["text"] != "hi" {
		t.Errorf("params.arguments = %v, want {text: hi}", req.Params.Arguments)
	}
}

// TestDialect2026Client_Prepare_MergesMeta is the regression test for the
// fixed Prepare bug: it used to overwrite the caller's entire params._meta
// object outright. VoidLLM is a gateway, not the origin of the request, so
// silently discarding caller metadata would corrupt it — only the three keys
// VoidLLM itself owns (protocolVersion, clientCapabilities, clientInfo) may
// be set or overwritten; every other key must survive untouched, including
// one that collides with a key VoidLLM owns (protocolVersion), where OUR
// value must win because VoidLLM, not the caller, determines the era it
// negotiates with the upstream in.
func TestDialect2026Client_Prepare_MergesMeta(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		raw   string
		check func(t *testing.T, meta map[string]json.RawMessage)
	}{
		{
			name: "caller's progressToken survives alongside our three required keys",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","_meta":{"progressToken":"abc-123"}}}`,
			check: func(t *testing.T, meta map[string]json.RawMessage) {
				t.Helper()
				var token string
				if err := json.Unmarshal(meta["progressToken"], &token); err != nil {
					t.Fatalf("progressToken missing or not a string: %v; meta: %v", err, meta)
				}
				if token != "abc-123" {
					t.Errorf("progressToken = %q, want %q", token, "abc-123")
				}
				for _, key := range []string{
					"io.modelcontextprotocol/protocolVersion",
					"io.modelcontextprotocol/clientCapabilities",
					"io.modelcontextprotocol/clientInfo",
				} {
					if _, ok := meta[key]; !ok {
						t.Errorf("our required _meta key %q is missing after merging in the caller's progressToken", key)
					}
				}
			},
		},
		{
			name: "caller's own io.modelcontextprotocol/protocolVersion is overwritten by ours — VoidLLM determines the era, not the caller",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"1900-01-01"}}}`,
			check: func(t *testing.T, meta map[string]json.RawMessage) {
				t.Helper()
				var v string
				if err := json.Unmarshal(meta["io.modelcontextprotocol/protocolVersion"], &v); err != nil {
					t.Fatalf("protocolVersion missing or malformed: %v", err)
				}
				if v != string(mcp.V20260728) {
					t.Errorf("protocolVersion = %q, want ours (%q) to win over the caller-supplied value", v, mcp.V20260728)
				}
			},
		},
		{
			name: "no _meta present at all: one is created carrying our three required keys",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			check: func(t *testing.T, meta map[string]json.RawMessage) {
				t.Helper()
				for _, key := range []string{
					"io.modelcontextprotocol/protocolVersion",
					"io.modelcontextprotocol/clientCapabilities",
					"io.modelcontextprotocol/clientInfo",
				} {
					if _, ok := meta[key]; !ok {
						t.Errorf("_meta[%q] missing when no _meta existed on the inbound request", key)
					}
				}
			},
		},
		{
			name: "a foreign key holding a nested object survives byte-for-byte equivalent, unchanged",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"com.example/custom":{"nested":{"a":1,"b":[1,2,3]}}}}}`,
			check: func(t *testing.T, meta map[string]json.RawMessage) {
				t.Helper()
				var got, want any
				if err := json.Unmarshal(meta["com.example/custom"], &got); err != nil {
					t.Fatalf("com.example/custom missing or malformed: %v", err)
				}
				if err := json.Unmarshal([]byte(`{"nested":{"a":1,"b":[1,2,3]}}`), &want); err != nil {
					t.Fatalf("test setup: %v", err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("com.example/custom = %v, want %v (unchanged)", got, want)
				}
			},
		},
		{
			name: "a foreign key holding an array survives unchanged",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"com.example/tags":["a","b","c"]}}}`,
			check: func(t *testing.T, meta map[string]json.RawMessage) {
				t.Helper()
				var tags []string
				if err := json.Unmarshal(meta["com.example/tags"], &tags); err != nil {
					t.Fatalf("com.example/tags missing or malformed: %v", err)
				}
				want := []string{"a", "b", "c"}
				if !reflect.DeepEqual(tags, want) {
					t.Errorf("com.example/tags = %v, want %v", tags, want)
				}
			},
		},
	}

	d := mcp.NewDialect2026Client(mcp.V20260728, mcp.ClientInfo{Name: "voidllm", Version: "0.1.0"})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			prepared, _, err := d.Prepare(&mcp.CallRequest{Raw: []byte(tc.raw)}, &mcp.UpstreamState{})
			if err != nil {
				t.Fatalf("Prepare() error = %v, want nil", err)
			}

			var req struct {
				Params struct {
					Meta map[string]json.RawMessage `json:"_meta"`
				} `json:"params"`
			}
			if err := json.Unmarshal(prepared, &req); err != nil {
				t.Fatalf("unmarshal prepared body: %v; body: %s", err, prepared)
			}
			tc.check(t, req.Params.Meta)
		})
	}
}

// TestDialect2026Client_Prepare_StandardHeadersTable verifies the standard
// request headers (MCP Streamable HTTP §4.2, docs/mcp-v2.md §4.2):
// MCP-Protocol-Version and Mcp-Method on every request, and Mcp-Name added
// ONLY for tools/call, resources/read, and prompts/get, sourced from
// params.name (tools/call, prompts/get) or params.uri (resources/read).
//
// Every case asserts the header value, once decoded via
// mcp.DecodeHeaderValue, is EXACTLY the corresponding body field — this is
// the property MCP Streamable HTTP §4.5 server-side validation enforces
// (validateMCPHeaders in internal/api/admin/mcp_handler.go, which rejects
// any mismatch with CodeHeaderMismatch), so Prepare must never construct a
// request a conformant server would reject.
func TestDialect2026Client_Prepare_StandardHeadersTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		raw         string
		wantName    bool
		wantNameVal string
	}{
		{
			name:     "tools/list carries no Mcp-Name",
			raw:      `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			wantName: false,
		},
		{
			name:     "server/discover carries no Mcp-Name",
			raw:      `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`,
			wantName: false,
		},
		{
			name:        "tools/call carries Mcp-Name from params.name",
			raw:         `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_weather","arguments":{}}}`,
			wantName:    true,
			wantNameVal: "get_weather",
		},
		{
			name:        "prompts/get carries Mcp-Name from params.name",
			raw:         `{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":{"name":"summarize"}}`,
			wantName:    true,
			wantNameVal: "summarize",
		},
		{
			name:        "resources/read carries Mcp-Name from params.uri",
			raw:         `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"file:///tmp/notes.txt"}}`,
			wantName:    true,
			wantNameVal: "file:///tmp/notes.txt",
		},
		{
			name:        "a non-ASCII tool name is base64-sentinel-encoded in the header but matches the decoded body value",
			raw:         `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"café_söker","arguments":{}}}`,
			wantName:    true,
			wantNameVal: "café_söker",
		},
	}

	d := mcp.NewDialect2026Client(mcp.V20260728, mcp.ClientInfo{Name: "voidllm-test", Version: "1.0"})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			prepared, hdr, err := d.Prepare(&mcp.CallRequest{Raw: []byte(tc.raw)}, &mcp.UpstreamState{})
			if err != nil {
				t.Fatalf("Prepare() error = %v, want nil", err)
			}

			var req struct {
				Method string `json:"method"`
			}
			if err := json.Unmarshal(prepared, &req); err != nil {
				t.Fatalf("unmarshal prepared body: %v", err)
			}

			// Mcp-Method must always be present and, once decoded, match the
			// body's method EXACTLY — the same property validateMCPHeaders
			// checks server-side.
			gotMethodHdr, ok := hdr[mcp.HeaderMethod]
			if !ok {
				t.Fatalf("%s header missing", mcp.HeaderMethod)
			}
			decodedMethod, ok := mcp.DecodeHeaderValue(gotMethodHdr)
			if !ok {
				t.Fatalf("%s header %q is not validly encoded", mcp.HeaderMethod, gotMethodHdr)
			}
			if decodedMethod != req.Method {
				t.Errorf("%s header decodes to %q, want it to exactly match body method %q", mcp.HeaderMethod, decodedMethod, req.Method)
			}

			gotNameHdr, hasName := hdr[mcp.HeaderName]
			if hasName != tc.wantName {
				t.Fatalf("%s header present = %v, want %v (headers: %v)", mcp.HeaderName, hasName, tc.wantName, hdr)
			}
			if !tc.wantName {
				return
			}
			decodedName, ok := mcp.DecodeHeaderValue(gotNameHdr)
			if !ok {
				t.Fatalf("%s header %q is not validly encoded", mcp.HeaderName, gotNameHdr)
			}
			if decodedName != tc.wantNameVal {
				t.Errorf("%s header decodes to %q, want %q", mcp.HeaderName, decodedName, tc.wantNameVal)
			}

			// The body's own params.name/uri must be untouched by encoding —
			// only the HEADER is sentinel-encoded, never the JSON body.
			var params struct {
				Name string `json:"name"`
				URI  string `json:"uri"`
			}
			_ = json.Unmarshal(mustParamsRaw(t, prepared), &params)
			bodyVal := params.Name
			if params.URI != "" {
				bodyVal = params.URI
			}
			if bodyVal != tc.wantNameVal {
				t.Errorf("body target value = %q, want %q (body itself must never be sentinel-encoded)", bodyVal, tc.wantNameVal)
			}
		})
	}
}

// mustParamsRaw extracts the raw params object from a prepared request body.
func mustParamsRaw(t *testing.T, prepared []byte) json.RawMessage {
	t.Helper()
	var top struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(prepared, &top); err != nil {
		t.Fatalf("unmarshal prepared body: %v", err)
	}
	return top.Params
}

// ---- Parse: resultType handling, MRTR deferral ------------------------------

func TestDialect2026Client_Parse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		raw         string
		wantErrCode int // 0 means no error expected
	}{
		{
			name: "resultType complete passes through",
			raw:  `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","tools":[]}}`,
		},
		{
			name: "missing resultType is treated as complete (a legacy-shaped result reaching a modern dialect)",
			raw:  `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`,
		},
		{
			name:        "resultType input_required is a distinct, named error — MRTR is not yet implemented",
			raw:         `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","inputRequests":{}}}`,
			wantErrCode: mcp.CodeMissingRequiredClientCapability,
		},
		{
			name: "an ordinary wire-level JSON-RPC error is NOT intercepted by Parse — it is valid content, not a transport failure",
			raw:  `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`,
		},
		{
			name: "a result-less response (neither result nor error) is not itself a Parse error",
			raw:  `{"jsonrpc":"2.0","id":1}`,
		},
		{
			name:        "malformed JSON is a parse error",
			raw:         `{not valid json`,
			wantErrCode: mcp.CodeParseError,
		},
		{
			name:        "a non-object result is a parse error when inspecting resultType",
			raw:         `{"jsonrpc":"2.0","id":1,"result":"not an object"}`,
			wantErrCode: mcp.CodeParseError,
		},
	}

	d := mcp.NewDialect2026Client(mcp.V20260728, mcp.ClientInfo{})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result, err := d.Parse([]byte(tc.raw))
			if tc.wantErrCode != 0 {
				if err == nil {
					t.Fatalf("Parse() error = nil, want code %d", tc.wantErrCode)
				}
				if err.Code != tc.wantErrCode {
					t.Errorf("Parse() error code = %d, want %d", err.Code, tc.wantErrCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() unexpected error: %+v", err)
			}
			if result == nil {
				t.Fatal("Parse() result = nil, want non-nil")
			}
		})
	}
}
