package admin_test

// Regression tests for docs/mcp-v2.md FIX 1: a whitespace-padded
// MCP-Protocol-Version header used to be trimmed by mcp.Negotiate but read
// UNTRIMMED by validateMCPHeaders (internal/api/admin/mcp_handler.go) and
// handleMCPSSE. Negotiate would happily dispatch such a request as modern
// (its own strings.TrimSpace saw a valid version), while
// isModernProtocolVersion(hdrVersion) — fed the same header value but
// untrimmed — reported "not modern" for the padded string and made
// validateMCPHeaders return immediately, WITHOUT ever checking Mcp-Method or
// Mcp-Name against the body. A caller could therefore send a
// MCP-Protocol-Version header padded with leading/trailing whitespace and
// have its Mcp-Method/Mcp-Name headers go completely unchecked against the
// body, even though the request was genuinely processed as modern end to end.
//
// Both readers now go exclusively through mcp.ProtocolVersionHeaderValue,
// which trims exactly once, so Negotiate and validateMCPHeaders can never
// again disagree about whether a given header value counts as modern.
//
// Every test below pads ONLY the wire-level MCP-Protocol-Version header —
// never the body's params._meta protocolVersion — because that isolates
// exactly what the fix changed: whether validateMCPHeaders's own
// isModernProtocolVersion(hdrVersion) recognizes a padded-but-otherwise-valid
// header as modern (see TestMCPHandler_WhitespaceBypass_MatchingMethod_StillModern,
// TestMCPHandler_WhitespaceBypass_MismatchedMethod_Rejected, and
// TestMCPHandler_WhitespaceBypass_MissingMcpName_Rejected). A separate test,
// TestMCPHandler_WhitespaceBypass_IdenticallyPaddedHeaderAndBody_StillRejected,
// additionally pins down the literal historical exploit precondition described
// in docs/mcp-v2.md — identical padding mirrored into the body too — and
// confirms it is rejected as well, regardless of which specific violation
// catches it first.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// headerPaddings enumerates every whitespace-padding shape the bypass
// exploited: leading tabs, trailing spaces, both at once, and no padding at
// all (the control case, which must behave identically to the padded ones
// once the fix is in place — that is the whole point of trimming centrally).
func headerPaddings() []struct {
	name string
	pad  func(s string) string
} {
	return []struct {
		name string
		pad  func(s string) string
	}{
		{"leading tabs", func(s string) string { return "\t\t" + s }},
		{"trailing spaces", func(s string) string { return s + "   " }},
		{"leading tabs and trailing spaces", func(s string) string { return "\t" + s + " \t" }},
		{"no padding (control case)", func(s string) string { return s }},
	}
}

// postWithRawHeader sends a POST with the MCP-Protocol-Version header set to
// exactly hdrVersion — no trimming applied by the test itself, since the
// whole point is to exercise whatever whitespace net/http lets through
// verbatim on the wire — plus whatever extraHeaders the caller supplies.
func postWithRawHeader(t *testing.T, app *fiber.App, key, body, hdrVersion string, extraHeaders map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("POST", mcpURL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", hdrVersion)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return resp
}

// TestMCPHandler_WhitespaceBypass_MatchingMethod_StillModern verifies the
// positive half of the fix: a whitespace-padded MCP-Protocol-Version header —
// with a CLEAN, matching body (both the body's own
// params._meta.protocolVersion and its Mcp-Method) — must still be recognized
// as modern and accepted. This is the realistic shape of the bug even absent
// any malice: a proxy or HTTP library along the way pads the header value in
// transit while the client's body is untouched; that alone must never turn
// into a rejection OR into skipped validation.
func TestMCPHandler_WhitespaceBypass_MatchingMethod_StillModern(t *testing.T) {
	t.Parallel()

	for _, pad := range headerPaddings() {
		t.Run(pad.name, func(t *testing.T) {
			t.Parallel()

			dsn := "file:TestMCPHandler_WhitespaceBypass_MatchingMethod_StillModern_" +
				sanitizeDSNName(pad.name) + "?mode=memory&cache=private"
			app, _, key := setupTestAppWithMCP(t, dsn)

			body := modernMetaRequest(1, "tools/list", string(mcp.V20260728))
			paddedHeader := pad.pad(string(mcp.V20260728))
			resp := postWithRawHeader(t, app, key, body, paddedHeader, map[string]string{"Mcp-Method": "tools/list"})
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusOK {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 200 (a padded header with a clean, matching body and matching "+
					"Mcp-Method must be accepted as modern); body: %s", resp.StatusCode, raw)
			}
			mcpResp := decodeMCPResponse(t, resp.Body)
			if mcpResp.Error != nil {
				t.Fatalf("unexpected protocol error: %+v", mcpResp.Error)
			}
			b, _ := json.Marshal(mcpResp.Result)
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("decode result: %v", err)
			}
			if m["resultType"] != "complete" {
				t.Errorf("resultType = %v, want %q — request must actually have been dispatched as MODERN, "+
					"not silently fallen back to legacy", m["resultType"], "complete")
			}
		})
	}
}

// TestMCPHandler_WhitespaceBypass_MismatchedMethod_Rejected is the
// highest-priority regression test in this file. Before the fix: a
// whitespace-padded MCP-Protocol-Version header made
// isModernProtocolVersion(hdrVersion) return false (Version.Valid() rejects a
// padded string outright), so validateMCPHeaders returned early WITHOUT ever
// checking Mcp-Method against the body — while Negotiate (which trims)
// dispatched the request as modern anyway. A caller could therefore send an
// Mcp-Method that does not match the body's "method" field and have it sail
// through completely unchecked, even though the request really was executed
// as modern. It must now be rejected with HTTP 400 and CodeHeaderMismatch,
// exactly like the equivalent unpadded case.
func TestMCPHandler_WhitespaceBypass_MismatchedMethod_Rejected(t *testing.T) {
	t.Parallel()

	for _, pad := range headerPaddings() {
		t.Run(pad.name, func(t *testing.T) {
			t.Parallel()

			dsn := "file:TestMCPHandler_WhitespaceBypass_MismatchedMethod_Rejected_" +
				sanitizeDSNName(pad.name) + "?mode=memory&cache=private"
			app, _, key := setupTestAppWithMCP(t, dsn)

			// Body is clean and internally consistent (method "tools/list",
			// _meta.protocolVersion "2026-07-28"); only the transport-level
			// Mcp-Method header lies about it. Pre-fix, the untrimmed
			// isModernProtocolVersion(paddedHeader) check returned false and
			// this mismatch was never inspected at all.
			body := modernMetaRequest(1, "tools/list", string(mcp.V20260728))
			paddedHeader := pad.pad(string(mcp.V20260728))
			resp := postWithRawHeader(t, app, key, body, paddedHeader, map[string]string{"Mcp-Method": "tools/call"})
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusBadRequest {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("REGRESSION (whitespace bypass): status = %d, want 400 — a padded header must not let "+
					"a mismatched Mcp-Method skip validation; body: %s", resp.StatusCode, raw)
			}
			mcpResp := decodeMCPErrorBody(t, resp.Body)
			if mcpResp.Error == nil {
				t.Fatal("REGRESSION (whitespace bypass): expected JSON-RPC error, got nil")
			}
			if mcpResp.Error.Code != mcp.CodeHeaderMismatch {
				t.Errorf("REGRESSION (whitespace bypass): Error.Code = %d, want %d (CodeHeaderMismatch)",
					mcpResp.Error.Code, mcp.CodeHeaderMismatch)
			}
		})
	}
}

// TestMCPHandler_WhitespaceBypass_MissingMcpName_Rejected is the tools/call
// variant of the same regression: a padded header must not let a tools/call
// request through with no Mcp-Name at all, even though the header, once
// trimmed, correctly matches the body's protocol version and the body's
// method matches Mcp-Method.
func TestMCPHandler_WhitespaceBypass_MissingMcpName_Rejected(t *testing.T) {
	t.Parallel()

	for _, pad := range headerPaddings() {
		t.Run(pad.name, func(t *testing.T) {
			t.Parallel()

			dsn := "file:TestMCPHandler_WhitespaceBypass_MissingMcpName_Rejected_" +
				sanitizeDSNName(pad.name) + "?mode=memory&cache=private"
			app, _, key := setupTestAppWithMCP(t, dsn)

			body := modernToolCallRequest(1, "list_models")
			paddedHeader := pad.pad(string(mcp.V20260728))
			// Mcp-Method matches, but Mcp-Name is deliberately omitted — the
			// pre-fix bug meant this reached Server.Handle unchecked, since
			// isModernProtocolVersion(paddedHeader) reported "not modern" and
			// validateMCPHeaders bailed out before ever getting to the
			// Mcp-Name presence check.
			resp := postWithRawHeader(t, app, key, body, paddedHeader, map[string]string{"Mcp-Method": "tools/call"})
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusBadRequest {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("REGRESSION (whitespace bypass): status = %d, want 400 — a padded header must not let "+
					"a tools/call request through with no Mcp-Name; body: %s", resp.StatusCode, raw)
			}
			mcpResp := decodeMCPErrorBody(t, resp.Body)
			if mcpResp.Error == nil {
				t.Fatal("REGRESSION (whitespace bypass): expected JSON-RPC error, got nil")
			}
			if mcpResp.Error.Code != mcp.CodeHeaderMismatch {
				t.Errorf("REGRESSION (whitespace bypass): Error.Code = %d, want %d (CodeHeaderMismatch)",
					mcpResp.Error.Code, mcp.CodeHeaderMismatch)
			}
		})
	}
}

// TestMCPHandler_WhitespaceBypass_IdenticallyPaddedHeaderAndBody_StillRejected
// pins down the exploit precondition docs/mcp-v2.md describes verbatim: the
// caller pads BOTH the MCP-Protocol-Version header AND
// params._meta.protocolVersion with the identical whitespace, which is what
// made the OLD, untrimmed header-vs-body equality check in validateMCPHeaders
// pass in the first place (both sides compared equal as raw, padded strings).
// With an Mcp-Method that does not match the body, this must be rejected with
// 400 + CodeHeaderMismatch — regardless of whether the fixed code catches it
// via the header/body value comparison (hdrVersion is now trimmed while
// bodyVersion is read as-is, so identical padding on both sides no longer
// compares equal) or via the Mcp-Method check the original bug skipped. Either
// way, the caller can no longer make Mcp-Method/Mcp-Name validation
// disappear by padding both sides identically.
func TestMCPHandler_WhitespaceBypass_IdenticallyPaddedHeaderAndBody_StillRejected(t *testing.T) {
	t.Parallel()

	for _, pad := range headerPaddings() {
		t.Run(pad.name, func(t *testing.T) {
			t.Parallel()

			dsn := "file:TestMCPHandler_WhitespaceBypass_IdenticallyPaddedHeaderAndBody_StillRejected_" +
				sanitizeDSNName(pad.name) + "?mode=memory&cache=private"
			app, _, key := setupTestAppWithMCP(t, dsn)

			paddedVersion := pad.pad(string(mcp.V20260728))
			body := modernMetaRequest(1, "tools/list", paddedVersion)
			resp := postWithRawHeader(t, app, key, body, paddedVersion, map[string]string{"Mcp-Method": "tools/call"})
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusBadRequest {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("REGRESSION (whitespace bypass, historical exploit precondition): status = %d, want 400; body: %s",
					resp.StatusCode, raw)
			}
			mcpResp := decodeMCPErrorBody(t, resp.Body)
			if mcpResp.Error == nil {
				t.Fatal("REGRESSION (whitespace bypass, historical exploit precondition): expected JSON-RPC error, got nil")
			}
			if mcpResp.Error.Code != mcp.CodeHeaderMismatch {
				t.Errorf("REGRESSION (whitespace bypass, historical exploit precondition): Error.Code = %d, want %d (CodeHeaderMismatch)",
					mcpResp.Error.Code, mcp.CodeHeaderMismatch)
			}
		})
	}
}

// TestMCPHandler_GET_WhitespaceBypass_PaddedModernHeader_Still405 verifies the
// GET-SSE side of the same fix: handleMCPSSE must recognize a
// whitespace-padded modern header as modern (via
// mcp.ProtocolVersionHeaderValue, which trims) and answer 405, exactly like
// an unpadded modern header — not silently fall through to opening the SSE
// stream, which is what the untrimmed comparison used to allow.
func TestMCPHandler_GET_WhitespaceBypass_PaddedModernHeader_Still405(t *testing.T) {
	t.Parallel()

	for _, pad := range headerPaddings() {
		t.Run(pad.name, func(t *testing.T) {
			t.Parallel()

			dsn := "file:TestMCPHandler_GET_WhitespaceBypass_PaddedModernHeader_Still405_" +
				sanitizeDSNName(pad.name) + "?mode=memory&cache=private"
			app, _, key := setupTestAppWithMCP(t, dsn)

			req := httptest.NewRequest("GET", mcpURL, nil)
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("MCP-Protocol-Version", pad.pad(string(mcp.V20260728)))

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusMethodNotAllowed {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("REGRESSION (whitespace bypass): status = %d, want 405 — a padded modern header on GET "+
					"must not fall through to the SSE stream; body: %s", resp.StatusCode, raw)
			}
		})
	}
}

// TestProtocolVersionHeaderValue is a direct unit test of
// mcp.ProtocolVersionHeaderValue — the single point every reader of
// MCP-Protocol-Version (Negotiate, validateMCPHeaders, handleMCPSSE) now goes
// through, so all three agree on the same trimmed value.
func TestProtocolVersionHeaderValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		hdr  mcp.Header
		want string
	}{
		{
			name: "leading and trailing tabs are trimmed",
			hdr:  mcp.MapHeader{mcp.HeaderProtocolVersion: "\t2026-07-28\t"},
			want: "2026-07-28",
		},
		{
			name: "leading and trailing spaces are trimmed",
			hdr:  mcp.MapHeader{mcp.HeaderProtocolVersion: "  2026-07-28  "},
			want: "2026-07-28",
		},
		{
			name: "mixed tabs and spaces are trimmed",
			hdr:  mcp.MapHeader{mcp.HeaderProtocolVersion: " \t 2026-07-28 \t "},
			want: "2026-07-28",
		},
		{
			name: "a clean value with no surrounding whitespace is left unchanged",
			hdr:  mcp.MapHeader{mcp.HeaderProtocolVersion: "2026-07-28"},
			want: "2026-07-28",
		},
		{
			name: "a header carrying only whitespace trims to the empty string",
			hdr:  mcp.MapHeader{mcp.HeaderProtocolVersion: "   \t  "},
			want: "",
		},
		{
			name: "an absent header returns the empty string",
			hdr:  mcp.MapHeader{},
			want: "",
		},
		{
			name: "a nil Header returns the empty string",
			hdr:  nil,
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := mcp.ProtocolVersionHeaderValue(tc.hdr)
			if got != tc.want {
				t.Errorf("ProtocolVersionHeaderValue() = %q, want %q", got, tc.want)
			}
		})
	}
}
