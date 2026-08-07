package admin_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
)

// reservedAuthHeaderCases enumerates auth_header values that name an MCP
// standard request header (docs/mcp-v2.md §4.2, plus the legacy
// Mcp-Session-Id) or the Mcp-Param-* tool-parameter family — every one of
// which mcp_servers.go's isReservedMCPHeader must reject, in any casing, both
// on create and on update. See reservedMCPProtocolHeaders' doc
// (mcp_servers.go) for why: HTTPTransport.Forward (mcp_proxy.go's
// transparent streaming proxy path) sets these headers from the caller's own
// already-validated request, and applies auth_header to the very same
// outbound request — an auth_header configured as one of these names would
// let a misconfigured server registration collide with, and (before the
// ordering fix — see TestForward_AuthHeaderVsMCPHeaderCollision_MCPHeaderWins
// in internal/mcp) potentially override, the MCP protocol header the
// caller's own request depends on.
var reservedAuthHeaderCases = []struct {
	name   string
	header string
}{
	{name: "Mcp-Method", header: "Mcp-Method"},
	{name: "Mcp-Method lowercase", header: "mcp-method"},
	{name: "Mcp-Method uppercase", header: "MCP-METHOD"},
	{name: "MCP-Protocol-Version", header: "MCP-Protocol-Version"},
	{name: "MCP-Protocol-Version lowercase", header: "mcp-protocol-version"},
	{name: "Mcp-Name", header: "Mcp-Name"},
	{name: "Mcp-Name lowercase", header: "mcp-name"},
	{name: "Mcp-Session-Id", header: "Mcp-Session-Id"},
	{name: "Mcp-Session-Id lowercase", header: "mcp-session-id"},
	{name: "Mcp-Param-Foo", header: "Mcp-Param-Foo"},
	{name: "Mcp-Param-Foo lowercase", header: "mcp-param-foo"},
	{name: "Mcp-Param prefix with mixed casing mid-word", header: "Mcp-PARAM-Region"},
}

// mcpServerErrorMessage decodes a mcpServerRequest error response body
// (apierror.Send's envelope: {"error": {"code", "message"}}) and returns the
// message field, for asserting the rejection is understandable rather than a
// bare status code.
func mcpServerErrorMessage(t *testing.T, body io.ReadCloser) string {
	t.Helper()
	got := decodeMCPServerResponse(t, body)
	errObj, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("error response has no \"error\" object: %v", got)
	}
	msg, _ := errObj["message"].(string)
	return msg
}

// ---- Create: reserved auth_header is rejected -------------------------------

// TestCreateMCPServer_API_RejectsReservedAuthHeader verifies that
// CreateMCPServer rejects auth_header values naming a reserved MCP protocol
// header, in any casing, with a 400 and an understandable error — and that an
// ordinary header name (X-API-Key) is still accepted, proving the rejection
// is specific to the reserved set and not an overly broad regression (see
// TestCreateMCPServer_API_OrdinaryAuthHeaderStillAccepted below).
func TestCreateMCPServer_API_RejectsReservedAuthHeader(t *testing.T) {
	t.Parallel()

	for _, tc := range reservedAuthHeaderCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := "file:TestCreateMCPServer_ReservedHeader_" + sanitizeTestName(tc.name) + "?mode=memory&cache=private"
			app, _, keyCache := setupMCPServersTestApp(t, dsn)
			key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-reserved-hdr")

			body := map[string]any{
				"name":        "Reserved Header Server",
				"alias":       "reserved-" + strings.ToLower(sanitizeTestName(tc.name)),
				"url":         "https://example.com",
				"auth_type":   "header",
				"auth_header": tc.header,
				"auth_token":  "secret",
			}

			resp := mcpServerRequest(t, app, http.MethodPost, "/api/v1/mcp-servers", key, body)
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusBadRequest {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 400 for auth_header = %q; body: %s", resp.StatusCode, tc.header, raw)
			}

			msg := mcpServerErrorMessage(t, resp.Body)
			if !strings.Contains(strings.ToLower(msg), "mcp") {
				t.Errorf("error message = %q, want it to mention the MCP protocol header conflict understandably", msg)
			}
		})
	}
}

// TestCreateMCPServer_API_OrdinaryAuthHeaderStillAccepted is the Gegenprobe
// for TestCreateMCPServer_API_RejectsReservedAuthHeader: a normal,
// non-reserved header name must still be accepted.
func TestCreateMCPServer_API_OrdinaryAuthHeaderStillAccepted(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateMCPServer_OrdinaryAuthHeader?mode=memory&cache=private"
	app, _, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-ordinary-hdr")

	body := map[string]any{
		"name":        "Ordinary Header Server",
		"alias":       "ordinary-hdr",
		"url":         "https://example.com",
		"auth_type":   "header",
		"auth_header": "X-API-Key",
		"auth_token":  "secret",
	}

	resp := mcpServerRequest(t, app, http.MethodPost, "/api/v1/mcp-servers", key, body)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body: %s", resp.StatusCode, raw)
	}
}

// ---- Update: reserved auth_header is rejected -------------------------------

// TestUpdateMCPServer_API_RejectsReservedAuthHeader is the update-path
// counterpart of TestCreateMCPServer_API_RejectsReservedAuthHeader: an
// existing server (created with a valid, non-reserved auth_header) must not
// be updatable to a reserved one either.
func TestUpdateMCPServer_API_RejectsReservedAuthHeader(t *testing.T) {
	t.Parallel()

	for _, tc := range reservedAuthHeaderCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := "file:TestUpdateMCPServer_ReservedHeader_" + sanitizeTestName(tc.name) + "?mode=memory&cache=private"
			app, _, keyCache := setupMCPServersTestApp(t, dsn)
			key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-update-reserved-hdr")

			created := createMCPServerViaAPI(t, app, key, map[string]any{
				"name":        "Update Reserved Header Server",
				"alias":       "update-reserved-" + strings.ToLower(sanitizeTestName(tc.name)),
				"url":         "https://example.com",
				"auth_type":   "header",
				"auth_header": "X-Original-Header",
				"auth_token":  "secret",
			})
			serverID := created["id"].(string)

			patch := map[string]any{"auth_header": tc.header}
			resp := mcpServerRequest(t, app, http.MethodPatch, "/api/v1/mcp-servers/"+serverID, key, patch)
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusBadRequest {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 400 for auth_header = %q; body: %s", resp.StatusCode, tc.header, raw)
			}

			msg := mcpServerErrorMessage(t, resp.Body)
			if !strings.Contains(strings.ToLower(msg), "mcp") {
				t.Errorf("error message = %q, want it to mention the MCP protocol header conflict understandably", msg)
			}
		})
	}
}

// TestUpdateMCPServer_API_OrdinaryAuthHeaderStillAccepted is the update-path
// Gegenprobe: an ordinary header name is still accepted by UpdateMCPServer.
func TestUpdateMCPServer_API_OrdinaryAuthHeaderStillAccepted(t *testing.T) {
	t.Parallel()

	dsn := "file:TestUpdateMCPServer_OrdinaryAuthHeader?mode=memory&cache=private"
	app, _, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-update-ordinary-hdr")

	created := createMCPServerViaAPI(t, app, key, map[string]any{
		"name":        "Update Ordinary Header Server",
		"alias":       "update-ordinary-hdr",
		"url":         "https://example.com",
		"auth_type":   "header",
		"auth_header": "X-Original-Header",
		"auth_token":  "secret",
	})
	serverID := created["id"].(string)

	patch := map[string]any{"auth_header": "X-API-Key"}
	resp := mcpServerRequest(t, app, http.MethodPatch, "/api/v1/mcp-servers/"+serverID, key, patch)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
}
