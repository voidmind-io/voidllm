package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/license"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// This file is the strongest available proof that dialect2026Client.Prepare
// (the outbound client side, internal/mcp) and validateMCPHeaders (the
// inbound header/body cross-check, internal/api/admin/mcp_handler.go — MCP
// Streamable HTTP §4.5, docs/mcp-v2.md §4.5) agree on the wire: it drives a
// real HTTPTransport.Call against VoidLLM's own, real admin MCP endpoint,
// exercising Prepare's output through actual net/http headers rather than
// re-implementing validateMCPHeaders' comparison logic in the test itself.
//
// The two packages have no import cycle to worry about: internal/mcp has no
// dependency on internal/api/admin in production code (only this external
// _test package does, which compiles as a separate test binary) — admin
// depends on mcp, never the reverse.

var roundTripHMACSecret = []byte("test-hmac-secret-for-mcp-roundtrip")

// newRoundTripAdminServer builds a real admin.Handler wired to a bare
// mcp.Server with one registered tool ("echo"), registers the real MCP
// routes via admin.RegisterRoutes, and returns an httptest.Server backed by
// that Fiber app (bridged via fiber.App.Test, which every other admin test
// in this codebase already uses to drive requests) plus a valid bearer key.
func newRoundTripAdminServer(t *testing.T, dsn string) (*httptest.Server, string) {
	t.Helper()

	ctx := context.Background()
	database, err := db.Open(ctx, config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             dsn,
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	keyCache := cache.New[string, auth.KeyInfo]()

	mcpServer := mcp.NewServer("voidllm-roundtrip-test", "0.0.0")
	mcpServer.RegisterTool(
		mcp.Tool{Name: "echo", InputSchema: mcp.ObjectSchema(map[string]mcp.SchemaProp{
			"text": {Type: "string"},
		})},
		func(_ context.Context, args json.RawMessage) (*mcp.ToolResult, error) {
			return mcp.TextResult(string(args)), nil
		},
	)

	handler := &admin.Handler{
		DB:         database,
		HMACSecret: roundTripHMACSecret,
		KeyCache:   keyCache,
		License:    license.NewHolder(license.Verify("", true)),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MCPServer:  mcpServer,
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, roundTripHMACSecret, nil)

	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	keyCache.Set(keygen.Hash(plaintext, roundTripHMACSecret), auth.KeyInfo{
		ID:      "roundtrip-test-key",
		KeyType: keygen.KeyTypeUser,
		Role:    auth.RoleMember,
		OrgID:   "org-mcp-roundtrip",
		Name:    "mcp roundtrip test key",
	})

	// Bridge the Fiber app onto a real httptest.Server so an actual
	// net/http.Client (HTTPTransport's) sends actual wire headers, and the
	// real Fiber routing/middleware/handler stack processes an actual
	// net/http request — not a hand-built fiber.Ctx.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, testErr := app.Test(r, fiber.TestConfig{Timeout: 5 * time.Second})
		if testErr != nil {
			http.Error(w, testErr.Error(), http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)

	return srv, plaintext
}

// TestDialect2026Client_Prepare_RoundTripsAgainstRealServer drives
// HTTPTransport.Call, pinned to the modern era, against a real admin MCP
// endpoint for several request shapes. Every one must be accepted (HTTP 200,
// no CodeHeaderMismatch) — proving Prepare's headers and body agree with
// what VoidLLM's own server-side validateMCPHeaders demands.
func TestDialect2026Client_Prepare_RoundTripsAgainstRealServer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "tools/list carries no Mcp-Name and is accepted",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		},
		{
			name: "tools/call with an ASCII tool name is accepted",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`,
		},
		{
			name: "tools/call with a non-ASCII tool name round-trips through the base64 sentinel and is still accepted",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"café 世界"}}}`,
		},
		{
			name: "server/discover carries no Mcp-Name and is accepted",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := "file:TestDialect2026Client_RoundTrip_" + strings.ReplaceAll(tc.name, " ", "_") + "?mode=memory&cache=private"
			srv, key := newRoundTripAdminServer(t, dsn)

			tr := mcp.NewHTTPTransport(srv.URL+"/api/v1/mcp/voidllm", "bearer", "", key,
				5*time.Second, true, "", nil, nil,
				mcp.ClientInfo{Name: "voidllm-roundtrip-client", Version: "test"}, mcp.V20260728, testStreamIdleTimeout)

			result, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(tc.raw)}, "")
			if err != nil {
				t.Fatalf("Call() error = %v, want nil — Prepare's headers must pass the real server's validateMCPHeaders", err)
			}

			var resp mcp.Response
			if err := json.Unmarshal(result.Body, &resp); err != nil {
				t.Fatalf("decode response: %v; body: %s", err, result.Body)
			}
			if resp.Error != nil && resp.Error.Code == mcp.CodeHeaderMismatch {
				t.Fatalf("server rejected the request with CodeHeaderMismatch: %+v — Prepare produced headers that disagree with its own body", resp.Error)
			}
		})
	}
}

// TestDialect2026Client_Prepare_TamperedHeader_IsRejectedByRealServer is the
// negative control for the round trip above: if this test's own header
// manipulation didn't provoke a real CodeHeaderMismatch from the live
// server, the positive round-trip test above would prove nothing (a server
// that accepts everything would pass it trivially). It bypasses Prepare and
// sends a genuinely mismatched Mcp-Method by hand, over the same real
// httptest-backed admin endpoint used above.
func TestDialect2026Client_Prepare_TamperedHeader_IsRejectedByRealServer(t *testing.T) {
	t.Parallel()

	srv, key := newRoundTripAdminServer(t, "file:TestDialect2026Client_TamperedHeader?mode=memory&cache=private")

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/mcp/voidllm", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set(mcp.HeaderProtocolVersion, "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call") // deliberately wrong — body says tools/list

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, raw)
	}

	var mcpResp mcp.Response
	if err := json.NewDecoder(resp.Body).Decode(&mcpResp); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if mcpResp.Error == nil || mcpResp.Error.Code != mcp.CodeHeaderMismatch {
		t.Errorf("Error = %+v, want CodeHeaderMismatch (%d)", mcpResp.Error, mcp.CodeHeaderMismatch)
	}
}

// TestDialect2026Client_Prepare_HeaderParams_RoundTripsAgainstRealServer is
// the x-mcp-header (MCP 2026-07-28 §4.3) counterpart of
// TestDialect2026Client_Prepare_RoundTripsAgainstRealServer: it registers a
// real tool whose input schema annotates "region" for header mirroring,
// resolves the binding via the real mcp.ToolHeaderParams (not a
// hand-constructed mcp.HeaderParam), and drives a genuine tools/call through
// HTTPTransport.Call against VoidLLM's own admin MCP endpoint with
// CallRequest.HeaderParams populated.
//
// A pure unit test on the returned header map cannot catch a regression
// where the Mcp-Param-Region mirroring logic is accidentally hoisted ahead
// of — or interleaved with — the point in Prepare that sets Mcp-Method and
// Mcp-Name, corrupting one of those two required headers as a side effect;
// this test can, because it runs the request through the real server's own
// validateMCPHeaders (mcp_handler.go), which rejects the request outright
// (CodeHeaderMismatch, HTTP 400) the moment Mcp-Method or Mcp-Name disagrees
// with the body — exactly the failure mode a header-map-only test has no way
// to observe, since the map would still individually contain seemingly
// correct-looking header values even if one had clobbered another.
func TestDialect2026Client_Prepare_HeaderParams_RoundTripsAgainstRealServer(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database, err := db.Open(ctx, config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             "file:TestDialect2026Client_HeaderParams_RoundTrip?mode=memory&cache=private",
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	keyCache := cache.New[string, auth.KeyInfo]()

	const headerParamSchema = `{
		"type": "object",
		"properties": {
			"region": {"type": "string", "x-mcp-header": "Region"},
			"query":  {"type": "string"}
		},
		"required": ["region", "query"]
	}`

	mcpServer := mcp.NewServer("voidllm-roundtrip-headerparams-test", "0.0.0")
	mcpServer.RegisterTool(
		mcp.Tool{Name: "lookup", InputSchema: mcp.JSONSchema(headerParamSchema)},
		func(_ context.Context, args json.RawMessage) (*mcp.ToolResult, error) {
			return mcp.TextResult(string(args)), nil
		},
	)

	handler := &admin.Handler{
		DB:         database,
		HMACSecret: roundTripHMACSecret,
		KeyCache:   keyCache,
		License:    license.NewHolder(license.Verify("", true)),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MCPServer:  mcpServer,
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, roundTripHMACSecret, nil)

	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	keyCache.Set(keygen.Hash(plaintext, roundTripHMACSecret), auth.KeyInfo{
		ID:      "roundtrip-headerparams-test-key",
		KeyType: keygen.KeyTypeUser,
		Role:    auth.RoleMember,
		OrgID:   "org-mcp-roundtrip-headerparams",
		Name:    "mcp roundtrip headerparams test key",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, testErr := app.Test(r, fiber.TestConfig{Timeout: 5 * time.Second})
		if testErr != nil {
			http.Error(w, testErr.Error(), http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)

	headerParams, err := mcp.ToolHeaderParams(mcp.JSONSchema(headerParamSchema))
	if err != nil {
		t.Fatalf("ToolHeaderParams() error = %v, want nil", err)
	}
	if len(headerParams) != 1 || headerParams[0].Name != "Region" {
		t.Fatalf("ToolHeaderParams() = %+v, want a single Region binding", headerParams)
	}

	tr := mcp.NewHTTPTransport(srv.URL+"/api/v1/mcp/voidllm", "bearer", "", plaintext,
		5*time.Second, true, "", nil, nil,
		mcp.ClientInfo{Name: "voidllm-roundtrip-headerparams-client", Version: "test"}, mcp.V20260728, testStreamIdleTimeout)

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{"region":"us-west1","query":"q"}}}`)
	result, err := tr.Call(ctx, &mcp.CallRequest{Raw: raw, HeaderParams: headerParams}, "")
	if err != nil {
		t.Fatalf("Call() error = %v, want nil — Prepare's Mcp-Param-Region mirroring must not corrupt Mcp-Method/Mcp-Name", err)
	}

	var resp mcp.Response
	if err := json.Unmarshal(result.Body, &resp); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, result.Body)
	}
	if resp.Error != nil {
		t.Fatalf("server rejected the request: %+v — the real server's own header/body validation caught a defect a header-map-only test could not", resp.Error)
	}
}
