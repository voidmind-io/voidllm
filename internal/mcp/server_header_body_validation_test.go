package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers docs/mcp-v2.md review round Fund 6: the built-in server
// (mcp.Server) now validates a registered tool's x-mcp-header bindings (MCP
// 2026-07-28 §4.3) against the request body on every tools/call, per §4.5's
// "Server, die den Body verarbeiten, MÜSSEN Header gegen Body validieren."
// Before this fix, validateMCPHeaders (internal/api/admin/mcp_handler.go)
// checked only version, method, and target name; a Mcp-Param-{Name} header
// disagreeing with the body's own argument reached the tool handler
// unexamined.

// guardedHeaderToolSchema annotates two properties for header mirroring —
// "region" (string) and "amount" (integer) — plus one unannotated "note"
// property, so a test can prove the unannotated property is never
// constrained by this validation at all.
const guardedHeaderToolSchema = `{
	"type": "object",
	"properties": {
		"region": {"type": "string", "x-mcp-header": "Region"},
		"amount": {"type": "integer", "x-mcp-header": "Amount"},
		"note": {"type": "string"}
	},
	"required": ["region", "amount"]
}`

// registerGuardedHeaderTool registers a tool annotated per
// guardedHeaderToolSchema on a fresh server, returning the server and a
// pointer the handler sets to true if and when it actually runs.
func registerGuardedHeaderTool(t *testing.T) (*mcp.Server, *bool) {
	t.Helper()

	s := mcp.NewServer("voidllm", "0.1.0")
	handlerCalled := new(bool)
	if err := s.RegisterTool(
		mcp.Tool{Name: "guarded_header_tool", InputSchema: mcp.JSONSchema(guardedHeaderToolSchema)},
		func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
			*handlerCalled = true
			return mcp.TextResult("ok"), nil
		},
	); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	return s, handlerCalled
}

// callGuardedTool drives a modern-era tools/call for guarded_header_tool
// with the given arguments and extra Mcp-Param-* headers, returning the
// decoded response and the raw HandleResult (for Hint assertions).
func callGuardedTool(t *testing.T, s *mcp.Server, arguments map[string]any, paramHeaders map[string]string) (mcp.Response, mcp.HandleResult) {
	t.Helper()

	body := modernRequestBody(1, "tools/call",
		map[string]any{"name": "guarded_header_tool", "arguments": arguments}, nil)

	hdr := mcp.MapHeader{mcp.HeaderProtocolVersion: string(mcp.V20260728)}
	for k, v := range paramHeaders {
		hdr[k] = v
	}

	result := s.Handle(context.Background(), []byte(body), hdr)

	var resp mcp.Response
	if err := json.Unmarshal(result.Body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nraw: %s", err, result.Body)
	}
	return resp, result
}

// TestServer_ToolsCall_HeaderBodyValidation is the table-driven enforcement
// test for §4.5 header-against-body validation on the built-in server.
func TestServer_ToolsCall_HeaderBodyValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		arguments         map[string]any
		headers           map[string]string
		wantHandlerCalled bool
		wantErr           bool
	}{
		{
			name:              "matching header and body: handler runs",
			arguments:         map[string]any{"region": "eu-west1", "amount": 42},
			headers:           map[string]string{"Mcp-Param-Region": "eu-west1", "Mcp-Param-Amount": "42"},
			wantHandlerCalled: true,
			wantErr:           false,
		},
		{
			name:              "missing Mcp-Param-Region header: rejected, handler never runs",
			arguments:         map[string]any{"region": "eu-west1", "amount": 42},
			headers:           map[string]string{"Mcp-Param-Amount": "42"},
			wantHandlerCalled: false,
			wantErr:           true,
		},
		{
			name:              "header disagrees with body (string): rejected, handler never runs",
			arguments:         map[string]any{"region": "eu-west1", "amount": 42},
			headers:           map[string]string{"Mcp-Param-Region": "us-west1", "Mcp-Param-Amount": "42"},
			wantHandlerCalled: false,
			wantErr:           true,
		},
		{
			name:      "integer header/body agree numerically despite different text (42.0 == 42, MCP 2026-07-28 §4.5)",
			arguments: map[string]any{"region": "eu-west1", "amount": 42},
			headers:   map[string]string{"Mcp-Param-Region": "eu-west1", "Mcp-Param-Amount": "42.0"},
			// integer comparison is numeric, not string — 42.0 must match 42.
			wantHandlerCalled: true,
			wantErr:           false,
		},
		{
			name:              "integer header/body genuinely disagree numerically: rejected",
			arguments:         map[string]any{"region": "eu-west1", "amount": 42},
			headers:           map[string]string{"Mcp-Param-Region": "eu-west1", "Mcp-Param-Amount": "43"},
			wantHandlerCalled: false,
			wantErr:           true,
		},
		{
			name:              "header value is not a number at all: rejected",
			arguments:         map[string]any{"region": "eu-west1", "amount": 42},
			headers:           map[string]string{"Mcp-Param-Region": "eu-west1", "Mcp-Param-Amount": "not-a-number"},
			wantHandlerCalled: false,
			wantErr:           true,
		},
		{
			name:      "non-ASCII header value matches after base64-sentinel decoding (MCP 2026-07-28 §4.4)",
			arguments: map[string]any{"region": "Hello, 世界", "amount": 42},
			// EncodeHeaderValue("Hello, 世界") == "=?base64?SGVsbG8sIOS4lueVjA==?="
			headers:           map[string]string{"Mcp-Param-Region": "=?base64?SGVsbG8sIOS4lueVjA==?=", "Mcp-Param-Amount": "42"},
			wantHandlerCalled: true,
			wantErr:           false,
		},
		{
			name:              "sentinel-wrapped header value is not valid base64: rejected",
			arguments:         map[string]any{"region": "eu-west1", "amount": 42},
			headers:           map[string]string{"Mcp-Param-Region": "=?base64?not-valid-base64?=", "Mcp-Param-Amount": "42"},
			wantHandlerCalled: false,
			wantErr:           true,
		},
		{
			name:              "an unannotated property is never validated, even if it were somehow mirrored",
			arguments:         map[string]any{"region": "eu-west1", "amount": 42, "note": "irrelevant"},
			headers:           map[string]string{"Mcp-Param-Region": "eu-west1", "Mcp-Param-Amount": "42"},
			wantHandlerCalled: true,
			wantErr:           false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, handlerCalled := registerGuardedHeaderTool(t)
			resp, result := callGuardedTool(t, s, tc.arguments, tc.headers)

			if *handlerCalled != tc.wantHandlerCalled {
				t.Errorf("handler called = %v, want %v", *handlerCalled, tc.wantHandlerCalled)
			}

			if !tc.wantErr {
				if resp.Error != nil {
					t.Fatalf("unexpected error: %+v", resp.Error)
				}
				return
			}

			if resp.Error == nil {
				t.Fatal("expected a JSON-RPC error, got nil")
			}
			if resp.Error.Code != mcp.CodeHeaderMismatch {
				t.Errorf("Error.Code = %d, want %d (CodeHeaderMismatch)", resp.Error.Code, mcp.CodeHeaderMismatch)
			}
			if result.Hint != mcp.HintBadRequest {
				t.Errorf("Hint = %v, want HintBadRequest (MCP 2026-07-28 §4.5 requires HTTP 400 for HeaderMismatch)", result.Hint)
			}
		})
	}
}

// TestServer_ToolsCall_HeaderBodyValidation_LegacyEraSkipsValidation verifies
// that a genuinely legacy-era request — which structurally cannot carry any
// Mcp-Param-{Name} header, since no ClientDialect this package ships ever
// renders one for a legacy request (legacyClientDialect.Prepare's own doc) —
// is never rejected for omitting headers it has no way to send. Without this
// era gate, every legacy caller of a header-annotated tool would be
// permanently unable to call it at all.
func TestServer_ToolsCall_HeaderBodyValidation_LegacyEraSkipsValidation(t *testing.T) {
	t.Parallel()

	s, handlerCalled := registerGuardedHeaderTool(t)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"guarded_header_tool","arguments":{"region":"eu-west1","amount":42}}}`
	result := s.Handle(context.Background(), []byte(body), mcp.MapHeader{})

	var resp mcp.Response
	if err := json.Unmarshal(result.Body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nraw: %s", err, result.Body)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error on a legacy-era request carrying no Mcp-Param-* headers at all: %+v", resp.Error)
	}
	if !*handlerCalled {
		t.Error("handler not called on a legacy-era request, want header/body validation skipped entirely for EraLegacy")
	}
}

// TestServer_ToolsCall_HeaderBodyValidation_ErrorNeverLeaksValues proves the
// CodeHeaderMismatch error text never embeds either side's value — both are
// tool-argument content, covered by this repo's zero-knowledge logging and
// error-message policy (see validateHeaderParams' own doc). A conspicuous,
// unique sentinel is sent as both the header and (a different) body value,
// and the whole encoded response is scanned for it.
func TestServer_ToolsCall_HeaderBodyValidation_ErrorNeverLeaksValues(t *testing.T) {
	t.Parallel()

	const headerSentinel = "SENTINEL-HEADER-VALUE-9f3a1c-do-not-leak-me"
	const bodySentinel = "SENTINEL-BODY-VALUE-5f2b8e-do-not-leak-me"

	s, _ := registerGuardedHeaderTool(t)
	_, result := callGuardedTool(t, s,
		map[string]any{"region": bodySentinel, "amount": 42},
		map[string]string{"Mcp-Param-Region": headerSentinel, "Mcp-Param-Amount": "42"})

	raw := string(result.Body)
	if strings.Contains(raw, headerSentinel) {
		t.Errorf("response leaks the header value sentinel: %s", raw)
	}
	if strings.Contains(raw, bodySentinel) {
		t.Errorf("response leaks the body value sentinel: %s", raw)
	}
}

// TestServer_RegisterTool_HeaderParamConstraintViolation_RejectsRegistration
// verifies RegisterTool's own registration-time enforcement (docs/mcp-v2.md
// review round Fund 6): a schema whose x-mcp-header annotation violates MCP
// 2026-07-28 §4.3 (here, "number" — explicitly excluded, unlike "integer")
// must register NOTHING and return a non-nil error wrapping
// ErrHeaderParamConstraint, rather than silently registering the tool
// without the header-mirroring guarantee its own annotation claims to offer.
func TestServer_RegisterTool_HeaderParamConstraintViolation_RejectsRegistration(t *testing.T) {
	t.Parallel()

	const invalidSchema = `{"type":"object","properties":{"amount":{"type":"number","x-mcp-header":"Amount"}}}`

	s := mcp.NewServer("voidllm", "0.1.0")
	err := s.RegisterTool(
		mcp.Tool{Name: "invalid_header_tool", InputSchema: mcp.JSONSchema(invalidSchema)},
		func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
			return mcp.TextResult("unreachable"), nil
		},
	)
	if err == nil {
		t.Fatal("RegisterTool() error = nil, want a non-nil error for a schema violating §4.3 constraints")
	}
	if !errors.Is(err, mcp.ErrHeaderParamConstraint) {
		t.Errorf("RegisterTool() error = %v, want it to wrap mcp.ErrHeaderParamConstraint", err)
	}

	for _, tool := range s.Tools() {
		if tool.Name == "invalid_header_tool" {
			t.Fatal("invalid_header_tool was registered despite RegisterTool returning an error — " +
				"a rejected registration must register nothing")
		}
	}
}
