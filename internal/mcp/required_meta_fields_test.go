package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// TestServer_Handle_Modern_RequiredMetaFieldValidation is the table-driven
// enforcement test for the 2026-07-28 revision's mandatory params._meta
// fields (docs/mcp-v2.md §3.2, quoted verbatim on dialect2026.Decode's doc):
//
//	A request missing any required field is malformed; the server MUST
//	reject it with JSON-RPC error code -32602 (Invalid params). On HTTP,
//	the response status MUST be 400 Bad Request.
//
// Every case is driven through the full Server.Handle path — with the
// MCP-Protocol-Version header set, so Negotiate resolves the modern era
// exactly as a real transport would — rather than calling dialect2026.Decode
// directly, so the test also pins down Handle's HTTP-status-hint mapping
// (HintBadRequest for the rejections, HintOK for the acceptances) alongside
// the JSON-RPC error code and message.
func TestServer_Handle_Modern_RequiredMetaFieldValidation(t *testing.T) {
	t.Parallel()

	const (
		metaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
		metaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
		metaClientInfo         = "io.modelcontextprotocol/clientInfo"
	)

	tests := []struct {
		name          string
		metaOverrides map[string]any // passed to modernRequestBody; nil value removes the key
		rawBody       string         // when non-empty, used verbatim instead of modernRequestBody
		wantErr       bool
		wantFields    []string // substrings the error message must contain, when wantErr
	}{
		{
			name:    "both MUST fields present is accepted",
			wantErr: false,
		},
		{
			name:          "protocolVersion missing is rejected, message names the field",
			metaOverrides: map[string]any{metaProtocolVersion: nil},
			wantErr:       true,
			wantFields:    []string{metaProtocolVersion},
		},
		{
			name:          "clientCapabilities missing is rejected, message names the field",
			metaOverrides: map[string]any{metaClientCapabilities: nil},
			wantErr:       true,
			wantFields:    []string{metaClientCapabilities},
		},
		{
			name: "both MUST fields missing is rejected",
			metaOverrides: map[string]any{
				metaProtocolVersion:    nil,
				metaClientCapabilities: nil,
			},
			wantErr:    true,
			wantFields: []string{metaProtocolVersion, metaClientCapabilities},
		},
		{
			// This is genuinely distinct from "both MUST fields missing"
			// above: that case sends params._meta as an EMPTY object ({}),
			// while this one sends no "_meta" key in params at all — both
			// satisfy neither MUST field, and both must be rejected the same
			// way, but they are different wire shapes (see
			// TestDialect2026_Decode_ParamsShape, which covers this
			// distinction directly against dialect2026.Decode). rawBody is
			// used here instead of modernRequestBody, which always emits a
			// (possibly empty) "_meta" key and so cannot construct this shape.
			name:       "_meta absent entirely (no \"_meta\" key in params at all) is rejected",
			rawBody:    `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
			wantErr:    true,
			wantFields: []string{metaProtocolVersion, metaClientCapabilities},
		},
		{
			name:          "clientInfo missing, both MUST fields present, is accepted — clientInfo is SHOULD, not MUST (docs/mcp-v2.md §3.2)",
			metaOverrides: map[string]any{metaClientInfo: nil},
			wantErr:       false,
		},
		{
			name:          "clientCapabilities explicit {} is accepted — an empty declaration is distinct from a missing one",
			metaOverrides: map[string]any{metaClientCapabilities: map[string]any{}},
			wantErr:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := mcp.NewServer("voidllm", "0.1.0")
			body := tc.rawBody
			if body == "" {
				body = modernRequestBody(1, "tools/list", nil, tc.metaOverrides)
			}
			hdr := mcp.MapHeader{mcp.HeaderProtocolVersion: string(mcp.V20260728)}

			result := s.Handle(context.Background(), []byte(body), hdr)
			if result.Body == nil {
				t.Fatal("Handle() Body = nil, want a JSON-RPC response")
			}

			var resp mcp.Response
			if err := json.Unmarshal(result.Body, &resp); err != nil {
				t.Fatalf("unmarshal response: %v\nraw: %s", err, result.Body)
			}

			if !tc.wantErr {
				if resp.Error != nil {
					t.Fatalf("unexpected error: %+v", resp.Error)
				}
				if result.Hint != mcp.HintOK {
					t.Errorf("Hint = %v, want HintOK for an accepted request", result.Hint)
				}
				return
			}

			if resp.Error == nil {
				t.Fatal("expected a JSON-RPC error, got nil")
			}
			if resp.Error.Code != mcp.CodeInvalidParams {
				t.Errorf("Error.Code = %d, want %d (CodeInvalidParams)", resp.Error.Code, mcp.CodeInvalidParams)
			}
			for _, want := range tc.wantFields {
				if !strings.Contains(resp.Error.Message, want) {
					t.Errorf("error message = %q, want it to contain %q", resp.Error.Message, want)
				}
			}
			if result.Hint != mcp.HintBadRequest {
				t.Errorf("Hint = %v, want HintBadRequest (docs/mcp-v2.md §3.2 requires HTTP 400 for this violation)", result.Hint)
			}
		})
	}
}
