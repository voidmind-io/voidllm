package admin_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// This file covers settings.mcp.stream_max_bytes (Handler.MCPStreamMaxBytes)
// as enforced by mcp_proxy.go's flushingWriter, at the full HTTP-proxy
// level. See internal/config's TestMCPConfig_EffectiveStreamMaxBytes for the
// literal 100 MiB default itself, checked purely through config — never by
// actually sending 100 MiB in a test.

// chunkedMCPServer starts an httptest.Server that writes body in
// chunkSize-byte pieces, flushing and pausing briefly after each one, as
// "text/event-stream" — so a caller-configured byte ceiling can be observed
// truncating mid-stream rather than rejecting one single oversized write
// wholesale. The pause is deliberate: without it, localhost delivers chunks
// fast enough that io.Copy's own Read calls coalesce several flushed writes
// into one larger read, making the byte boundary this test asserts on
// unpredictable — spacing them out forces each flush to arrive as its own,
// separately observed Read/Write pair in HandleMCPProxy's copy loop.
func chunkedMCPServer(t *testing.T, body []byte, chunkSize int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < len(body); i += chunkSize {
			end := i + chunkSize
			if end > len(body) {
				end = len(body)
			}
			_, _ = w.Write(body[i:end])
			flusher.Flush()
			time.Sleep(15 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestMCPProxy_StreamMaxBytes_Exceeded_TruncatesWithoutSyntheticEvent
// verifies that once the configured ceiling is reached, the stream simply
// ends: the client receives exactly the bytes delivered up to (and not past)
// the boundary — a genuine prefix of what the upstream sent, not zero bytes
// and not the full body — and nothing synthetic (no error/abort event) is
// appended, exactly like the unrelated mid-stream-abort and
// idle-timeout cases (docs/mcp-v2.md §3.9).
func TestMCPProxy_StreamMaxBytes_Exceeded_TruncatesWithoutSyntheticEvent(t *testing.T) {
	t.Parallel()

	// 10 chunks of 10 bytes each, upstream total 100 bytes.
	const chunkSize = 10
	const chunkCount = 10
	full := strings.Repeat("0123456789", chunkCount)

	const maxBytes = 50 // exactly 5 chunks' worth

	upstream := chunkedMCPServer(t, []byte(full), chunkSize)

	dsn := "file:TestMCPProxy_StreamMaxBytes_Exceeded?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyAppWithMaxBytes(t, dsn, maxBytes)
	org := mustCreateTestOrg(t, database, "bytelimit-exceeded")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "bytelimit-exceeded-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	got, _ := io.ReadAll(resp.Body) // an error/truncated read here is expected; the byte content is what matters

	if len(got) != maxBytes {
		t.Fatalf("client received %d bytes, want exactly %d (the configured ceiling, not zero and not the full "+
			"%d-byte upstream body)", len(got), maxBytes, len(full))
	}
	if string(got) != full[:maxBytes] {
		t.Errorf("received bytes = %q, want the genuine prefix of the upstream's body: %q", got, full[:maxBytes])
	}
}

// TestMCPProxy_StreamMaxBytes_Zero_MeansUnbounded verifies that an explicit
// MCPStreamMaxBytes of 0 disables the ceiling entirely: a stream well beyond
// a small reference limit (the same 50-byte ceiling
// TestMCPProxy_StreamMaxBytes_Exceeded_TruncatesWithoutSyntheticEvent above
// proves DOES truncate at) runs through completely unbounded when the
// ceiling is explicitly 0.
func TestMCPProxy_StreamMaxBytes_Zero_MeansUnbounded(t *testing.T) {
	t.Parallel()

	const chunkSize = 10
	const chunkCount = 20 // 200 bytes total — 4x the reference ceiling above
	full := strings.Repeat("0123456789", chunkCount)

	upstream := chunkedMCPServer(t, []byte(full), chunkSize)

	dsn := "file:TestMCPProxy_StreamMaxBytes_Zero?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyAppWithMaxBytes(t, dsn, 0)
	org := mustCreateTestOrg(t, database, "bytelimit-zero")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "bytelimit-zero-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v — MCPStreamMaxBytes=0 must never truncate", err)
	}
	if string(got) != full {
		t.Errorf("received %d bytes, want the full %d-byte upstream body (MCPStreamMaxBytes=0 means unbounded)",
			len(got), len(full))
	}
}
