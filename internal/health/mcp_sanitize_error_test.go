package health_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/voidmind-io/voidllm/internal/health"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file is table-driven regression coverage for sanitizeError's
// upstreamHTTPStatusPattern/jsonRPCErrorCodePattern branch (checker.go): a
// failure that carries a recognizable upstream HTTP status — in any of the
// three message shapes internal/mcp can currently produce it in, reached via
// ListTools — must map to "http <status>" (plus a "(json-rpc <code>)" suffix
// when a JSON-RPC error code is also present), never fall through to the
// generic "probe failed". Before upstreamHTTPStatusPattern existed, every one
// of these cases fell through to "probe failed", discarding the one piece of
// diagnostic information (the HTTP status itself) that carries no upstream
// content and is therefore safe to surface (docs/mcp-v2.md §11.2/§11.5).
func TestMCPHealthChecker_UpstreamHTTPStatus_SanitizesToHTTPStatus_NotProbeFailed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler http.HandlerFunc
		// transport builds the *mcp.HTTPTransport to probe srv.URL with —
		// some cases need era auto-detection, others a specific pin, to
		// reach the exact message shape under test.
		transport func(endpoint string) *mcp.HTTPTransport
		want      string
	}{
		{
			// doCall's default branch (http_transport.go): a modern-era
			// upstream, already resolved, answers tools/list itself with a
			// plain HTTP 401 — "upstream returned HTTP %d", no JSON-RPC
			// code involved at all.
			name: "modern era: tools/list itself answers with HTTP 401",
			handler: func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if rpcMethod(body) == "server/discover" {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"jsonrpc":"2.0","id":"probe-discover","result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{},"ttlMs":0,"cacheScope":"public"}}`)
					return
				}
				w.WriteHeader(http.StatusUnauthorized)
			},
			transport: newPinnedModernTransport,
			want:      "http 401",
		},
		{
			// probeEra's own default branch: server/discover itself answers
			// with an HTTP status probeEra does not recognize as any of the
			// modern success/error shapes — "probe: unexpected server/discover
			// status %d", wrapped by Call as "resolve protocol era: %w".
			name: "auto probe: server/discover answers with an unrecognized HTTP 500",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			transport: newAutoProbeTransport,
			want:      "http 500",
		},
		{
			// probeEra falls through to probeLegacy (server/discover 404),
			// and the legacy initialize probe itself then answers with an
			// unrecognized status — "probe: unexpected initialize status %d".
			name: "auto probe falls to legacy: initialize answers with an unrecognized HTTP 503",
			handler: func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if rpcMethod(body) == "server/discover" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			},
			transport: newAutoProbeTransport,
			want:      "http 503",
		},
		{
			// legacyClientDialect.Warmup: initialize succeeds at the HTTP
			// layer (200) but the body carries a JSON-RPC error object —
			// "upstream returned HTTP %d with JSON-RPC error %d", wrapped by
			// Call as "warmup: %w". Both the HTTP status AND the numeric
			// JSON-RPC code must survive; the upstream's free-form message
			// text must not (covered separately by
			// TestMCPHealthChecker_UpstreamJSONRPCErrorMessage_NotLeakedIntoLastError).
			name: "legacy warmup: initialize returns HTTP 200 with a JSON-RPC error body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				switch rpcMethod(body) {
				case "server/discover":
					w.WriteHeader(http.StatusNotFound)
				case "initialize":
					fmt.Fprint(w, `{"jsonrpc":"2.0","id":"warmup-initialize","error":{"code":-32001,"message":"server not ready"}}`)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			},
			transport: newAutoProbeTransport,
			want:      "http 200 (json-rpc -32001)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)

			id := "sanitize-" + sanitizeHealthTestName(tc.name)
			transports := map[string]*mcp.HTTPTransport{id: tc.transport(srv.URL)}
			c := newMCPChecker(func() []health.MCPServerTarget {
				return []health.MCPServerTarget{newMCPTarget(id, "Sanitize Test Server", id)}
			}, staticTransportFor(transports))
			stop := c.Start()
			t.Cleanup(stop)

			got := c.GetHealth(id)
			if got.Status != "unhealthy" {
				t.Fatalf("Status = %q, want %q (LastError=%q)", got.Status, "unhealthy", got.LastError)
			}
			if got.LastError != tc.want {
				t.Errorf("LastError = %q, want %q", got.LastError, tc.want)
			}
			if got.LastError == "probe failed" {
				t.Errorf("LastError fell through to the generic fallback, want the sanitized HTTP status %q to have been recognized", tc.want)
			}
		})
	}
}

// sanitizeHealthTestName replaces characters that are awkward in a server ID
// (spaces, colons) with hyphens, mirroring the sanitizeTestName helper
// internal/api/admin's test suite uses for the same purpose — duplicated
// here rather than imported, since internal/health has no existing
// dependency on internal/api/admin's test helpers.
func sanitizeHealthTestName(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}
