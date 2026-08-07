package mcp_test

import (
	"fmt"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers docs/mcp-v2.md Fund 5: Envelope.Name must be populated
// for every method TargetParamKey names — "tools/call" and "prompts/get"
// from params.name, "resources/read" from params.uri — not only
// "tools/call", which is the only method the pre-fix Decode implementations
// (both dialect_2026.go and dialect_legacy.go) special-cased by hand. A
// method TargetParamKey does not recognize (e.g. "tools/list") must leave
// Name empty. Table-driven and symmetric across both server dialects, since
// TargetParamKey's mapping is meant to be identical in both eras.

// envelopeNameTests is the shared table both dialects' Decode are run
// against — one row per case TargetParamKey.go's own mapping distinguishes,
// plus the "no name at all" control.
var envelopeNameTests = []struct {
	name       string
	method     string
	paramsJSON string // the raw JSON object literal for "params"
	wantName   string
}{
	{
		name:       "tools/call extracts params.name",
		method:     "tools/call",
		paramsJSON: `{"name":"search","arguments":{}}`,
		wantName:   "search",
	},
	{
		name:       "prompts/get extracts params.name",
		method:     "prompts/get",
		paramsJSON: `{"name":"my_prompt","arguments":{}}`,
		wantName:   "my_prompt",
	},
	{
		name:       "resources/read extracts params.uri, not params.name",
		method:     "resources/read",
		paramsJSON: `{"uri":"file:///data.txt"}`,
		wantName:   "file:///data.txt",
	},
	{
		name:       "tools/list carries no name at all: Name stays empty",
		method:     "tools/list",
		paramsJSON: `{}`,
		wantName:   "",
	},
}

// TestLegacyDialect_Decode_EnvelopeName_AllNamingMethods drives
// envelopeNameTests through legacyDialect.Decode.
func TestLegacyDialect_Decode_EnvelopeName_AllNamingMethods(t *testing.T) {
	t.Parallel()

	d := mcp.NewLegacyDialect(mcp.V20250326)

	for _, tc := range envelopeNameTests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, tc.method, tc.paramsJSON)
			env, err := d.Decode([]byte(raw), mcp.MapHeader{})
			if err != nil {
				t.Fatalf("Decode() unexpected error: %+v", err)
			}
			if env.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", env.Name, tc.wantName)
			}
		})
	}
}

// TestDialect2026_Decode_EnvelopeName_AllNamingMethods is the modern-era
// counterpart, symmetric with the legacy test above — same table, same
// per-method expectations — the only difference being the modern request
// shape (params._meta carrying the two MUST fields).
func TestDialect2026_Decode_EnvelopeName_AllNamingMethods(t *testing.T) {
	t.Parallel()

	d := mcp.NewDialect2026(mcp.V20260728, "voidllm", "0.1.0")

	for _, tc := range envelopeNameTests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw := fmt.Sprintf(
				`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`,
				tc.method, modernParamsWithMeta(tc.paramsJSON),
			)
			env, err := d.Decode([]byte(raw), modernHeader())
			if err != nil {
				t.Fatalf("Decode() unexpected error: %+v", err)
			}
			if env.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", env.Name, tc.wantName)
			}
		})
	}
}

// modernParamsWithMeta merges baseParamsJSON — a raw JSON object literal,
// e.g. `{"name":"search"}` — with a params._meta object satisfying the
// 2026-07-28 revision's two MUST fields (io.modelcontextprotocol/
// protocolVersion and io.modelcontextprotocol/clientCapabilities), returning
// a new raw JSON object literal for "params". baseParamsJSON must be `{}` or
// a non-empty JSON object literal with no trailing/leading whitespace
// concerns — every call site in this file passes a static literal.
func modernParamsWithMeta(baseParamsJSON string) string {
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	if baseParamsJSON == "{}" {
		return "{" + meta + "}"
	}
	// baseParamsJSON is a non-empty object literal "{...}"; splice meta in
	// as an additional top-level field.
	inner := baseParamsJSON[1 : len(baseParamsJSON)-1]
	return "{" + inner + "," + meta + "}"
}
