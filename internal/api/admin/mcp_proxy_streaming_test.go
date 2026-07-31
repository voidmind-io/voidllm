package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/license"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers the full HTTP round trip through HandleMCPProxy
// (mcp_proxy.go) for the properties the streaming rewrite's response-header
// allowlist, Content-Type ownership, and error-passthrough contracts promise
// — as opposed to forward_streaming_test.go (internal/mcp), which drives
// HTTPTransport.Forward directly. Every subtest here is table-driven and uses
// a real httptest.Server as the upstream, exactly like the rest of this
// package's MCP proxy tests.

// setupMCPProxyAppWithStreamIdleTimeout is like setupMCPProxyApp but sets
// Handler.MCPStreamIdleTimeout explicitly, for tests that need to observe
// Forward's idle-timeout behavior end to end through the real proxy handler
// (HandleMCPProxy → buildAdHocTransport → mcp.NewHTTPTransport) instead of at
// forward_streaming_test.go's HTTPTransport.Forward level. Every other test
// in this package leaves it at its zero value (120s default), which is why
// this dedicated helper exists rather than adding a parameter to
// setupMCPProxyApp itself.
func setupMCPProxyAppWithStreamIdleTimeout(t *testing.T, dsn string, idle time.Duration) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo]) {
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

	handler := &admin.Handler{
		DB:            database,
		HMACSecret:    testHMACSecret,
		EncryptionKey: testEncryptionKey,
		KeyCache:      keyCache,
		License:       license.NewHolder(license.Verify("", true)),
		Log:           noopLogger(t),
		// RegisterRoutes only mounts /api/v1/mcp/:alias when MCPServer is
		// non-nil (routes.go) — a bare server with no registered tools is
		// enough, since every test using this helper only proxies to an
		// EXTERNAL server, never to the built-in "voidllm" alias.
		MCPServer:            mcp.NewServer("voidllm", "test"),
		MCPCallTimeout:       5 * time.Second,
		MCPStreamIdleTimeout: idle,
		MCPAllowPrivateURLs:  true, // tests use loopback httptest servers
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return app, database, keyCache
}

// setupMCPProxyAppWithMaxBytes is like setupMCPProxyApp but sets
// Handler.MCPStreamMaxBytes explicitly (0 leaves it at the Handler zero
// value — unbounded), for tests that need to observe
// settings.mcp.stream_max_bytes enforcement end to end through the real
// proxy handler. See mcp_proxy_bytelimit_test.go.
func setupMCPProxyAppWithMaxBytes(t *testing.T, dsn string, maxBytes int64) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo]) {
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

	handler := &admin.Handler{
		DB:                  database,
		HMACSecret:          testHMACSecret,
		EncryptionKey:       testEncryptionKey,
		KeyCache:            keyCache,
		License:             license.NewHolder(license.Verify("", true)),
		Log:                 noopLogger(t),
		MCPServer:           mcp.NewServer("voidllm", "test"),
		MCPCallTimeout:      5 * time.Second,
		MCPStreamMaxBytes:   maxBytes,
		MCPAllowPrivateURLs: true,
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return app, database, keyCache
}

// setupMCPProxyAppWithRequestID is like setupMCPProxyApp but additionally
// wires apierror.RequestIDMiddleware() as global middleware, ahead of
// admin.RegisterRoutes, exactly as the real Application does at the top
// level (internal/app/routes.go — admin.RegisterRoutes itself never wires
// this middleware; every other helper in this package deliberately omits it
// since none of their assertions care about X-Request-Id). Tests using this
// helper need the real middleware in the chain to observe VoidLLM's own
// X-Request-Id value winning a name collision with the upstream's own copy
// of the same header — see TestMCPProxy_XRequestID_UpstreamNeverLeaks_OwnIDExactlyOnce.
func setupMCPProxyAppWithRequestID(t *testing.T, dsn string) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo]) {
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

	handler := &admin.Handler{
		DB:                  database,
		HMACSecret:          testHMACSecret,
		EncryptionKey:       testEncryptionKey,
		KeyCache:            keyCache,
		License:             license.NewHolder(license.Verify("", true)),
		Log:                 noopLogger(t),
		MCPServer:           mcp.NewServer("voidllm", "test"),
		MCPCallTimeout:      5 * time.Second,
		MCPAllowPrivateURLs: true,
	}

	app := fiber.New()
	app.Use(apierror.RequestIDMiddleware())
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return app, database, keyCache
}

// sanitizeTestName replaces characters that are awkward in a SQLite DSN or an
// MCP server alias with underscores/hyphens, mirroring the existing
// convention in TestMCPProxy_HeaderValidation and
// TestMCPProxy_ValidMCPMethods_ModernEraMethodsNotLabeledUnknown.
func sanitizeTestName(name string) string {
	r := strings.NewReplacer(" ", "-", "/", "-", ",", "", "*", "star", ":", "-", "(", "", ")", "", ".", "-")
	return r.Replace(name)
}

// ---- Response header allowlist, table-driven -------------------------------

// TestMCPProxy_ResponseHeaderAllowlist is the full-stack, table-driven
// counterpart of internal/api/admin/mcp_headers.go's allowedMCPResponseHeaders
// and mcpHopByHopHeaders: every header on the allowlist must reach the
// caller with every value the upstream sent, X-RateLimit-* must pass by
// prefix in either canonical casing, and everything else — hop-by-hop
// headers, Server, X-Powered-By, and any other header outside the allowlist
// — must never reach the caller.
func TestMCPProxy_ResponseHeaderAllowlist(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		setHeaders  func(w http.ResponseWriter)
		wantPresent map[string][]string // canonical header name -> exact wanted values
		wantAbsent  []string            // canonical header names that must not appear at all
		// wantStatus is the expected response status. Zero means the table's
		// default, fiber.StatusOK — every case reaches the caller normally
		// except the "Content-Encoding is rejected" case below, whose upstream
		// response never becomes a *ForwardResult at all (see that case's doc).
		wantStatus int
	}{
		{
			name:        "Content-Type passes through",
			setHeaders:  func(http.ResponseWriter) {},
			wantPresent: map[string][]string{"Content-Type": {"application/json"}},
		},
		{
			name: "WWW-Authenticate passes through",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="mcp", error="invalid_token"`)
			},
			wantPresent: map[string][]string{"Www-Authenticate": {`Bearer realm="mcp", error="invalid_token"`}},
		},
		{
			name: "Retry-After passes through",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Set("Retry-After", "30")
			},
			wantPresent: map[string][]string{"Retry-After": {"30"}},
		},
		{
			// This case used to be named "X-Request-ID passes through" and
			// asserted the opposite: that the upstream's own X-Request-ID
			// value reached the caller. That was a duplication bug —
			// RequestIDMiddleware (internal/apierror/requestid.go) already
			// sets X-Request-ID exactly once for every VoidLLM response,
			// including this one, and THAT id — not the upstream's own,
			// unrelated value under the same header name — is the one that
			// correlates across VoidLLM's own logs and metrics for this
			// request. VoidLLM's own id wins: allowedMCPResponseHeaders
			// (mcp_headers.go) deliberately does not list X-Request-ID, so
			// copyMCPResponseHeaders never mirrors the upstream's copy, and
			// the caller sees VoidLLM's id exactly once instead of two
			// different values under the same header name (docs/mcp-v2.md,
			// review finding C3 — the same class of duplication bug already
			// fixed for Mcp-Session-Id, see
			// TestMCPProxy_SessionIDHeaderMirroredExactlyOnce below).
			name: "X-Request-ID is not mirrored from the upstream, VoidLLM own id wins",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Set("X-Request-ID", "upstream-req-id-must-not-leak")
			},
			wantAbsent: []string{"X-Request-ID"},
		},
		{
			name: "Cache-Control passes through",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Set("Cache-Control", "public, max-age=3600")
			},
			wantPresent: map[string][]string{"Cache-Control": {"public, max-age=3600"}},
		},
		{
			// This case used to be named "Content-Encoding passes through"
			// and asserted the opposite: that an upstream's own
			// Content-Encoding: br reached the caller unchanged. That was
			// the exact decompression-bomb hole review finding B closed —
			// Content-Encoding is no longer on allowedMCPResponseHeaders at
			// all (mcp_headers.go), and mcp.HTTPTransport.Forward now
			// rejects any Content-Encoding it still sees after Go's own
			// transparent gzip handling as UNSOLICITED, failing the call
			// outright instead of streaming the encoded bytes through (see
			// Forward's doc, http_transport.go) — HandleMCPProxy turns that
			// callErr into a plain 502, never a *ForwardResult, so this
			// header can never reach copyMCPResponseHeaders in the first
			// place.
			//
			// Why this matters: settings.mcp.stream_max_bytes
			// (h.MCPStreamMaxBytes) counts WIRE bytes as they cross this
			// proxy. The real MCP client on the other side of this proxy
			// never negotiated "br" either (VoidLLM's own request never
			// sets Accept-Encoding), so it has no way to decode it — but if
			// VoidLLM forwarded it anyway, a malicious or misconfigured
			// upstream could send a small, byte-limit-compliant
			// "br"-encoded body that decompresses into gigabytes on the
			// CLIENT's side: a decompression bomb that sails straight under
			// a wire-byte ceiling designed to stop exactly this class of
			// attack. Rejecting the call outright, before any response body
			// is ever forwarded, is what keeps that ceiling meaningful. See
			// mcp_proxy_compression_test.go for the full-stack 502 coverage
			// and the companion "gzip still works" regression coverage.
			//
			// "br" (Brotli), not "gzip", deliberately: Go's own
			// net/http.Transport auto-negotiates gzip on every outbound
			// request that does not set its own Accept-Encoding (neither
			// rawPost nor Forward do), and transparently decompresses a
			// gzip-encoded response, DELETING Content-Encoding/Content-Length
			// from the response itself as part of that (documented
			// net/http.Transport behavior, not a VoidLLM bug) — so a real
			// upstream encoding gzip by default is unaffected by this fix.
			// Brotli is never auto-negotiated, so it still reaches Forward's
			// own unsolicited-encoding check unmodified, exercising the
			// rejection this case is actually about.
			name: "Content-Encoding is rejected as an unsolicited encoding, not mirrored",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Set("Content-Encoding", "br")
			},
			wantAbsent: []string{"Content-Encoding"},
			wantStatus: fiber.StatusBadGateway,
		},
		{
			name: "Content-Language passes through",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Set("Content-Language", "en-US")
			},
			wantPresent: map[string][]string{"Content-Language": {"en-US"}},
		},
		{
			name: "X-RateLimit-* passes through by prefix",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Set("X-RateLimit-Limit", "100")
				w.Header().Set("X-RateLimit-Remaining", "42")
			},
			wantPresent: map[string][]string{
				"X-Ratelimit-Limit":     {"100"},
				"X-Ratelimit-Remaining": {"42"},
			},
		},
		{
			name: "X-Ratelimit-* (already-lowercased-hyphen form) also passes through by prefix",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Set("X-Ratelimit-Reset", "60")
			},
			wantPresent: map[string][]string{"X-Ratelimit-Reset": {"60"}},
		},
		{
			name: "multi-valued header keeps every value",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Add("X-RateLimit-Policy", "org;q=10")
				w.Header().Add("X-RateLimit-Policy", "key;q=5")
			},
			wantPresent: map[string][]string{"X-Ratelimit-Policy": {"org;q=10", "key;q=5"}},
		},
		{
			name: "hop-by-hop headers are dropped",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Set("Connection", "keep-alive")
				w.Header().Set("Keep-Alive", "timeout=5")
				w.Header().Set("Proxy-Authenticate", "Basic")
				w.Header().Set("Proxy-Authorization", "Basic xyz")
				w.Header().Set("TE", "trailers")
				w.Header().Set("Trailer", "X-Foo")
			},
			wantAbsent: []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer"},
		},
		{
			name: "Server and X-Powered-By are dropped",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Set("Server", "some-internal-server/1.2.3")
				w.Header().Set("X-Powered-By", "Express")
			},
			wantAbsent: []string{"Server", "X-Powered-By"},
		},
		{
			name: "an unrelated header outside the allowlist is dropped",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Set("X-Internal-Debug-Token", "should-not-leak")
			},
			wantAbsent: []string{"X-Internal-Debug-Token"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				tc.setHeaders(w)
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			}))
			t.Cleanup(upstream.Close)

			alias := "hdr-" + sanitizeTestName(tc.name)
			dsn := "file:TestMCPProxy_ResponseHeaderAllowlist_" + sanitizeTestName(tc.name) + "?mode=memory&cache=private"
			app, database, keyCache := setupMCPProxyApp(t, dsn)
			org := mustCreateTestOrg(t, database, alias)
			memberKey := addMCPTestKey(t, keyCache, org.ID)

			s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
			if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
				t.Fatalf("SetOrgMCPAccess: %v", err)
			}

			resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
			defer resp.Body.Close()

			wantStatus := tc.wantStatus
			if wantStatus == 0 {
				wantStatus = fiber.StatusOK
			}
			if resp.StatusCode != wantStatus {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, wantStatus, raw)
			}

			for name, want := range tc.wantPresent {
				if got := resp.Header.Values(name); !reflect.DeepEqual(got, want) {
					t.Errorf("header %q = %v, want %v", name, got, want)
				}
			}
			for _, name := range tc.wantAbsent {
				if got := resp.Header.Values(name); len(got) != 0 {
					t.Errorf("header %q = %v, want absent entirely", name, got)
				}
			}
		})
	}
}

// ---- X-Request-Id: VoidLLM's own id, exactly once --------------------------

// TestMCPProxy_XRequestID_UpstreamNeverLeaks_OwnIDExactlyOnce is the
// full-stack counterpart of the "X-Request-ID is NOT mirrored from the
// upstream" case in TestMCPProxy_ResponseHeaderAllowlist above, driven
// through a Fiber app that also wires apierror.RequestIDMiddleware() — the
// real middleware that sets X-Request-Id for every VoidLLM response — so this
// test can additionally prove the positive half of that contract: the caller
// still receives exactly one X-Request-Id header, carrying VoidLLM's own
// value, never the upstream's.
func TestMCPProxy_XRequestID_UpstreamNeverLeaks_OwnIDExactlyOnce(t *testing.T) {
	t.Parallel()

	const poisonUpstreamValue = "upstream-req-id-must-not-leak"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", poisonUpstreamValue)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_XRequestID_UpstreamNeverLeaks?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyAppWithRequestID(t, dsn)
	org := mustCreateTestOrg(t, database, "xreqid")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	s := createExternalMCPServerPinned(t, database, "xreqid-server", upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPost(t, app, "xreqid-server", memberKey, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	got := resp.Header.Values("X-Request-Id")
	if len(got) != 1 {
		t.Fatalf("X-Request-Id values = %#v, want exactly one value", got)
	}
	if got[0] == poisonUpstreamValue {
		t.Errorf("X-Request-Id = %q, want VoidLLM's own id, never the upstream's", got[0])
	}
	if got[0] == "" {
		t.Error("X-Request-Id is empty, want a non-empty id set by RequestIDMiddleware")
	}
}

// ---- X-Accel-Buffering: set only for an SSE response, never for JSON ------

// TestMCPProxy_XAccelBuffering_SetOnlyForSSEContentType verifies
// mcp_proxy.go's own c.Set("X-Accel-Buffering", "no"): it must be set when
// the UPSTREAM's Content-Type is text/event-stream (telling a reverse proxy
// sitting in front of VoidLLM not to buffer the stream, or the whole
// streaming rewrite is defeated at that hop — see mcp_proxy.go's comment),
// and must NOT be set for an ordinary application/json response, which is
// never streamed chunk-by-chunk and has no such buffering concern.
func TestMCPProxy_XAccelBuffering_SetOnlyForSSEContentType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		upstreamCT        string
		upstreamBody      string
		wantAccelBuffered bool
	}{
		{
			name:              "text/event-stream upstream: X-Accel-Buffering: no is set",
			upstreamCT:        "text/event-stream",
			upstreamBody:      "event: message\n" + `data: {"jsonrpc":"2.0","id":1,"result":{}}` + "\n\n",
			wantAccelBuffered: true,
		},
		{
			name:              "application/json upstream: X-Accel-Buffering is NOT set",
			upstreamCT:        "application/json",
			upstreamBody:      `{"jsonrpc":"2.0","id":1,"result":{}}`,
			wantAccelBuffered: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.upstreamCT)
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, tc.upstreamBody)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}))
			t.Cleanup(upstream.Close)

			alias := "accel-" + sanitizeTestName(tc.name)
			dsn := "file:TestMCPProxy_XAccelBuffering_" + sanitizeTestName(tc.name) + "?mode=memory&cache=private"
			app, database, keyCache := setupMCPProxyApp(t, dsn)
			org := mustCreateTestOrg(t, database, alias)
			memberKey := addMCPTestKey(t, keyCache, org.ID)

			s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
			if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
				t.Fatalf("SetOrgMCPAccess: %v", err)
			}

			// A caller sending Accept: both media types (the modern,
			// spec-compliant shape) so the upstream's own Content-Type is
			// what decides the response shape here — not the legacy-wrap
			// branch.
			resp := proxyPostWithHeaders(t, app, alias, memberKey,
				`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
				map[string]string{"Accept": "application/json, text/event-stream"})
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusOK {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
			}

			got := resp.Header.Get("X-Accel-Buffering")
			if tc.wantAccelBuffered {
				if got != "no" {
					t.Errorf("X-Accel-Buffering = %q, want %q for an SSE upstream response", got, "no")
				}
			} else if got != "" {
				t.Errorf("X-Accel-Buffering = %q, want absent for a non-SSE (application/json) upstream response", got)
			}
		})
	}
}

// TestMCPProxy_SessionIDHeaderMirroredExactlyOnce guards against a regression
// found while writing the header-allowlist coverage above:
// mcp.HeaderSessionID ("Mcp-Session-Id") must be mirrored to the caller
// EXACTLY ONCE on the transparent proxy path, with the upstream's value
// unchanged.
//
// Previously, mcp_proxy.go explicitly did `c.Set(mcp.HeaderSessionID, sid)`
// once, using session-mirroring logic that predates the streaming rewrite —
// and mcp_headers.go's allowedMCPResponseHeaders ALSO listed
// mcp.HeaderSessionID, so the later copyMCPResponseHeaders call did
// `c.Response().Header.Add(name, v)` for it a SECOND time with the SAME
// value. Because Add appends rather than replacing, the caller ended up
// receiving Mcp-Session-Id twice on the wire with two identical values.
// Some HTTP client libraries — including the Fetch API's Headers.get(),
// which several MCP TypeScript SDK transports are built on — coalesce
// repeated header values with ", " when read back, which corrupted the
// session ID a legacy client sees into "sid, sid" rather than "sid".
//
// allowedMCPResponseHeaders (mcp_headers.go) is now the single source of
// truth for which response headers are mirrored; there is no longer a
// second, explicit c.Set alongside it.
func TestMCPProxy_SessionIDHeaderMirroredExactlyOnce(t *testing.T) {
	t.Parallel()

	const sessionID = "sess-XYZ-mirrored-once"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(mcp.HeaderSessionID, sessionID)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_SessionIDHeaderMirroredExactlyOnce?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "sid-mirrored-once")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	s := createExternalMCPServerPinned(t, database, "sid-mirrored-once-server", upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPost(t, app, "sid-mirrored-once-server", memberKey, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	got := resp.Header.Values(mcp.HeaderSessionID)
	want := []string{sessionID}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Mcp-Session-Id values = %#v, want %#v (mirrored exactly once, with the upstream's value "+
			"unchanged)", got, want)
	}
}

// ---- Content-Type belongs to the upstream ----------------------------------

// TestMCPProxy_ContentType_BelongsToUpstream_NotOverriddenByAcceptSSE verifies
// the property that motivated removing HandleMCPProxy's old Accept-based
// branch entirely: a conformant modern client's Accept header MUST list both
// application/json and text/event-stream (docs/mcp-v2.md §4.1) — the upstream
// alone decides which one it actually answers with, and the proxy must not
// substitute its own choice based on what the client would also accept.
func TestMCPProxy_ContentType_BelongsToUpstream_NotOverriddenByAcceptSSE(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_ContentType_BelongsToUpstream?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "content-type-ownership")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	s := createExternalMCPServerPinned(t, database, "content-type-server", upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPostWithHeaders(t, app, "content-type-server", memberKey,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		map[string]string{"Accept": "application/json, text/event-stream"})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json prefix (the upstream's own choice)", ct)
	}
	if strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, must not contain text/event-stream — the proxy must never substitute "+
			"its own format decision for the upstream's, even though the client's Accept header would also "+
			"allow it", ct)
	}
}

// ---- Error status with headers ---------------------------------------------

// TestMCPProxy_ErrorStatusWithHeaders_ArrivesWithHeaders is table-driven
// coverage for two of the allowlisted headers that specifically exist to make
// a non-2xx upstream response actionable by the real caller on the other
// side of the proxy: WWW-Authenticate on a 401, and Retry-After on a 429.
// Both the status and the header must reach the caller unchanged, exactly as
// the byte-identical body does (see TestMCPProxy_Forward_UpstreamErrorStatus_PassedThroughUnchanged
// in mcp_proxy_test.go for the body-only counterpart).
func TestMCPProxy_ErrorStatusWithHeaders_ArrivesWithHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		headerName  string
		headerValue string
		body        string
	}{
		{
			name:        "401 with WWW-Authenticate",
			status:      http.StatusUnauthorized,
			headerName:  "WWW-Authenticate",
			headerValue: `Bearer realm="mcp", error="invalid_token"`,
			body:        `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"unauthorized"}}`,
		},
		{
			name:        "429 with Retry-After",
			status:      http.StatusTooManyRequests,
			headerName:  "Retry-After",
			headerValue: "30",
			body:        `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"rate limited"}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set(tc.headerName, tc.headerValue)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(upstream.Close)

			alias := "err-hdr-" + sanitizeTestName(tc.name)
			dsn := "file:TestMCPProxy_ErrorStatusWithHeaders_" + sanitizeTestName(tc.name) + "?mode=memory&cache=private"
			app, database, keyCache := setupMCPProxyApp(t, dsn)
			org := mustCreateTestOrg(t, database, alias)
			memberKey := addMCPTestKey(t, keyCache, org.ID)

			s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
			if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
				t.Fatalf("SetOrgMCPAccess: %v", err)
			}

			resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`)
			defer resp.Body.Close()

			if resp.StatusCode != tc.status {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, tc.status, raw)
			}
			if got := resp.Header.Get(tc.headerName); got != tc.headerValue {
				t.Errorf("%s = %q, want %q", tc.headerName, got, tc.headerValue)
			}

			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if string(raw) != tc.body {
				t.Errorf("body = %q, want byte-identical to the upstream's error body: %q", raw, tc.body)
			}
		})
	}
}

// ---- Error before the first byte: an ordinary error response, no ----------
// ---- half-open stream --------------------------------------------------------

// TestMCPProxy_UpstreamUnreachable_ReturnsOrdinaryErrorResponse_NoHalfOpenStream
// verifies Forward's doc: a failure that happens BEFORE the upstream ever
// answers (connection refused, here — nothing is listening on the target
// address at all) must reach the caller as an ordinary, complete error
// response, not a stream that opens and then hangs or truncates.
func TestMCPProxy_UpstreamUnreachable_ReturnsOrdinaryErrorResponse_NoHalfOpenStream(t *testing.T) {
	t.Parallel()

	// Bind an ephemeral port and immediately release it: the resulting URL
	// reliably has nothing listening on it (barring an implausible race with
	// some other process claiming the exact same port in between), giving a
	// deterministic connection-refused failure without depending on any
	// externally-reserved "known unreachable" address.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve ephemeral port: %v", err)
	}
	unreachableURL := "http://" + ln.Addr().String() + "/mcp"
	if err := ln.Close(); err != nil {
		t.Fatalf("release ephemeral port: %v", err)
	}

	dsn := "file:TestMCPProxy_UpstreamUnreachable?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "upstream-unreachable")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	s := createExternalMCPServerPinned(t, database, "unreachable-server", unreachableURL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPost(t, app, "unreachable-server", memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadGateway {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 502 (an ordinary, complete error response); body: %s", resp.StatusCode, raw)
	}

	var rpcResp mcp.Response
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v — a pre-first-byte failure must produce a COMPLETE response body, not a "+
			"truncated or half-open one", err)
	}
	if err := json.Unmarshal(raw, &rpcResp); err != nil {
		t.Fatalf("decode error body: %v; raw: %s", err, raw)
	}
	if rpcResp.Error == nil {
		t.Error("Error field is nil, want a non-nil JSON-RPC error for an unreachable upstream")
	}
}

// ---- Progress before Result: full-stack proof the client gets both --------

// TestMCPProxy_SSE_ProgressBeforeResult_ClientReceivesBothEventsInOrder is
// the full-stack counterpart of internal/mcp's
// TestForward_SSE_ProgressBeforeResult_BothEventsDeliveredInOrder, driven all
// the way through the real HTTP proxy handler (HandleMCPProxy →
// SendStreamWriter → flushingWriter → io.Copy), not just HTTPTransport.Forward
// in isolation. This is "the test that proves the bug" at the layer that
// actually matters to a real MCP client sitting on the other side of
// /api/v1/mcp/:alias: against the pre-rewrite implementation (rawPost →
// io.ReadAll → extractSSEData, which returns only the first "data:" line),
// the client would have received ONLY the progress notification and the
// tools/call result would have been silently discarded.
func TestMCPProxy_SSE_ProgressBeforeResult_ClientReceivesBothEventsInOrder(t *testing.T) {
	t.Parallel()

	const progressEvent = "event: message\n" +
		`data: {"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"tok-1","progress":50,"total":100}}` +
		"\n\n"
	const resultEvent = "event: message\n" +
		`data: {"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","content":[{"type":"text","text":"done"}]}}` +
		"\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, progressEvent)
		flusher.Flush()
		fmt.Fprint(w, resultEvent)
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_SSE_ProgressBeforeResult?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "progress-before-result")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	s := createExternalMCPServerPinned(t, database, "progress-before-result-server", upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPost(t, app, "progress-before-result-server", memberKey,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	want := progressEvent + resultEvent
	if string(raw) != want {
		t.Errorf("client received:\n%s\nwant BOTH events, in order:\n%s\n\n"+
			"(against the pre-rewrite rawPost/extractSSEData path, the client would have received only "+
			"the progress notification — the result would have been silently discarded)", raw, want)
	}
}

// ---- Mid-stream abort: the stream ends, nothing is invented ---------------

// TestMCPProxy_MidStreamAbort_EndsCleanly_NoSyntheticEvent verifies the other
// half of Forward's error-handling contract (docs/mcp-v2.md §3.9): once
// headers have arrived and streaming has begun, an upstream connection that
// drops mid-stream must simply end the caller's stream at whatever point it
// broke — no synthetic error/abort event is invented on the caller's behalf,
// unlike the LLM proxy's unrelated abort-event behavior. The spec places the
// retry obligation entirely on the caller: "Bricht ein Response-Stream, ist
// der Request verloren und MUSS als neuer Request mit neuer ID wiederholt
// werden."
func TestMCPProxy_MidStreamAbort_EndsCleanly_NoSyntheticEvent(t *testing.T) {
	t.Parallel()

	const firstChunk = "event: message\n" +
		`data: {"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":1}}` +
		"\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, firstChunk)
		w.(http.Flusher).Flush()

		// Simulate an abrupt disconnect (as opposed to a clean end of body):
		// hijack the underlying connection and close it directly, without
		// writing whatever transfer-encoding terminator net/http would
		// otherwise send when the handler returns normally.
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, hijackErr := hj.Hijack()
		if hijackErr != nil {
			return
		}
		conn.Close()
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_MidStreamAbort?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "mid-stream-abort")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	s := createExternalMCPServerPinned(t, database, "mid-abort-server", upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPost(t, app, "mid-abort-server", memberKey, `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{}}`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	raw, _ := io.ReadAll(resp.Body) // read until the connection drop; an error here is expected and ignored
	got := string(raw)

	if got != firstChunk {
		t.Errorf("body = %q, want exactly the bytes sent before the abrupt disconnect (%q) — nothing must "+
			"be appended or altered", got, firstChunk)
	}
	if strings.Contains(got, "notifications/cancelled") || strings.Contains(got, `"aborted"`) || strings.Contains(got, "event: error") {
		t.Errorf("body contains a synthetic abort/cancellation event, want none (docs/mcp-v2.md §3.9: the "+
			"caller MUST retry as an entirely new request instead); body: %q", got)
	}
}

// ---- h.MCPStreamIdleTimeout wiring, full stack -----------------------------

// TestMCPProxy_StreamIdleTimeout_FullStack is the full-stack counterpart of
// internal/mcp's TestForward_IdleTimeout_NoDataAtAll_StreamEndsAfterIdleWindow
// and TestForward_SubscriptionsListen_PauseJustUnderIdleTimeout_NotificationStillDelivered:
// it proves Handler.MCPStreamIdleTimeout actually reaches
// mcp.NewHTTPTransport's streamIdleTimeout parameter through
// buildAdHocTransport (mcp_proxy.go), not just that the underlying mechanism
// works in isolation. Handler.MCPStreamIdleTimeout itself had no test
// coverage anywhere in this package before this test.
func TestMCPProxy_StreamIdleTimeout_FullStack(t *testing.T) {
	t.Parallel()

	const idle = 200 * time.Millisecond

	t.Run("upstream goes silent: stream ends after the configured idle window", func(t *testing.T) {
		t.Parallel()

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done() // hang until the proxy's idle timeout cancels us
		}))
		t.Cleanup(upstream.Close)

		dsn := "file:TestMCPProxy_StreamIdleTimeout_FullStack_Silent?mode=memory&cache=private"
		app, database, keyCache := setupMCPProxyAppWithStreamIdleTimeout(t, dsn, idle)
		org := mustCreateTestOrg(t, database, "stream-idle-silent")
		memberKey := addMCPTestKey(t, keyCache, org.ID)

		s := createExternalMCPServerPinned(t, database, "stream-idle-silent-server", upstream.URL, "2026-07-28")
		if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
			t.Fatalf("SetOrgMCPAccess: %v", err)
		}

		resp := proxyPost(t, app, "stream-idle-silent-server", memberKey, `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen"}`)
		defer resp.Body.Close()

		if resp.StatusCode != fiber.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
		}

		start := time.Now()
		raw, _ := io.ReadAll(resp.Body) // must terminate, not hang forever
		elapsed := time.Since(start)

		if len(raw) != 0 {
			t.Errorf("body = %q, want empty — the upstream never sent a byte", raw)
		}
		if elapsed > 3*time.Second {
			t.Errorf("stream took %v to end, want well under 3s for a %v idle window", elapsed, idle)
		}
	})

	t.Run("subscriptions/listen: pause under the idle window still delivers the notification", func(t *testing.T) {
		t.Parallel()

		const pause = 80 * time.Millisecond // comfortably under idle

		const ack = "event: message\n" +
			`data: {"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{}}` + "\n\n"
		const notif = "event: message\n" +
			`data: {"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{}}` + "\n\n"

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			fmt.Fprint(w, ack)
			flusher.Flush()
			time.Sleep(pause)
			fmt.Fprint(w, notif)
			flusher.Flush()
		}))
		t.Cleanup(upstream.Close)

		dsn := "file:TestMCPProxy_StreamIdleTimeout_FullStack_Pause?mode=memory&cache=private"
		app, database, keyCache := setupMCPProxyAppWithStreamIdleTimeout(t, dsn, idle)
		org := mustCreateTestOrg(t, database, "stream-idle-pause")
		memberKey := addMCPTestKey(t, keyCache, org.ID)

		s := createExternalMCPServerPinned(t, database, "stream-idle-pause-server", upstream.URL, "2026-07-28")
		if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
			t.Fatalf("SetOrgMCPAccess: %v", err)
		}

		resp := proxyPost(t, app, "stream-idle-pause-server", memberKey, `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen"}`)
		defer resp.Body.Close()

		if resp.StatusCode != fiber.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
		}

		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v — a pause under the idle window must not truncate the stream", err)
		}

		want := ack + notif
		if string(raw) != want {
			t.Errorf("body = %q, want both events: %q", raw, want)
		}
	})
}
