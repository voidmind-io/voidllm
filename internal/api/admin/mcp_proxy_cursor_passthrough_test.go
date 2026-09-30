package admin_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// TestMCPProxy_ToolsListCursor_PassedThroughByteIdentical verifies the
// intermediary /api/v1/mcp/:alias route: HandleMCPProxy is a transparent
// pass-through (transport.Forward — see mcp_proxy.go's own doc), never the
// pagination-aware HTTPTransport.ListTools client. A tools/list request
// carrying params.cursor must reach the upstream byte-identical, and the
// upstream's own nextCursor in its response must reach the caller
// byte-identical too — VoidLLM neither validates nor rewrites cursor content
// on this path, unlike the cursor rejection built-in Server.dispatch applies
// to its OWN "voidllm" alias (see internal/mcp's
// TestServer_ToolsList_CursorParam_RejectedBothEras).
func TestMCPProxy_ToolsListCursor_PassedThroughByteIdentical(t *testing.T) {
	t.Parallel()

	const cursorSentinel = "opaque-cursor-value-SENTINEL-abc123"
	const nextCursorSentinel = "opaque-next-cursor-SENTINEL-xyz789"

	requestBody := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":%q}}`, cursorSentinel)
	upstreamResponse := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","inputSchema":{"type":"object"}}],"nextCursor":%q}}`, nextCursorSentinel)

	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read err", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, upstreamResponse)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_ToolsListCursor_PassedThroughByteIdentical?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "proxy-cursor-passthrough")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	s := createExternalMCPServer(t, database, "cursor-server", upstream.URL)
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPost(t, app, "cursor-server", memberKey, requestBody)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	// The upstream must have received the request body byte-identical,
	// cursor included.
	if string(gotBody) != requestBody {
		t.Errorf("upstream received body =\n%s\nwant byte-identical to:\n%s", gotBody, requestBody)
	}
	if !strings.Contains(string(gotBody), cursorSentinel) {
		t.Errorf("upstream request body missing the cursor sentinel entirely: %s", gotBody)
	}

	// The caller must receive the upstream's response body byte-identical,
	// nextCursor included.
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(raw) != upstreamResponse {
		t.Errorf("response body =\n%s\nwant byte-identical to the upstream's response:\n%s", raw, upstreamResponse)
	}
	if !strings.Contains(string(raw), nextCursorSentinel) {
		t.Errorf("response body missing the nextCursor sentinel entirely: %s", raw)
	}
}
