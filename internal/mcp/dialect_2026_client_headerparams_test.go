package mcp_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// ---- Prepare mirrors resolvable x-mcp-header bindings (MCP 2026-07-28 §4.3)

// TestDialect2026Client_Prepare_HeaderParams_MirrorsBindings verifies Prepare
// renders a bound tool argument onto Mcp-Param-{Name}, reusing
// EncodeHeaderValue's own base64-sentinel rules rather than re-implementing
// them: a plain ASCII value passes through unchanged, and a non-ASCII value
// comes out in EXACTLY the sentinel form EncodeHeaderValue itself produces
// (docs/mcp-v2.md §4.4's own worked example). The second case is what
// catches a Prepare that renders x-mcp-header values through some ad hoc
// encoding of its own instead of calling EncodeHeaderValue.
func TestDialect2026Client_Prepare_HeaderParams_MirrorsBindings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		arguments string
		wantValue string
	}{
		{
			name:      "plain ASCII argument passes through unchanged",
			arguments: `{"region":"us-west1"}`,
			wantValue: "us-west1",
		},
		{
			name:      "non-ASCII argument is base64-sentinel-encoded exactly as EncodeHeaderValue would",
			arguments: `{"region":"Hello, 世界"}`,
			wantValue: "=?base64?SGVsbG8sIOS4lueVjA==?=",
		},
	}

	d := mcp.NewDialect2026Client(mcp.V20260728, mcp.ClientInfo{Name: "voidllm-test", Version: "1.0"})
	param := mcp.HeaderParam{Name: "Region", Path: []string{"region"}, Kind: mcp.HeaderParamKindString}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":` + tc.arguments + `}}`)
			_, hdr, err := d.Prepare(&mcp.CallRequest{Raw: raw, HeaderParams: []mcp.HeaderParam{param}}, &mcp.UpstreamState{})
			if err != nil {
				t.Fatalf("Prepare() error = %v, want nil", err)
			}

			got, ok := hdr["Mcp-Param-Region"]
			if !ok {
				t.Fatalf("Mcp-Param-Region header missing; hdr = %v", hdr)
			}
			if got != tc.wantValue {
				t.Errorf("Mcp-Param-Region = %q, want %q", got, tc.wantValue)
			}

			// Confirm it round-trips to exactly what EncodeHeaderValue itself
			// would have produced for this argument's raw value — proving
			// Prepare reused the shared function rather than rebuilding its
			// own encoding.
			var arg struct {
				Region string `json:"region"`
			}
			if err := json.Unmarshal([]byte(tc.arguments), &arg); err != nil {
				t.Fatalf("unmarshal test arguments: %v", err)
			}
			if want := mcp.EncodeHeaderValue(arg.Region); got != want {
				t.Errorf("Mcp-Param-Region = %q, want exactly EncodeHeaderValue(%q) = %q", got, arg.Region, want)
			}
		})
	}
}

// ---- Integer precision: verbatim, never through float64 --------------------

// TestDialect2026Client_Prepare_HeaderParams_IntegerPrecision_Verbatim
// verifies a large integer argument (beyond float64's 53-bit mantissa) is
// rendered onto the header byte-for-byte as it appeared in the request body.
// Rot the instant headerParamValue's integer path is changed to round-trip
// through float64 or any other numeric type: 9007199254740993 would then
// come out as 9007199254740992 (the nearest representable float64) or an
// exponential form, either of which fails this exact-string comparison.
func TestDialect2026Client_Prepare_HeaderParams_IntegerPrecision_Verbatim(t *testing.T) {
	t.Parallel()

	d := mcp.NewDialect2026Client(mcp.V20260728, mcp.ClientInfo{})
	param := mcp.HeaderParam{Name: "Amount", Path: []string{"amount"}, Kind: mcp.HeaderParamKindInteger}

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pay","arguments":{"amount":9007199254740993}}}`)
	_, hdr, err := d.Prepare(&mcp.CallRequest{Raw: raw, HeaderParams: []mcp.HeaderParam{param}}, &mcp.UpstreamState{})
	if err != nil {
		t.Fatalf("Prepare() error = %v, want nil", err)
	}

	const want = "9007199254740993"
	got, ok := hdr["Mcp-Param-Amount"]
	if !ok {
		t.Fatalf("Mcp-Param-Amount header missing; hdr = %v", hdr)
	}
	if got != want {
		t.Errorf("Mcp-Param-Amount = %q, want %q byte-for-byte", got, want)
	}
}

// TestDialect2026Client_Prepare_HeaderParams_FloatValue_NoHeader is the
// companion case: an argument of 42.0 against a Kind=Integer binding must
// produce NO header at all, because the JSON token "42.0" is not a bare
// integer literal (headerParamValue's isJSONBareInteger rejects any '.').
func TestDialect2026Client_Prepare_HeaderParams_FloatValue_NoHeader(t *testing.T) {
	t.Parallel()

	d := mcp.NewDialect2026Client(mcp.V20260728, mcp.ClientInfo{})
	param := mcp.HeaderParam{Name: "Amount", Path: []string{"amount"}, Kind: mcp.HeaderParamKindInteger}

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pay","arguments":{"amount":42.0}}}`)
	_, hdr, err := d.Prepare(&mcp.CallRequest{Raw: raw, HeaderParams: []mcp.HeaderParam{param}}, &mcp.UpstreamState{})
	if err != nil {
		t.Fatalf("Prepare() error = %v, want nil", err)
	}
	if v, ok := hdr["Mcp-Param-Amount"]; ok {
		t.Errorf("Mcp-Param-Amount = %q, want no header for a non-bare-integer JSON token", v)
	}
}

// ---- Missing/null/type-mismatched arguments: silently skipped, no error ----

// TestDialect2026Client_Prepare_HeaderParams_UnresolvableArgument_NoHeaderNoError
// verifies headerParamValue's documented "silently skip" contract end to end
// through Prepare: a missing argument, an explicit JSON null, an object
// where a string was declared, and a string where a boolean was declared all
// produce NEITHER a header NOR an error. This matters operationally: an
// empty Mcp-Param-X header would trip the upstream's own §4.5 validation
// with a -32020 HeaderMismatch, turning a merely incomplete argument set
// into a hard failure Prepare itself must never manufacture.
func TestDialect2026Client_Prepare_HeaderParams_UnresolvableArgument_NoHeaderNoError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		arguments string
		param     mcp.HeaderParam
	}{
		{
			name:      "argument missing entirely",
			arguments: `{"other":"value"}`,
			param:     mcp.HeaderParam{Name: "Region", Path: []string{"region"}, Kind: mcp.HeaderParamKindString},
		},
		{
			name:      "argument is explicit JSON null",
			arguments: `{"region":null}`,
			param:     mcp.HeaderParam{Name: "Region", Path: []string{"region"}, Kind: mcp.HeaderParamKindString},
		},
		{
			name:      "an object where a string was declared",
			arguments: `{"region":{"nested":"object"}}`,
			param:     mcp.HeaderParam{Name: "Region", Path: []string{"region"}, Kind: mcp.HeaderParamKindString},
		},
		{
			name:      "a string where a boolean was declared",
			arguments: `{"dryRun":"true"}`,
			param:     mcp.HeaderParam{Name: "Dry-Run", Path: []string{"dryRun"}, Kind: mcp.HeaderParamKindBoolean},
		},
	}

	d := mcp.NewDialect2026Client(mcp.V20260728, mcp.ClientInfo{})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":` + tc.arguments + `}}`)
			_, hdr, err := d.Prepare(&mcp.CallRequest{Raw: raw, HeaderParams: []mcp.HeaderParam{tc.param}}, &mcp.UpstreamState{})
			if err != nil {
				t.Fatalf("Prepare() error = %v, want nil", err)
			}
			for k := range hdr {
				if strings.HasPrefix(k, mcp.HeaderParamPrefix) {
					t.Errorf("hdr contains %q = %q, want no Mcp-Param-* header for an unresolvable argument", k, hdr[k])
				}
			}
		})
	}
}

// ---- HeaderParams only rendered for tools/call ------------------------------

// TestDialect2026Client_Prepare_HeaderParams_OnlyForToolsCall verifies that a
// non-tools/call request carrying a non-empty req.HeaderParams (which
// CallMCPTool sets unconditionally — see CallRequest.HeaderParams' own doc)
// still emits no Mcp-Param-* header at all. Prepare gates the whole
// mirroring block on method == "tools/call"; this is the regression test for
// that gate specifically, independent of whether any individual binding
// would otherwise resolve.
func TestDialect2026Client_Prepare_HeaderParams_OnlyForToolsCall(t *testing.T) {
	t.Parallel()

	d := mcp.NewDialect2026Client(mcp.V20260728, mcp.ClientInfo{})
	param := mcp.HeaderParam{Name: "Region", Path: []string{"region"}, Kind: mcp.HeaderParamKindString}

	// tools/list has no "arguments" at all, but the schema-mirroring block
	// must never even attempt to resolve the binding for a non-tools/call
	// method — params.region below is deliberately present and resolvable to
	// prove that.
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"region":"us-west1"}}`)
	_, hdr, err := d.Prepare(&mcp.CallRequest{Raw: raw, HeaderParams: []mcp.HeaderParam{param}}, &mcp.UpstreamState{})
	if err != nil {
		t.Fatalf("Prepare() error = %v, want nil", err)
	}
	for k := range hdr {
		if strings.HasPrefix(k, mcp.HeaderParamPrefix) {
			t.Errorf("hdr contains %q, want no Mcp-Param-* header on a non-tools/call request", k)
		}
	}
}
