package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// ---- Version -------------------------------------------------------------

func TestDialect2026_Version(t *testing.T) {
	t.Parallel()

	d := mcp.NewDialect2026(mcp.V20260728, "voidllm", "0.1.0")
	if got := d.Version(); got != mcp.V20260728 {
		t.Errorf("Version() = %q, want %q", got, mcp.V20260728)
	}
}

// ---- Decode: _meta keys land in the Envelope ---------------------------------

// TestDialect2026_Decode_MetaFields verifies that every params._meta key the
// 2026-07-28 revision defines for per-request identity and capabilities
// (io.modelcontextprotocol/protocolVersion, clientCapabilities, clientInfo,
// logLevel) lands in the corresponding Envelope field (docs/mcp-v2.md §3.2).
func TestDialect2026_Decode_MetaFields(t *testing.T) {
	t.Parallel()

	raw := `{
		"jsonrpc": "2.0",
		"id": "discover-1",
		"method": "server/discover",
		"params": {
			"_meta": {
				"io.modelcontextprotocol/protocolVersion": "2026-07-28",
				"io.modelcontextprotocol/clientCapabilities": {"extensions": {"io.modelcontextprotocol/tasks": {}}},
				"io.modelcontextprotocol/clientInfo": {"name": "ExampleClient", "version": "1.0.0"},
				"io.modelcontextprotocol/logLevel": "debug"
			}
		}
	}`

	d := mcp.NewDialect2026(mcp.V20260728, "voidllm", "0.1.0")
	env, err := d.Decode([]byte(raw), mcp.MapHeader{})
	if err != nil {
		t.Fatalf("Decode() unexpected error: %+v", err)
	}

	if env.Method != "server/discover" {
		t.Errorf("Method = %q, want %q", env.Method, "server/discover")
	}
	if env.Version != mcp.V20260728 {
		t.Errorf("Version = %q, want %q", env.Version, mcp.V20260728)
	}
	if env.ClientInfo.Name != "ExampleClient" || env.ClientInfo.Version != "1.0.0" {
		t.Errorf("ClientInfo = %+v, want {ExampleClient 1.0.0}", env.ClientInfo)
	}
	if env.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", env.LogLevel, "debug")
	}

	var caps map[string]any
	if err := json.Unmarshal(env.ClientCaps, &caps); err != nil {
		t.Fatalf("unmarshal ClientCaps: %v", err)
	}
	extensions, _ := caps["extensions"].(map[string]any)
	if _, ok := extensions["io.modelcontextprotocol/tasks"]; !ok {
		t.Errorf("ClientCaps.extensions missing io.modelcontextprotocol/tasks, got: %v", caps)
	}
}

// TestDialect2026_Decode_MissingMeta_Rejected verifies the 2026-07-28
// revision's mandatory-field enforcement (docs/mcp-v2.md §3.2, quoted
// verbatim on dialect2026.Decode's doc): a request whose params carry no
// _meta object at all is missing BOTH MUST fields — protocolVersion and
// clientCapabilities — and must be rejected with CodeInvalidParams, whose
// Error.Hint is explicitly HintBadRequest so an HTTP-aware caller reports
// HTTP 400, not the ordinary HTTP 200 CodeInvalidParams convention (see
// hintForError/statusHintFor).
//
// SPEC CHANGE: this test used to be named
// TestDialect2026_Decode_MissingMeta_Defaults and asserted the OPPOSITE — that
// an absent _meta was tolerated by defaulting to the dialect's negotiated
// version and empty capabilities. That tolerant behavior is no longer
// spec-compliant: a fabricated empty clientCapabilities would claim a
// declaration the client never actually sent, making "the client declared no
// extensions" indistinguishable from "the client declared nothing at all" —
// exactly the ambiguity RequireExtensions's -32021 check depends on being
// able to tell apart (see dialect2026.Decode's doc and missingRequiredMetaError).
func TestDialect2026_Decode_MissingMeta_Rejected(t *testing.T) {
	t.Parallel()

	d := mcp.NewDialect2026(mcp.V20260728, "voidllm", "0.1.0")
	_, err := d.Decode([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), modernHeader())
	if err == nil {
		t.Fatal("Decode() error = nil, want CodeInvalidParams for a request with no params._meta at all")
	}
	if err.Code != mcp.CodeInvalidParams {
		t.Errorf("Decode() error code = %d, want %d (CodeInvalidParams)", err.Code, mcp.CodeInvalidParams)
	}
	if err.Hint != mcp.HintBadRequest {
		t.Errorf("Decode() error Hint = %v, want HintBadRequest (docs/mcp-v2.md §3.2 requires HTTP 400 for this violation)", err.Hint)
	}
	for _, want := range []string{"io.modelcontextprotocol/protocolVersion", "io.modelcontextprotocol/clientCapabilities"} {
		if !strings.Contains(err.Message, want) {
			t.Errorf("Decode() error message = %q, want it to name the missing field %q", err.Message, want)
		}
	}
}

// modernMetaObject returns a map for params._meta satisfying both MUST
// fields the 2026-07-28 revision requires (docs/mcp-v2.md §3.2):
// io.modelcontextprotocol/protocolVersion and
// io.modelcontextprotocol/clientCapabilities. overrides replaces or removes
// individual keys — a nil value in overrides removes the key entirely —
// letting a test exercise one MUST field's own validation in isolation
// without ALSO tripping the OTHER MUST field's separate "is it present at
// all" check (missingRequiredMetaError in dialect_2026.go). Values in
// overrides may be json.RawMessage to construct deliberately mistyped
// fields, since json.Marshal renders a RawMessage verbatim.
func modernMetaObject(overrides map[string]any) map[string]any {
	meta := map[string]any{
		"io.modelcontextprotocol/protocolVersion":    string(mcp.V20260728),
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	for k, v := range overrides {
		if v == nil {
			delete(meta, k)
			continue
		}
		meta[k] = v
	}
	return meta
}

// modernRequestBody builds a JSON-RPC request body for method whose params
// merge extraParams with a valid params._meta (see modernMetaObject),
// letting metaOverrides replace or remove individual _meta keys. This is the
// shared fixture every modern-era test in this package uses to avoid
// repeating the two MUST _meta fields by hand — see docs/mcp-v2.md §3.2.
func modernRequestBody(id int, method string, extraParams map[string]any, metaOverrides map[string]any) string {
	params := map[string]any{}
	for k, v := range extraParams {
		params[k] = v
	}
	params["_meta"] = modernMetaObject(metaOverrides)

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	b, err := json.Marshal(req)
	if err != nil {
		panic(err) // test helper: inputs are always static, JSON-marshalable test data
	}
	return string(b)
}

// TestDialect2026_Decode_ToolsCallExtractsName verifies params.name is read
// for tools/call the same way the legacy dialect reads it — Name comes from
// the body, not from _meta.
func TestDialect2026_Decode_ToolsCallExtractsName(t *testing.T) {
	t.Parallel()

	d := mcp.NewDialect2026(mcp.V20260728, "voidllm", "0.1.0")
	raw := modernRequestBody(1, "tools/call", map[string]any{"name": "get_weather", "arguments": map[string]any{}}, nil)
	env, err := d.Decode([]byte(raw), modernHeader())
	if err != nil {
		t.Fatalf("Decode() unexpected error: %+v", err)
	}
	if env.Name != "get_weather" {
		t.Errorf("Name = %q, want %q", env.Name, "get_weather")
	}
}

func TestDialect2026_Decode_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		raw      string
		wantCode int
	}{
		{"invalid JSON", `{not valid`, mcp.CodeParseError},
		{"wrong jsonrpc version", `{"jsonrpc":"1.0","id":1,"method":"tools/list"}`, mcp.CodeInvalidRequest},
	}

	d := mcp.NewDialect2026(mcp.V20260728, "voidllm", "0.1.0")

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

// TestDialect2026_Decode_MalformedMetaFields verifies FIX 7: a params._meta
// key that IS present but wrongly typed is rejected with CodeInvalidParams —
// as opposed to a key that is entirely ABSENT, which is now its own, separate
// rejection (missing a MUST field — see
// TestDialect2026_Decode_MissingMeta_Rejected), not a malformed-field one. The
// error message must name the offending _meta key so the caller can fix its
// request, but must NEVER echo the malformed value back — per VoidLLM's
// zero-knowledge logging rule, which extends to error messages returned to
// the caller (see metaTypeError's doc in dialect_2026.go).
//
// Every malformed-field case below uses metaFieldRequest, which — since
// dialect2026.Decode now requires BOTH protocolVersion and clientCapabilities
// to be present (docs/mcp-v2.md §3.2) — sets the field under test to the
// given (deliberately wrong) raw value while leaving the OTHER MUST field at
// a valid default (see modernMetaObject). Without that, every case here would
// trip the separate "other required field is missing" check before Decode
// ever got to examine the malformed field this test is actually about.
func TestDialect2026_Decode_MalformedMetaFields(t *testing.T) {
	t.Parallel()

	// A value distinctive enough that its presence in an error message could
	// only mean it was echoed back verbatim.
	const secretValue = "super-secret-should-never-be-echoed-back"

	tests := []struct {
		name      string
		raw       string
		wantErr   bool
		wantField string
	}{
		{
			name:      "clientCapabilities as a JSON string",
			raw:       metaFieldRequest("io.modelcontextprotocol/clientCapabilities", fmt.Sprintf("%q", secretValue)),
			wantErr:   true,
			wantField: "io.modelcontextprotocol/clientCapabilities",
		},
		{
			name:      "clientCapabilities as a JSON number",
			raw:       metaFieldRequest("io.modelcontextprotocol/clientCapabilities", "42"),
			wantErr:   true,
			wantField: "io.modelcontextprotocol/clientCapabilities",
		},
		{
			name:      "clientCapabilities as a JSON array",
			raw:       metaFieldRequest("io.modelcontextprotocol/clientCapabilities", `["a","b"]`),
			wantErr:   true,
			wantField: "io.modelcontextprotocol/clientCapabilities",
		},
		{
			// FIX B: json.Unmarshal("null", &map) succeeds with a nil map and
			// no error, so isJSONObject must special-case null explicitly —
			// otherwise "clientCapabilities": null would be silently accepted
			// as a (vacuously empty) object instead of rejected as the wrong
			// type it is.
			name:      "clientCapabilities as JSON null",
			raw:       metaFieldRequest("io.modelcontextprotocol/clientCapabilities", "null"),
			wantErr:   true,
			wantField: "io.modelcontextprotocol/clientCapabilities",
		},
		{
			name:      "protocolVersion as a JSON number",
			raw:       metaFieldRequest("io.modelcontextprotocol/protocolVersion", "20260728"),
			wantErr:   true,
			wantField: "io.modelcontextprotocol/protocolVersion",
		},
		{
			name:      "protocolVersion as a JSON object",
			raw:       metaFieldRequest("io.modelcontextprotocol/protocolVersion", fmt.Sprintf(`{"v":%q}`, secretValue)),
			wantErr:   true,
			wantField: "io.modelcontextprotocol/protocolVersion",
		},
		{
			name:      "protocolVersion as a JSON boolean",
			raw:       metaFieldRequest("io.modelcontextprotocol/protocolVersion", "true"),
			wantErr:   true,
			wantField: "io.modelcontextprotocol/protocolVersion",
		},
		{
			// SPEC CHANGE: this case used to be named "field absent entirely
			// remains tolerant, no error" and asserted wantErr: false. An
			// empty _meta object satisfies NEITHER MUST field, so it is
			// rejected the same as _meta being absent entirely (see
			// TestDialect2026_Decode_MissingMeta_Rejected) — this is the
			// missing-required-fields check, not a malformed-single-field
			// one, so no single wantField is asserted here.
			name:    "_meta present but empty is rejected: satisfies neither MUST field (spec change)",
			raw:     `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{}}}`,
			wantErr: true,
		},
	}

	d := mcp.NewDialect2026(mcp.V20260728, "voidllm", "0.1.0")

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := d.Decode([]byte(tc.raw), mcp.MapHeader{})

			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Decode() unexpected error: %+v", err)
				}
				return
			}

			if err == nil {
				t.Fatal("Decode() error = nil, want CodeInvalidParams")
			}
			if err.Code != mcp.CodeInvalidParams {
				t.Errorf("Decode() error code = %d, want %d (CodeInvalidParams)", err.Code, mcp.CodeInvalidParams)
			}
			if !strings.Contains(err.Message, tc.wantField) {
				t.Errorf("error message = %q, want it to name the malformed field %q", err.Message, tc.wantField)
			}
			if strings.Contains(err.Message, secretValue) {
				t.Errorf("error message leaked the malformed field's VALUE, want only the field NAME: %q", err.Message)
			}
		})
	}
}

// TestDialect2026_Decode_ParamsShape verifies FIX C: Decode must distinguish
// several different shapes of params rather than silently swallowing every
// unmarshal error into "no _meta" the way it did before the fix.
//   - params absent entirely, params an object with no _meta key at all
//     (including the empty object {}), and params._meta present as an empty
//     object: all three now satisfy NEITHER MUST _meta field (docs/mcp-v2.md
//     §3.2) and are REJECTED with CodeInvalidParams — this is a SPEC CHANGE
//     from these cases' previous, tolerant behavior (see
//     TestDialect2026_Decode_MissingMeta_Rejected, which pins down the same
//     rejection for the plainest of these shapes).
//   - params is an object whose _meta key IS present but is not itself an
//     object (string/array/number/explicit JSON null): CodeInvalidParams,
//     naming "_meta". An explicit "_meta": null is deliberately its own row,
//     not folded into "absent": distinguishing "key missing" from "key
//     present but null" is the exact distinction this dialect's Decode must
//     make (see paramsMetaIsExplicitNull in dialect_2026.go), and confusing
//     the two was the failure mode this fix targets. This is unaffected by
//     the spec change above: a wrongly-typed _meta is rejected before Decode
//     ever reaches the MUST-field presence check (see dialect2026.Decode).
//   - params is present but is not a JSON object at all (string/array/
//     number): CodeInvalidParams, since JSON-RPC 2.0 requires params, when
//     present, to be a structured value. Also unaffected by the spec change,
//     for the same reason.
//
// Every case sends the MCP-Protocol-Version header dialect2026 negotiates
// under, even though Decode itself does not consult hdr (see its doc
// comment) — matching what a real request on this dialect would carry.
func TestDialect2026_Decode_ParamsShape(t *testing.T) {
	t.Parallel()

	// A value distinctive enough that its presence in an error message could
	// only mean it was echoed back verbatim — same technique as
	// TestDialect2026_Decode_MalformedMetaFields, applied here to a
	// wrongly-typed top-level _meta value rather than a wrongly-typed
	// nested _meta field.
	const secretValue = "super-secret-should-never-be-echoed-back"

	tests := []struct {
		name           string
		raw            string
		wantErr        bool
		wantField      string
		wantNotContain string
	}{
		{
			// SPEC CHANGE: formerly "params absent entirely is tolerant"
			// (wantErr: false). No params at all means no _meta at all,
			// which now satisfies neither MUST field.
			name:      "params absent entirely is rejected: missing both MUST _meta fields (spec change)",
			raw:       `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			wantErr:   true,
			wantField: "io.modelcontextprotocol/protocolVersion",
		},
		{
			// SPEC CHANGE: formerly "params is an object without _meta is
			// tolerant" (wantErr: false).
			name:      "params is an object without _meta is rejected: missing both MUST _meta fields (spec change)",
			raw:       `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"foo":"bar"}}`,
			wantErr:   true,
			wantField: "io.modelcontextprotocol/protocolVersion",
		},
		{
			// SPEC CHANGE: formerly "params is an empty object (no _meta key
			// at all) is tolerant" (wantErr: false).
			name:      "params is an empty object (no _meta key at all) is rejected: missing both MUST _meta fields (spec change)",
			raw:       `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
			wantErr:   true,
			wantField: "io.modelcontextprotocol/protocolVersion",
		},
		{
			// SPEC CHANGE: formerly "params._meta present as an empty object
			// is accepted" (wantErr: false). An empty _meta object satisfies
			// neither MUST field either — see
			// TestDialect2026_Decode_MalformedMetaFields's equivalent case.
			name:      "params._meta present as an empty object is rejected: satisfies neither MUST field (spec change)",
			raw:       `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{}}}`,
			wantErr:   true,
			wantField: "io.modelcontextprotocol/protocolVersion",
		},
		{
			name:      "params is an object whose _meta is a JSON string is rejected",
			raw:       `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":"kaputt"}}`,
			wantErr:   true,
			wantField: "_meta",
		},
		{
			name:      "params is an object whose _meta is a JSON array is rejected",
			raw:       `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":[1,2,3]}}`,
			wantErr:   true,
			wantField: "_meta",
		},
		{
			// The regression this fix exists for: "_meta": null must be
			// rejected the same as any other wrongly-typed _meta, NOT
			// tolerated as if the key were simply absent. Both shapes
			// unmarshal to the same nil params.Meta map, so the two are
			// indistinguishable without the explicit-null check this test
			// pins down (see paramsMetaIsExplicitNull).
			name:      "params._meta explicit JSON null is rejected, distinct from _meta being absent",
			raw:       `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":null}}`,
			wantErr:   true,
			wantField: "_meta",
		},
		{
			name:           "params._meta as a JSON string carrying a sentinel value never leaks it in the error",
			raw:            fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":%q}}`, secretValue),
			wantErr:        true,
			wantField:      "_meta",
			wantNotContain: secretValue,
		},
		{
			name:    "params is a JSON string (not an object at all) is rejected",
			raw:     `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":"kaputt"}`,
			wantErr: true,
		},
		{
			name:    "params is a JSON array (not an object at all) is rejected",
			raw:     `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":[1,2,3]}`,
			wantErr: true,
		},
		{
			name:    "params is a JSON number (not an object at all) is rejected",
			raw:     `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":42}`,
			wantErr: true,
		},
		{
			// params: null is distinct from params being ABSENT entirely
			// (len(req.Params) == 0): the JSON-RPC body explicitly carries
			// the 4-byte literal "null" for params, which is neither absent
			// nor an object. json.Unmarshal("null", &map) succeeds with a nil
			// map and no error — the same pitfall isJSONObject guards against
			// for _meta — so this pins down that the top-level params check
			// (Decode's own inline "err != nil || topParams == nil" branch)
			// catches a null params value too, not just a genuinely malformed
			// one.
			name:    "params is the JSON null literal (not an object at all) is rejected",
			raw:     `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":null}`,
			wantErr: true,
		},
	}

	d := mcp.NewDialect2026(mcp.V20260728, "voidllm", "0.1.0")

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := d.Decode([]byte(tc.raw), modernHeader())

			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Decode() unexpected error: %+v", err)
				}
				return
			}

			if err == nil {
				t.Fatal("Decode() error = nil, want CodeInvalidParams")
			}
			if err.Code != mcp.CodeInvalidParams {
				t.Errorf("Decode() error code = %d, want %d (CodeInvalidParams)", err.Code, mcp.CodeInvalidParams)
			}
			if tc.wantField != "" && !strings.Contains(err.Message, tc.wantField) {
				t.Errorf("error message = %q, want it to name the offending field %q", err.Message, tc.wantField)
			}
			if tc.wantNotContain != "" && strings.Contains(err.Message, tc.wantNotContain) {
				t.Errorf("error message = %q leaked the sentinel value, want only the field NAME", err.Message)
			}
		})
	}
}

// metaFieldRequest builds a tools/list request whose params._meta carries
// metaKey set to rawJSONValue (already-encoded JSON, not a Go value, so
// callers can construct deliberately mistyped values) while the OTHER MUST
// _meta field (docs/mcp-v2.md §3.2) is left at a valid default — see
// modernMetaObject and modernRequestBody, which this is built on top of.
func metaFieldRequest(metaKey, rawJSONValue string) string {
	return modernRequestBody(1, "tools/list", nil, map[string]any{metaKey: json.RawMessage(rawJSONValue)})
}

// ---- Large arguments blocks pass through Decode untouched ------------------
//
// docs/mcp-v2.md, FIX 5: dialect2026.Decode used to unmarshal req.Params THREE
// separate times (isJSONObject, then a check for an explicit "_meta": null,
// then the actual params.Meta decode), each a full unmarshal of the entire
// params object — including a tools/call request's "arguments" payload, which
// can be arbitrarily large and has nothing to do with _meta at all. The three
// passes were collapsed into one single unmarshal into topParams, from which
// every distinction Decode needs is derived. This test pins down the
// observable contract that collapse must not have changed: a large
// "arguments" block sitting alongside _meta in params must reach
// Envelope.Params completely unexamined and byte-for-byte unchanged — Decode
// never re-serializes or otherwise touches it.

// bigToolCallArguments returns a deliberately large, deterministic arguments
// payload — 2000 numbered entries — standing in for a real tool call's
// potentially large argument set (e.g. a big batch of rows, a long document).
func bigToolCallArguments() map[string]any {
	args := make(map[string]any, 2000)
	for i := 0; i < 2000; i++ {
		args[fmt.Sprintf("field_%04d", i)] = fmt.Sprintf("value-%04d-some-payload-content", i)
	}
	return args
}

// TestDialect2026_Decode_LargeArgumentsBlockPassesThroughUnchanged builds a
// tools/call request whose params carry both a valid _meta object and a large
// "arguments" block, constructed so paramsBytes — the exact bytes of the
// "params" JSON value — are known independently of however Decode processes
// them. It then asserts Envelope.Params is byte-for-byte identical to
// paramsBytes: proof that Decode's single-pass _meta extraction never
// re-marshals, reorders, or otherwise mutates the surrounding params object it
// was handed, no matter how large "arguments" is.
func TestDialect2026_Decode_LargeArgumentsBlockPassesThroughUnchanged(t *testing.T) {
	t.Parallel()

	bigArgs := bigToolCallArguments()

	params := map[string]any{
		"name":      "big_tool",
		"arguments": bigArgs,
		"_meta":     modernMetaObject(nil),
	}
	paramsBytes, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}

	reqBytes, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  json.RawMessage(paramsBytes),
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	d := mcp.NewDialect2026(mcp.V20260728, "voidllm", "0.1.0")
	env, decErr := d.Decode(reqBytes, modernHeader())
	if decErr != nil {
		t.Fatalf("Decode() unexpected error: %+v", decErr)
	}

	if !bytes.Equal([]byte(env.Params), paramsBytes) {
		t.Fatalf("Envelope.Params was not passed through byte-for-byte:\nlen(got)=%d len(want)=%d",
			len(env.Params), len(paramsBytes))
	}

	// Belt-and-suspenders: also confirm "arguments" specifically decodes back
	// to exactly what was sent, not merely that SOME bytes matched.
	var decodedParams struct {
		Arguments map[string]string `json:"arguments"`
	}
	if err := json.Unmarshal(env.Params, &decodedParams); err != nil {
		t.Fatalf("unmarshal env.Params: %v", err)
	}
	if len(decodedParams.Arguments) != len(bigArgs) {
		t.Fatalf("decoded arguments has %d entries, want %d", len(decodedParams.Arguments), len(bigArgs))
	}
	for k, v := range bigArgs {
		if decodedParams.Arguments[k] != v {
			t.Errorf("arguments[%q] = %q, want %q", k, decodedParams.Arguments[k], v)
		}
	}

	// The tool name Decode DOES extract from params (Name is read for
	// tools/call, see TestDialect2026_Decode_ToolsCallExtractsName) must also
	// be unaffected by the large sibling "arguments" block.
	if env.Name != "big_tool" {
		t.Errorf("Name = %q, want %q", env.Name, "big_tool")
	}
}

// BenchmarkDialect2026Decode_LargeArguments measures Decode's cost against a
// tools/call request carrying a large "arguments" block, so a future regression
// that reintroduces a second or third full unmarshal of req.Params (see this
// test file's collapsed-single-pass doc above) shows up as a clear, measurable
// regression here rather than only as a functional test.
func BenchmarkDialect2026Decode_LargeArguments(b *testing.B) {
	params := map[string]any{
		"name":      "big_tool",
		"arguments": bigToolCallArguments(),
		"_meta":     modernMetaObject(nil),
	}
	paramsBytes, err := json.Marshal(params)
	if err != nil {
		b.Fatalf("marshal params: %v", err)
	}
	reqBytes, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  json.RawMessage(paramsBytes),
	})
	if err != nil {
		b.Fatalf("marshal request: %v", err)
	}

	d := mcp.NewDialect2026(mcp.V20260728, "voidllm", "0.1.0")
	hdr := modernHeader()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, decErr := d.Decode(reqBytes, hdr); decErr != nil {
			b.Fatalf("Decode() unexpected error: %+v", decErr)
		}
	}
}

// ---- EncodeResult -------------------------------------------------------------

// TestDialect2026_EncodeResult_AlwaysComplete verifies every modern-era result
// carries resultType: "complete" and self-identifies the server under
// _meta["io.modelcontextprotocol/serverInfo"], regardless of whether the
// Server set a CacheHint (docs/mcp-v2.md §3.2, §3.8).
func TestDialect2026_EncodeResult_AlwaysComplete(t *testing.T) {
	t.Parallel()

	d := mcp.NewDialect2026(mcp.V20260728, "voidllm", "0.1.0")
	out, encErr := d.EncodeResult(json.RawMessage(`1`), &mcp.Result{Payload: map[string]any{"tools": []any{}}})
	if encErr != nil {
		t.Fatalf("EncodeResult() unexpected error: %v", encErr)
	}

	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	result, ok := decoded["result"].(map[string]any)
	if !ok {
		t.Fatalf("result field type = %T, want map[string]any", decoded["result"])
	}

	if result["resultType"] != "complete" {
		t.Errorf("resultType = %v, want %q", result["resultType"], "complete")
	}
	if _, ok := result["ttlMs"]; ok {
		t.Errorf("ttlMs present without a CacheHint being set, got: %v", result)
	}
	if _, ok := result["cacheScope"]; ok {
		t.Errorf("cacheScope present without a CacheHint being set, got: %v", result)
	}

	meta, ok := result["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("_meta field type = %T, want map[string]any", result["_meta"])
	}
	serverInfo, ok := meta["io.modelcontextprotocol/serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("_meta[serverInfo] type = %T, want map[string]any", meta["io.modelcontextprotocol/serverInfo"])
	}
	if serverInfo["name"] != "voidllm" || serverInfo["version"] != "0.1.0" {
		t.Errorf("serverInfo = %v, want {name:voidllm version:0.1.0}", serverInfo)
	}
}

// TestDialect2026_EncodeResult_CacheHint verifies ttlMs and cacheScope are
// added to the wire exactly when the Server sets a non-empty CacheHint.Scope,
// and that a negative ttlMs is clamped to 0 per docs/mcp-v2.md §5.
func TestDialect2026_EncodeResult_CacheHint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		cache      mcp.CacheHint
		wantTTL    float64
		wantScope  string
		wantNoHint bool
	}{
		{
			name:      "public scope with positive ttl",
			cache:     mcp.CacheHint{TTLMs: 3600000, Scope: mcp.CacheScopePublic},
			wantTTL:   3600000,
			wantScope: "public",
		},
		{
			name:      "private scope with positive ttl",
			cache:     mcp.CacheHint{TTLMs: 60000, Scope: mcp.CacheScopePrivate},
			wantTTL:   60000,
			wantScope: "private",
		},
		{
			name:      "negative ttl is clamped to zero",
			cache:     mcp.CacheHint{TTLMs: -500, Scope: mcp.CacheScopePrivate},
			wantTTL:   0,
			wantScope: "private",
		},
		{
			name:       "zero-value CacheHint (empty scope) emits no cache fields at all",
			cache:      mcp.CacheHint{},
			wantNoHint: true,
		},
	}

	d := mcp.NewDialect2026(mcp.V20260728, "voidllm", "0.1.0")

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out, encErr := d.EncodeResult(json.RawMessage(`1`), &mcp.Result{Payload: map[string]any{}, Cache: tc.cache})
			if encErr != nil {
				t.Fatalf("EncodeResult() unexpected error: %v", encErr)
			}

			var decoded map[string]any
			if err := json.Unmarshal(out, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			result := decoded["result"].(map[string]any)

			if tc.wantNoHint {
				if _, ok := result["ttlMs"]; ok {
					t.Errorf("ttlMs present for empty CacheHint, got: %v", result)
				}
				if _, ok := result["cacheScope"]; ok {
					t.Errorf("cacheScope present for empty CacheHint, got: %v", result)
				}
				return
			}

			ttl, ok := result["ttlMs"].(float64)
			if !ok {
				t.Fatalf("ttlMs type = %T, want float64", result["ttlMs"])
			}
			if ttl != tc.wantTTL {
				t.Errorf("ttlMs = %v, want %v", ttl, tc.wantTTL)
			}
			if ttl < 0 {
				t.Errorf("ttlMs = %v, must be >= 0 per CacheableResult (docs/mcp-v2.md §5)", ttl)
			}
			if result["cacheScope"] != tc.wantScope {
				t.Errorf("cacheScope = %v, want %q", result["cacheScope"], tc.wantScope)
			}
		})
	}
}

// TestDialect2026_EncodeResult_PayloadCannotOverrideWrapperFields verifies
// EncodeResult's own documented ordering guarantee (dialect_2026.go): a
// handler-supplied payload is merged in FIRST, and resultType, ttlMs,
// cacheScope, and _meta are all set AFTER — never the other way around. A
// payload that happens to carry a key with the same name as one of these
// four wrapper fields must never be able to override the value EncodeResult
// itself decided, whether that payload key is attacker-controlled (a tool's
// own return value, echoed into a payload map by a handler that does not
// sanitize it) or simply an accidental collision. All four fields are
// checked together, in one test, precisely so a future change to this
// ordering cannot silently regress one of them while the others still pass
// (docs/mcp-v2.md, review finding D).
func TestDialect2026_EncodeResult_PayloadCannotOverrideWrapperFields(t *testing.T) {
	t.Parallel()

	d := mcp.NewDialect2026(mcp.V20260728, "voidllm", "0.1.0")

	payload := map[string]any{
		// A genuine, unrelated payload field, so this test also proves the
		// malicious/colliding keys below do not clobber ordinary payload
		// content either.
		"tools": []any{map[string]any{"name": "search"}},

		// Each of the four keys below collides with a name EncodeResult sets
		// itself, with an attacker-shaped value chosen to be unmistakable if
		// it survives into the wire response.
		"resultType": "input_required", // would smuggle a fake MRTR result if it won
		"ttlMs":      float64(999999999),
		"cacheScope": "public", // would smuggle public caching for what should be private
		"_meta": map[string]any{
			"io.modelcontextprotocol/serverInfo": map[string]any{
				"name":    "attacker-controlled-fake-server",
				"version": "0.0.0-evil",
			},
		},
	}

	out, encErr := d.EncodeResult(json.RawMessage(`1`), &mcp.Result{
		Payload: payload,
		Cache:   mcp.CacheHint{TTLMs: 60000, Scope: mcp.CacheScopePrivate},
	})
	if encErr != nil {
		t.Fatalf("EncodeResult() unexpected error: %v", encErr)
	}

	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	result, ok := decoded["result"].(map[string]any)
	if !ok {
		t.Fatalf("result field type = %T, want map[string]any", decoded["result"])
	}

	// resultType: always EncodeResult's own "complete", never the payload's
	// "input_required".
	if result["resultType"] != "complete" {
		t.Errorf("resultType = %v, want %q (the payload's own resultType key must never win)", result["resultType"], "complete")
	}

	// ttlMs/cacheScope: always the Cache hint EncodeResult was actually
	// given, never the payload's own colliding values.
	ttl, ok := result["ttlMs"].(float64)
	if !ok {
		t.Fatalf("ttlMs type = %T, want float64", result["ttlMs"])
	}
	if ttl != 60000 {
		t.Errorf("ttlMs = %v, want %v (the payload's own ttlMs key must never win)", ttl, 60000)
	}
	if result["cacheScope"] != mcp.CacheScopePrivate {
		t.Errorf("cacheScope = %v, want %q (the payload's own cacheScope key must never win)", result["cacheScope"], mcp.CacheScopePrivate)
	}

	// _meta: always EncodeResult's own serverInfo, never the payload's
	// attacker-supplied _meta.
	meta, ok := result["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("_meta field type = %T, want map[string]any", result["_meta"])
	}
	serverInfo, ok := meta["io.modelcontextprotocol/serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("_meta[serverInfo] type = %T, want map[string]any", meta["io.modelcontextprotocol/serverInfo"])
	}
	if serverInfo["name"] != "voidllm" || serverInfo["version"] != "0.1.0" {
		t.Errorf("serverInfo = %v, want {name:voidllm version:0.1.0} (the payload's own _meta must never win)", serverInfo)
	}

	// The genuine, unrelated payload content must still have survived the
	// merge unchanged.
	tools, ok := result["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want the original payload's tools slice to survive the merge", result["tools"])
	}
}

// TestDialect2026_EncodeResult_NilResult_ReturnsError verifies FIX A's
// defensive rejection of a nil *Result, mirroring
// TestLegacyDialect_EncodeResult_NilResult_ReturnsError: EncodeResult must
// error rather than silently producing a response with neither "result" nor
// "error".
func TestDialect2026_EncodeResult_NilResult_ReturnsError(t *testing.T) {
	t.Parallel()

	d := mcp.NewDialect2026(mcp.V20260728, "voidllm", "0.1.0")
	out, encErr := d.EncodeResult(json.RawMessage(`1`), nil)
	if encErr == nil {
		t.Fatal("EncodeResult(nil) error = nil, want non-nil")
	}
	if out != nil {
		t.Errorf("EncodeResult(nil) body = %s, want nil alongside a non-nil error", out)
	}
}

// ---- EncodeError --------------------------------------------------------------

func TestDialect2026_EncodeError(t *testing.T) {
	t.Parallel()

	d := mcp.NewDialect2026(mcp.V20260728, "voidllm", "0.1.0")
	out, encErr := d.EncodeError(json.RawMessage(`5`), &mcp.Error{
		Code:    mcp.CodeUnsupportedProtocolVersion,
		Message: "unsupported protocol version",
		Data:    map[string]any{"supported": []string{"2026-07-28"}, "requested": "1900-01-01"},
	})
	if encErr != nil {
		t.Fatalf("EncodeError() unexpected error: %v", encErr)
	}

	var decoded mcp.Response
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Result != nil {
		t.Errorf("Result = %v, want nil on an error response", decoded.Result)
	}
	if decoded.Error == nil {
		t.Fatal("Error is nil, want non-nil")
	}
	if decoded.Error.Code != mcp.CodeUnsupportedProtocolVersion {
		t.Errorf("Error.Code = %d, want %d", decoded.Error.Code, mcp.CodeUnsupportedProtocolVersion)
	}
	// The error envelope shape is identical across eras — no resultType,
	// _meta, or cache fields belong on an error response.
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, ok := raw["result"]; ok {
		t.Errorf("error response must not carry a \"result\" key, got: %v", raw)
	}
}

// ---- Server-level: cacheScope per cacheable method (docs/mcp-v2.md §5) -------

// modernHeader returns transport headers that negotiate the 2026-07-28 era.
func modernHeader() mcp.Header {
	return mcp.MapHeader{mcp.HeaderProtocolVersion: string(mcp.V20260728)}
}

// TestServer_ToolsList_Modern_CacheScopePrivate verifies that tools/list under
// the modern era is scoped private: because OnToolsListHook can filter by
// caller role, the result is caller-specific even though the tool schemas
// themselves are not (docs/mcp-v2.md §5, docs/mcp-v2.md §11.3 Befund 2).
func TestServer_ToolsList_Modern_CacheScopePrivate(t *testing.T) {
	t.Parallel()

	s := mcp.NewServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{Name: "a_tool", InputSchema: mcp.ObjectSchema(nil)},
		func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
			return mcp.TextResult("ok"), nil
		})

	out := s.Handle(context.Background(), []byte(modernRequestBody(1, "tools/list", nil, nil)), modernHeader())

	result := decodeModernResult(t, out.Body)
	if result["cacheScope"] != mcp.CacheScopePrivate {
		t.Errorf("tools/list cacheScope = %v, want %q", result["cacheScope"], mcp.CacheScopePrivate)
	}
	assertNonNegativeTTL(t, result)
}

// TestServer_Discover_Modern_CacheScopePublic verifies that server/discover is
// scoped public: its content depends only on server configuration, never on
// caller identity, so any client or shared gateway may reuse it.
func TestServer_Discover_Modern_CacheScopePublic(t *testing.T) {
	t.Parallel()

	s := mcp.NewServer("voidllm", "0.1.0")
	out := s.Handle(context.Background(), []byte(modernRequestBody(1, "server/discover", nil, nil)), modernHeader())

	result := decodeModernResult(t, out.Body)
	if result["cacheScope"] != mcp.CacheScopePublic {
		t.Errorf("server/discover cacheScope = %v, want %q", result["cacheScope"], mcp.CacheScopePublic)
	}
	assertNonNegativeTTL(t, result)
}

// decodeModernResult unmarshals a modern-era Handle() response into the
// result object's map[string]any representation.
func decodeModernResult(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var resp mcp.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nraw: %s", err, raw)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", resp.Error)
	}
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("re-marshal result: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode result map: %v", err)
	}
	return m
}

// assertNonNegativeTTL fails the test if result["ttlMs"] is absent or negative.
func assertNonNegativeTTL(t *testing.T, result map[string]any) {
	t.Helper()
	ttl, ok := result["ttlMs"].(float64)
	if !ok {
		t.Fatalf("ttlMs type = %T, want float64 (present)", result["ttlMs"])
	}
	if ttl < 0 {
		t.Errorf("ttlMs = %v, must be >= 0 per CacheableResult", ttl)
	}
}
