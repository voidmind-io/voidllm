package admin_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// This file covers mcp.HTTPTransport.Forward's unsolicited-encoding rejection
// (http_transport.go, docs/mcp-v2.md review finding B) at the full HTTP-proxy
// level: Go's own net/http.Transport transparently negotiates and decodes
// gzip, so that case must keep working exactly as before; any OTHER
// Content-Encoding this build never asked for (br, zstd, ...) must be
// rejected outright rather than streamed through as a potential
// decompression bomb underneath settings.mcp.stream_max_bytes' wire-byte
// ceiling. See TestMCPProxy_ResponseHeaderAllowlist's "Content-Encoding is
// rejected..." case for the header-allowlist-level assertion this file's
// tests complement with full response-body/status coverage.

// gzipCompress gzip-compresses body for a test upstream to serve as its raw
// response bytes.
func gzipCompress(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(body); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// TestMCPProxy_Compression_Gzip_TransparentlyDecoded is the most important
// case in this file: it must NOT break. Go's net/http.Transport adds its own
// "Accept-Encoding: gzip" whenever the outbound request sets none (neither
// rawPost nor Forward do) and transparently decompresses a gzip-encoded
// response, stripping Content-Encoding/Content-Length before Forward's own
// unsolicited-encoding check ever runs — so a real upstream that compresses
// its responses by default (a common, sensible default) must keep working
// exactly as it did before review finding B's fix.
func TestMCPProxy_Compression_Gzip_TransparentlyDecoded(t *testing.T) {
	t.Parallel()

	const plainBody = `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search"}]}}`
	compressed := gzipCompress(t, []byte(plainBody))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(compressed)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_Compression_Gzip?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "compression-gzip")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "compression-gzip-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (a gzip-compressing upstream must keep working); body: %s", resp.StatusCode, raw)
	}

	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want absent (Go's own transport already decoded the body and "+
			"stripped this header before VoidLLM ever saw it)", got)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(raw) != plainBody {
		t.Errorf("body = %q, want the DECOMPRESSED upstream body: %q", raw, plainBody)
	}
}

// TestMCPProxy_Compression_UnsolicitedEncoding_Rejected_NoHalfOpenStream is
// table-driven coverage for the two encodings Go's transport never
// auto-negotiates on its own — br and zstd — both of which must be rejected
// as an unsolicited Content-Encoding: the call fails outright with an
// ordinary, complete 502 response (never a half-open stream, since Forward
// never returns a *ForwardResult on this path in the first place — the
// rejection happens before HandleMCPProxy could ever start streaming), and
// the upstream's response body is closed rather than leaked. See
// TestMCPProxy_UpstreamUnreachable_ReturnsOrdinaryErrorResponse_NoHalfOpenStream
// (mcp_proxy_streaming_test.go) for the sibling "ordinary error response, not
// a half-open stream" property this shares.
func TestMCPProxy_Compression_UnsolicitedEncoding_Rejected_NoHalfOpenStream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		encoding string
	}{
		{name: "br (Brotli), never auto-negotiated by Go's transport", encoding: "br"},
		{name: "zstd, never auto-negotiated by Go's transport", encoding: "zstd"},
		{name: "deflate, never auto-negotiated by Go's transport", encoding: "deflate"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// The body content is irrelevant here — Forward rejects based
				// on the Content-Encoding header's mere PRESENCE, never
				// attempting to actually decode the body. Some arbitrary
				// non-empty bytes stand in for a genuinely encoded body.
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Encoding", tc.encoding)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("not-actually-decodable-opaque-bytes"))
			}))
			t.Cleanup(upstream.Close)

			alias := "compression-rejected-" + sanitizeTestName(tc.name)
			dsn := "file:TestMCPProxy_Compression_UnsolicitedEncoding_" + sanitizeTestName(tc.name) + "?mode=memory&cache=private"
			app, database, keyCache := setupMCPProxyApp(t, dsn)
			org := mustCreateTestOrg(t, database, alias)
			memberKey := addMCPTestKey(t, keyCache, org.ID)

			s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
			if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
				t.Fatalf("SetOrgMCPAccess: %v", err)
			}

			resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusBadGateway {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 502 (an unsolicited %q Content-Encoding must be rejected); body: %s",
					resp.StatusCode, tc.encoding, raw)
			}

			if got := resp.Header.Get("Content-Encoding"); got != "" {
				t.Errorf("Content-Encoding = %q, want absent — the rejected encoding must never reach the caller", got)
			}

			// A COMPLETE, ordinary error response, not a truncated or
			// half-open one: the whole point of rejecting BEFORE Forward
			// returns a *ForwardResult is that HandleMCPProxy never even
			// starts SendStreamWriter on this path.
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v — a rejected-encoding failure must produce a COMPLETE response body, "+
					"not a truncated or half-open one", err)
			}
			if len(raw) == 0 {
				t.Error("response body is empty, want a JSON-RPC error body")
			}
		})
	}
}
