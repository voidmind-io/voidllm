package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
)

// reconstructSSEBody parses a single SSE event block produced by
// formatSSEMessage and reconstructs the original body a line-by-line SSE
// parser would see: every "data: " line has its prefix stripped, and the
// results are joined with "\n" — exactly reversing formatSSEMessage's own
// per-line "data: " prefixing. It also verifies the block's own shape is a
// valid SSE event: it must start with "event: message\n" and every
// non-blank content line must be a "data: " line (a bare, un-prefixed
// non-empty line would mean the message got truncated at that line by an SSE
// parser, which is exactly the bug per-line "data:" prefixing exists to
// prevent — see formatSSEMessage's doc in mcp_handler.go).
func reconstructSSEBody(t *testing.T, raw string) string {
	t.Helper()

	const eventPrefix = "event: message\n"
	if !strings.HasPrefix(raw, eventPrefix) {
		t.Fatalf("SSE block does not start with %q; got: %q", eventPrefix, raw)
	}
	rest := strings.TrimPrefix(raw, eventPrefix)

	var dataLines []string
	for _, line := range strings.Split(rest, "\n") {
		if line == "" {
			// The blank line terminating the event. Anything after it (there
			// should be nothing, for a single-event block) is not this
			// event's concern.
			break
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("line inside the SSE event is neither empty nor \"data: \"-prefixed — a "+
				"line-by-line SSE parser would treat this as a bare line and lose the rest of the "+
				"message; line = %q, full block: %q", line, raw)
		}
		dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
	}

	return strings.Join(dataLines, "\n")
}

// TestFormatSSEMessage_OneDataLinePerBodyLine verifies formatSSEMessage
// (used by the built-in server's handleMCPRequest — see FormatSSEMessage's
// doc) wraps every line of body with its own "data:" prefix, so a body
// containing raw newlines — pretty-printed JSON, in particular — survives
// being carried over SSE, where only content belonging to a "data:"-prefixed
// line is delivered to an SSE client at all.
//
// This is exercised directly against the shared function rather than through
// the built-in HTTP endpoint's real response body: the built-in server's own
// responses are always produced by jsonx.Marshal, which is always compact
// (single-line) by construction — see TestMCPHandler_SSE_PostWithAcceptSSE
// for that endpoint's real, single-line regression coverage — so a literally
// multi-line body can only be exercised synthetically here, directly against
// the function. formatSSEMessage is no longer used by HandleMCPProxy
// (mcp_proxy.go): the streaming rewrite made the proxy path a byte-identical
// pass-through that never reformats an upstream's response, regardless of
// the caller's Accept header — see
// TestMCPProxy_PassesThroughByteIdentical_MultilineUpstreamBody_EvenWithSSEAccept
// below for that (now deliberately opposite) contract.
func TestFormatSSEMessage_OneDataLinePerBodyLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{
			name: "single-line body is unchanged (regression)",
			body: `{"jsonrpc":"2.0","id":1,"result":{"status":"ok"}}`,
		},
		{
			name: "pretty-printed (multi-line) JSON body gets one data: line per body line",
			body: prettyJSON(t, map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  map[string]any{"status": "ok", "nested": map[string]any{"a": 1, "b": 2}},
			}),
		},
		{
			name: "body with a blank line in the middle",
			body: "line one\n\nline three",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out := admin.FormatSSEMessage([]byte(tc.body))

			if !strings.HasSuffix(out, "\n\n") {
				t.Errorf("SSE block does not end with a blank line terminating the event; got: %q", out)
			}

			got := reconstructSSEBody(t, out)
			if got != tc.body {
				t.Errorf("reconstructed body =\n%q\nwant byte-identical to the original body:\n%q", got, tc.body)
			}

			// Every line of the original body must have produced its OWN
			// "data: " line — i.e. exactly len(strings.Split(body, "\n"))
			// occurrences of the "data: " prefix — never fewer, which would
			// mean lines got merged or dropped.
			wantDataLines := len(strings.Split(tc.body, "\n"))
			gotDataLines := strings.Count(out, "\ndata: ") // every data line is preceded by a newline
			if gotDataLines != wantDataLines {
				t.Errorf("data: line count = %d, want %d (one per body line)", gotDataLines, wantDataLines)
			}
		})
	}
}

// prettyJSON marshals v with indentation (json.MarshalIndent), guaranteeing
// a genuinely multi-line JSON document for tests that need one.
func prettyJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	return string(b)
}

// TestMCPProxy_PassesThroughByteIdentical_MultilineUpstreamBody_EvenWithSSEAccept
// used to be TestMCPProxy_SSE_MultilineUpstreamBody_WrappedPerLine and asserted
// the OPPOSITE of what it asserts now: that HandleMCPProxy re-wrapped the
// upstream's body as an SSE "data:"-per-line message when the caller sent
// Accept: text/event-stream. That behavior no longer exists — the streaming
// rewrite (docs/mcp-v2.md, "Streaming-Umbau") made HandleMCPProxy a genuine
// transparent intermediary: it never interprets or reformats the upstream's
// body, regardless of what the caller's Accept header says (see
// mcp.HTTPTransport.Forward's doc and mcp_proxy.go's comment above
// copyMCPResponseHeaders). The upstream alone decides Content-Type and
// framing; a pretty-printed, genuinely multi-line JSON-RPC body must reach
// the caller byte-for-byte, not one "data:" line per body line.
//
// formatSSEMessage itself (mcp_handler.go) is unchanged and still used by the
// BUILT-IN server's own responses — see TestFormatSSEMessage_OneDataLinePerBodyLine
// for that unit-level coverage, and TestMCPHandler_SSE_PostWithAcceptSSE for
// its own end-to-end HTTP coverage. This test now covers the proxy path's
// deliberately different contract: no wrapping, ever.
func TestMCPProxy_PassesThroughByteIdentical_MultilineUpstreamBody_EvenWithSSEAccept(t *testing.T) {
	t.Parallel()

	prettyBody := prettyJSON(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result": map[string]any{
			"tools": []any{
				map[string]any{"name": "search", "description": "Search the web"},
			},
		},
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, prettyBody)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_PassesThroughByteIdentical_MultilineUpstreamBody?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "proxy-sse-multiline")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	s := createExternalMCPServer(t, database, "sse-multiline-server", upstream.URL)
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/sse-multiline-server",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	// A conformant modern client MUST send both media types (docs/mcp-v2.md
	// §4.1) — this is deliberately not "text/event-stream" alone, to prove
	// the upstream's own choice of application/json is what survives, not a
	// proxy-side Accept-based branch (see TestMCPProxy_ContentType_BelongsToUpstream_*
	// for the dedicated coverage of exactly that property).
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+memberKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	// The upstream's own Content-Type, not an SSE one the proxy invents.
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json prefix (the upstream's own, not proxy-invented SSE)", ct)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if string(raw) != prettyBody {
		t.Errorf("body =\n%q\nwant byte-identical to the upstream's pretty-printed body, with NO SSE "+
			"\"event:\"/\"data:\" wrapping added:\n%q", raw, prettyBody)
	}

	// Sanity check that the fixture is genuinely multi-line — otherwise this
	// test would pass vacuously even if byte-identical pass-through were broken.
	if !strings.Contains(prettyBody, "\n") {
		t.Fatal("test fixture prettyBody has no newline at all — the test setup itself is broken")
	}
}

// TestMCPProxy_SSEOnlyAccept_WrapsJSONUpstreamResponse used to be named
// TestMCPProxy_PassesThroughByteIdentical_SingleLineUpstreamBody_EvenWithSSEAccept
// (before that, TestMCPProxy_SSE_SingleLineUpstreamBody_Unchanged) and
// asserted the OPPOSITE of what it asserts now: that a caller sending only
// "Accept: text/event-stream" got the upstream's application/json body back
// completely unwrapped, byte-identical.
//
// That was the exact legacy regression acceptsOnlySSE (mcp_handler.go) exists
// to close: before the streaming rewrite, HandleMCPProxy had no Accept-based
// branch of its own at all, so a caller who never adopted the modern
// requirement to accept BOTH media types (docs/mcp-v2.md §1a, §4.1) — the
// definition of a legacy client here — would be handed raw application/json
// it cannot parse as the SSE stream it explicitly, exclusively asked for.
// HandleMCPProxy now special-cases exactly that caller (see mcp_proxy.go's
// comment above the acceptsOnlySSE branch) and wraps the upstream's
// application/json body as a single SSE "message" event via
// sendLegacySSEWrapped/formatSSEMessage, exactly as the built-in server's
// handleMCPRequest already does. See
// TestMCPProxy_PassesThroughByteIdentical_SingleLineUpstreamBody_WithBothMediaTypes
// below for the Gegenprobe: a caller sending Accept: "application/json,
// text/event-stream" (what every modern client MUST send) still gets the
// upstream's own choice through byte-identical, exactly as before — only the
// legacy-only-SSE caller is special-cased.
func TestMCPProxy_SSEOnlyAccept_WrapsJSONUpstreamResponse(t *testing.T) {
	t.Parallel()

	const compactBody = `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, compactBody)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_SSEOnlyAccept_WrapsJSONUpstreamResponse?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "proxy-sse-singleline")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	s := createExternalMCPServer(t, database, "sse-singleline-server", upstream.URL)
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/sse-singleline-server",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	// Deliberately ONLY text/event-stream — never also application/json. This
	// is the one Accept shape acceptsOnlySSE treats as a legacy caller; see
	// this test's own doc.
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+memberKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream (wrapped for a caller that cannot parse "+
			"application/json)", ct)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	want := admin.FormatSSEMessage([]byte(compactBody))
	if string(raw) != want {
		t.Errorf("body = %q, want the upstream's JSON body wrapped as a single SSE \"message\" event: %q", raw, want)
	}
	if got := reconstructSSEBody(t, string(raw)); got != compactBody {
		t.Errorf("reconstructed SSE body = %q, want the original compact JSON: %q", got, compactBody)
	}
}

// TestMCPProxy_PassesThroughByteIdentical_SingleLineUpstreamBody_WithBothMediaTypes
// is the Gegenprobe for TestMCPProxy_SSEOnlyAccept_WrapsJSONUpstreamResponse
// above: a caller whose Accept header names BOTH application/json and
// text/event-stream — what every modern MCP client MUST send
// (docs/mcp-v2.md §4.1) — is never treated as a legacy, SSE-only caller by
// acceptsOnlySSE, so the upstream's compact application/json body reaches it
// completely untouched, exactly as
// TestMCPProxy_PassesThroughByteIdentical_MultilineUpstreamBody_EvenWithSSEAccept
// already proves for a genuinely multi-line body. This is the single-line
// half of that same matrix.
func TestMCPProxy_PassesThroughByteIdentical_SingleLineUpstreamBody_WithBothMediaTypes(t *testing.T) {
	t.Parallel()

	const compactBody = `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, compactBody)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_PassesThroughByteIdentical_SingleLineUpstreamBody_BothTypes?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "proxy-sse-singleline-both")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	s := createExternalMCPServer(t, database, "sse-singleline-both-server", upstream.URL)
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/sse-singleline-both-server",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	// A conformant modern client MUST send both media types (docs/mcp-v2.md
	// §4.1) — this is the shape that keeps a caller OUT of the legacy,
	// SSE-only special case.
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+memberKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json prefix (the upstream's own, not proxy-invented SSE)", ct)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if string(raw) != compactBody {
		t.Errorf("body = %q, want byte-identical to the upstream's compact body, with no SSE framing added: %q", raw, compactBody)
	}
}

// TestMCPProxy_GenuineUpstreamSSE_NeverRewrapped_RegardlessOfClientAccept
// verifies the third leg of acceptsOnlySSE's contract, spelled out in
// mcp_proxy.go's comment above it: a genuine upstream SSE response
// (Content-Type: text/event-stream) is never touched by the legacy-wrap
// branch, because it is already the exact format a legacy, SSE-only caller
// expects — unlike TestMCPProxy_SSEOnlyAccept_WrapsJSONUpstreamResponse
// above, there is nothing here for sendLegacySSEWrapped to do. This holds
// regardless of what the caller's own Accept header says: an SSE-only
// caller, a modern caller sending both media types, and a caller sending no
// Accept header at all must all receive the upstream's own SSE bytes
// completely unchanged.
func TestMCPProxy_GenuineUpstreamSSE_NeverRewrapped_RegardlessOfClientAccept(t *testing.T) {
	t.Parallel()

	const sseBody = "event: message\n" +
		`data: {"jsonrpc":"2.0","id":1,"result":{"tools":[]}}` +
		"\n\n"

	tests := []struct {
		name   string
		accept string
	}{
		{name: "SSE-only Accept (the legacy shape)", accept: "text/event-stream"},
		{name: "both media types (the modern, spec-compliant shape)", accept: "application/json, text/event-stream"},
		{name: "no Accept header at all", accept: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, sseBody)
				w.(http.Flusher).Flush()
			}))
			t.Cleanup(upstream.Close)

			alias := "genuine-sse-" + sanitizeTestName(tc.name)
			dsn := "file:TestMCPProxy_GenuineUpstreamSSE_" + sanitizeTestName(tc.name) + "?mode=memory&cache=private"
			app, database, keyCache := setupMCPProxyApp(t, dsn)
			org := mustCreateTestOrg(t, database, alias)
			memberKey := addMCPTestKey(t, keyCache, org.ID)

			// Pinned, not probed (createExternalMCPServerPinned's own doc):
			// this fixture answers every request — including whatever
			// server/discover or initialize probe an unpinned transport would
			// send first — with the identical canned SSE body below, which
			// carries a fixed id ("1") that will not match a probe's own id.
			// Before rawPost gained real request-id matching for SSE
			// responses (Fund 1), that mismatch was invisible to era probing;
			// now it would surface as a probe failure this test has nothing
			// to do with. Pinning sidesteps probing entirely, exactly the
			// noise this helper exists to avoid, and Forward — the only path
			// this test exercises — never depends on which era was pinned.
			s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
			if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
				t.Fatalf("SetOrgMCPAccess: %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/"+alias,
				strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
			req.Header.Set("Content-Type", "application/json")
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			req.Header.Set("Authorization", "Bearer "+memberKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusOK {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
			}
			ct := resp.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "text/event-stream") {
				t.Errorf("Content-Type = %q, want text/event-stream (the upstream's own)", ct)
			}

			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if string(raw) != sseBody {
				t.Errorf("body = %q, want byte-identical to the upstream's own SSE bytes, never re-wrapped: %q", raw, sseBody)
			}
		})
	}
}
