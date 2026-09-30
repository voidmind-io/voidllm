package admin_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// headerValidationCase is one row of the shared Mcp-Name header validation
// table exercised against both the built-in MCP server
// (TestMCPHandler_HeaderValidation_AllThreeMethods) and the external-server
// proxy path (TestMCPProxy_HeaderValidation_AllThreeMethods): a request whose
// Mcp-Method matches the body but whose Mcp-Name header disagrees with,
// omits, or must be base64-sentinel-decoded to match the params field
// mcp.TargetParamKey names for method (docs/mcp-v2.md §4.2, §4.4).
type headerValidationCase struct {
	name    string
	method  string // "tools/call", "prompts/get", or "resources/read"
	bodyVal string // the value carried at params.name or params.uri

	// sendMcpName controls whether the Mcp-Name header is sent at all; when
	// true, mcpNameHeader is the literal header value sent (already
	// sentinel-encoded by the case, where relevant).
	sendMcpName   bool
	mcpNameHeader string

	wantHeaderMismatch bool
}

// headerValidationCases returns the shared table, generating the base64
// sentinel header values at call time (via encoding/base64) rather than
// hardcoding literals, so the expected encoding can never drift from the
// value it is supposed to decode back to.
func headerValidationCases() []headerValidationCase {
	var cases []headerValidationCase

	// mcp.TargetParamKey's three methods: tools/call and prompts/get mirror
	// params.name, resources/read mirrors params.uri instead.
	targets := []struct {
		method  string
		bodyVal string
	}{
		{method: "tools/call", bodyVal: "search"},
		{method: "prompts/get", bodyVal: "greeting"},
		{method: "resources/read", bodyVal: "file:///tmp/report.txt"},
	}

	for _, tgt := range targets {
		sentinel := "=?base64?" + base64.StdEncoding.EncodeToString([]byte(tgt.bodyVal)) + "?="

		cases = append(cases,
			headerValidationCase{
				name:               tgt.method + ": Mcp-Name header matches body, accepted",
				method:             tgt.method,
				bodyVal:            tgt.bodyVal,
				sendMcpName:        true,
				mcpNameHeader:      tgt.bodyVal,
				wantHeaderMismatch: false,
			},
			headerValidationCase{
				name:               tgt.method + ": Mcp-Name header disagrees with body, rejected",
				method:             tgt.method,
				bodyVal:            tgt.bodyVal,
				sendMcpName:        true,
				mcpNameHeader:      tgt.bodyVal + "-does-not-match",
				wantHeaderMismatch: true,
			},
			headerValidationCase{
				name:               tgt.method + ": Mcp-Name header missing entirely, rejected",
				method:             tgt.method,
				bodyVal:            tgt.bodyVal,
				sendMcpName:        false,
				wantHeaderMismatch: true,
			},
			headerValidationCase{
				name:               tgt.method + ": base64-sentinel Mcp-Name header decodes to match, accepted",
				method:             tgt.method,
				bodyVal:            tgt.bodyVal,
				sendMcpName:        true,
				mcpNameHeader:      sentinel,
				wantHeaderMismatch: false,
			},
		)
	}

	return cases
}

// modernTargetRequestBody builds a modern-era JSON-RPC request body for
// method whose params carry both MUST _meta fields (docs/mcp-v2.md §3.2) plus
// a single field — "name" for tools/call and prompts/get, "uri" for
// resources/read — set to val.
func modernTargetRequestBody(method, targetKey, val string) string {
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params": map[string]any{
			targetKey: val,
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		},
	}
	b, _ := json.Marshal(req)
	return string(b)
}

// aliasSafeName replaces characters that are not valid inside a single
// "/api/v1/mcp/:alias" path segment — most importantly "/", since several
// case names in headerValidationCases() start with a method name like
// "tools/call" — so a derived DSN, org slug, or MCP server alias built from a
// case name never accidentally introduces an extra path segment or invalid
// SQLite DSN character.
func aliasSafeName(name string) string {
	r := strings.NewReplacer(" ", "-", "/", "-", ":", "-", ",", "")
	return r.Replace(name)
}

// targetKeyFor mirrors mcp.TargetParamKey for the three methods this table
// covers, panicking (test-only) on anything else so a case with an
// unexpected method fails loudly instead of silently building a malformed
// body.
func targetKeyFor(t *testing.T, method string) string {
	t.Helper()
	key, ok := mcp.TargetParamKey(method)
	if !ok {
		t.Fatalf("mcp.TargetParamKey(%q) ok = false, want true for a method this table covers", method)
	}
	return key
}

// TestMCPHandler_HeaderValidation_AllThreeMethods runs headerValidationCases
// against the built-in server's POST /api/v1/mcp/voidllm endpoint. Unlike the
// existing tools/call-only suite (TestMCPHandler_ModernHeaderMismatch_McpNameVsBody
// et al.), this covers all three methods mcp.TargetParamKey names — including
// prompts/get and resources/read, neither of which the built-in server
// actually implements as a dispatchable method. That is irrelevant here:
// validateMCPHeaders runs purely against the body, before Server.Handle ever
// resolves a method to a handler, so header validation's own accept/reject
// decision is fully exercised regardless of what (if anything) would happen
// next.
func TestMCPHandler_HeaderValidation_AllThreeMethods(t *testing.T) {
	t.Parallel()

	for _, tc := range headerValidationCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestMCPHandler_HeaderValidation_AllThreeMethods_%s?mode=memory&cache=private",
				aliasSafeName(tc.name))
			app, _, key := setupTestAppWithMCP(t, dsn)

			targetKey := targetKeyFor(t, tc.method)
			body := modernTargetRequestBody(tc.method, targetKey, tc.bodyVal)

			headers := map[string]string{"Mcp-Method": tc.method}
			if tc.sendMcpName {
				headers["Mcp-Name"] = tc.mcpNameHeader
			}

			resp := mcpPostModern(t, app, key, body, headers)
			defer resp.Body.Close()

			if tc.wantHeaderMismatch {
				if resp.StatusCode != fiber.StatusBadRequest {
					raw, _ := io.ReadAll(resp.Body)
					t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, raw)
				}
				mcpResp := decodeMCPErrorBody(t, resp.Body)
				if mcpResp.Error == nil || mcpResp.Error.Code != mcp.CodeHeaderMismatch {
					t.Errorf("Error = %+v, want CodeHeaderMismatch (%d)", mcpResp.Error, mcp.CodeHeaderMismatch)
				}
				return
			}

			// Accepted by header validation: the request must NOT be a
			// CodeHeaderMismatch, regardless of whatever the method dispatch
			// itself then does (unimplemented methods here still correctly
			// surface as CodeMethodNotFound, not as a header-validation
			// failure).
			if resp.StatusCode == fiber.StatusBadRequest {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = 400, want header validation to accept this request; body: %s", raw)
			}
		})
	}
}

// TestMCPProxy_HeaderValidation_AllThreeMethods runs the SAME
// headerValidationCases table against the transparent-intermediary proxy
// path (/:alias, HandleMCPProxy) rather than only the built-in server, per
// docs/mcp-v2.md §4.5's "intermediaries must validate too" requirement (FIX
// 4) — a header/body disagreement must be rejected before ever reaching the
// upstream, and a valid request must be forwarded exactly once.
func TestMCPProxy_HeaderValidation_AllThreeMethods(t *testing.T) {
	t.Parallel()

	for _, tc := range headerValidationCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var upstreamHits int
			var mu sync.Mutex
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				upstreamHits++
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			}))
			t.Cleanup(upstream.Close)

			dsn := fmt.Sprintf("file:TestMCPProxy_HeaderValidation_AllThreeMethods_%s?mode=memory&cache=private",
				aliasSafeName(tc.name))
			app, database, keyCache := setupMCPProxyApp(t, dsn)
			org := mustCreateTestOrg(t, database, "proxy-headerval-"+aliasSafeName(tc.name))
			memberKey := addMCPTestKey(t, keyCache, org.ID)

			alias := "headerval-" + aliasSafeName(tc.name)
			s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
			if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
				t.Fatalf("SetOrgMCPAccess: %v", err)
			}

			targetKey := targetKeyFor(t, tc.method)
			body := modernTargetRequestBody(tc.method, targetKey, tc.bodyVal)

			headers := map[string]string{
				"MCP-Protocol-Version": "2026-07-28",
				"Mcp-Method":           tc.method,
			}
			if tc.sendMcpName {
				headers["Mcp-Name"] = tc.mcpNameHeader
			}

			resp := proxyPostWithHeaders(t, app, alias, memberKey, body, headers)
			defer resp.Body.Close()

			if tc.wantHeaderMismatch {
				if resp.StatusCode != fiber.StatusBadRequest {
					raw, _ := io.ReadAll(resp.Body)
					t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, raw)
				}
				mcpResp := decodeMCPErrorBody(t, resp.Body)
				if mcpResp.Error == nil || mcpResp.Error.Code != mcp.CodeHeaderMismatch {
					t.Errorf("Error = %+v, want CodeHeaderMismatch (%d)", mcpResp.Error, mcp.CodeHeaderMismatch)
				}

				mu.Lock()
				hits := upstreamHits
				mu.Unlock()
				if hits != 0 {
					t.Errorf("upstream received %d requests, want 0 (rejected before ever reaching upstream)", hits)
				}
				return
			}

			if resp.StatusCode != fiber.StatusOK {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 200 (forwarded upstream); body: %s", resp.StatusCode, raw)
			}
			mu.Lock()
			hits := upstreamHits
			mu.Unlock()
			if hits != 1 {
				t.Errorf("upstream received %d requests, want exactly 1", hits)
			}
		})
	}
}
