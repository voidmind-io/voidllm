package mcp_test

import (
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// ---- Negotiate: the five era-detection rules (docs/mcp-v2.md §4.6) ----------

func TestNegotiate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		hdr     mcp.Header
		want    mcp.Version
		wantErr bool
	}{
		{
			name: "rule 1: MCP-Protocol-Version header names a modern version",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			hdr:  mcp.MapHeader{mcp.HeaderProtocolVersion: "2026-07-28"},
			want: mcp.V20260728,
		},
		{
			name: "rule 2: body method is initialize, no header",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
			hdr:  mcp.MapHeader{},
			want: mcp.V20250326,
		},
		{
			name: "rule 3: MCP-Protocol-Version header names an older, legacy version",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			hdr:  mcp.MapHeader{mcp.HeaderProtocolVersion: "2025-06-18"},
			want: mcp.V20250618,
		},
		{
			name: "rule 3: MCP-Protocol-Version header names the oldest legacy version",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			hdr:  mcp.MapHeader{mcp.HeaderProtocolVersion: "2025-11-25"},
			want: mcp.V20251125,
		},
		{
			name: "rule 4: neither header nor initialize method present",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			hdr:  mcp.MapHeader{},
			want: mcp.V20250326,
		},
		{
			name: "rule 4: nil header behaves like an absent header",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"ping"}`,
			hdr:  nil,
			want: mcp.V20250326,
		},
		{
			name:    "rule 5: unrecognized MCP-Protocol-Version yields UnsupportedProtocolVersion",
			raw:     `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			hdr:     mcp.MapHeader{mcp.HeaderProtocolVersion: "1900-01-01"},
			wantErr: true,
		},
		{
			name: "header value is trimmed of surrounding whitespace",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			hdr:  mcp.MapHeader{mcp.HeaderProtocolVersion: "  2026-07-28  "},
			want: mcp.V20260728,
		},
		{
			// Regression for docs/mcp-v2.md FIX 1: Negotiate and
			// validateMCPHeaders/handleMCPSSE (internal/api/admin) must never
			// disagree about whether a padded header counts as modern — both
			// now go through mcp.ProtocolVersionHeaderValue, which trims tabs
			// exactly like spaces.
			name: "header value is trimmed of leading/trailing tabs",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			hdr:  mcp.MapHeader{mcp.HeaderProtocolVersion: "\t2026-07-28\t"},
			want: mcp.V20260728,
		},
		{
			name: "header wins even when the body method is initialize",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
			hdr:  mcp.MapHeader{mcp.HeaderProtocolVersion: "2026-07-28"},
			want: mcp.V20260728,
		},
		{
			name: "an unparseable body does not itself cause a Negotiate error when no header is set",
			raw:  `{not valid json`,
			hdr:  mcp.MapHeader{},
			want: mcp.V20250326,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := mcp.Negotiate([]byte(tc.raw), tc.hdr)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("Negotiate() error = nil, want UnsupportedProtocolVersion error")
				}
				if err.Code != mcp.CodeUnsupportedProtocolVersion {
					t.Errorf("Negotiate() error code = %d, want %d", err.Code, mcp.CodeUnsupportedProtocolVersion)
				}
				return
			}
			if err != nil {
				t.Fatalf("Negotiate() unexpected error: %+v", err)
			}
			if got != tc.want {
				t.Errorf("Negotiate() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNegotiate_UnsupportedVersion_ErrorData verifies that the
// UnsupportedProtocolVersion error's Data field carries both "supported" (every
// version VoidLLM understands) and "requested" (the exact value the caller
// sent), per docs/mcp-v2.md §8's UnsupportedProtocolVersionError shape.
func TestNegotiate_UnsupportedVersion_ErrorData(t *testing.T) {
	t.Parallel()

	const requested = "1900-01-01"
	hdr := mcp.MapHeader{mcp.HeaderProtocolVersion: requested}

	_, err := mcp.Negotiate([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), hdr)
	if err == nil {
		t.Fatal("Negotiate() error = nil, want UnsupportedProtocolVersion error")
	}
	if err.Code != mcp.CodeUnsupportedProtocolVersion {
		t.Fatalf("error code = %d, want %d", err.Code, mcp.CodeUnsupportedProtocolVersion)
	}

	data, ok := err.Data.(map[string]any)
	if !ok {
		t.Fatalf("error.Data type = %T, want map[string]any", err.Data)
	}

	gotRequested, ok := data["requested"].(string)
	if !ok || gotRequested != requested {
		t.Errorf("data[\"requested\"] = %v, want %q", data["requested"], requested)
	}

	supported, ok := data["supported"].([]mcp.Version)
	if !ok {
		t.Fatalf("data[\"supported\"] type = %T, want []mcp.Version", data["supported"])
	}
	if len(supported) != 4 {
		t.Errorf("data[\"supported\"] len = %d, want 4", len(supported))
	}
	wantSet := map[mcp.Version]bool{
		mcp.V20250326: true, mcp.V20250618: true, mcp.V20251125: true, mcp.V20260728: true,
	}
	for _, v := range supported {
		if !wantSet[v] {
			t.Errorf("data[\"supported\"] contains unexpected version %q", v)
		}
		delete(wantSet, v)
	}
	if len(wantSet) != 0 {
		t.Errorf("data[\"supported\"] is missing versions: %v", wantSet)
	}
}

// TestNegotiate_HeaderNameCaseInsensitive verifies that Negotiate finds the
// MCP-Protocol-Version header regardless of the casing the caller used, since
// HTTP header names are case-insensitive (RFC 9110 §5.1) and MapHeader.Get
// implements that contract.
func TestNegotiate_HeaderNameCaseInsensitive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		headerKey string
	}{
		{"canonical casing", "MCP-Protocol-Version"},
		{"all lowercase", "mcp-protocol-version"},
		{"all uppercase", "MCP-PROTOCOL-VERSION"},
		{"mixed casing", "Mcp-protocol-VERSION"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			hdr := mcp.MapHeader{tc.headerKey: "2026-07-28"}
			got, err := mcp.Negotiate([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), hdr)
			if err != nil {
				t.Fatalf("Negotiate() unexpected error: %+v", err)
			}
			if got != mcp.V20260728 {
				t.Errorf("Negotiate() with header key %q = %q, want %q", tc.headerKey, got, mcp.V20260728)
			}
		})
	}
}
