package admin_test

// Tests for collectMCPParamHeaders and forwardHeaders (mcp_proxy.go) — the
// Mcp-Param-{Name} tool-parameter header family MCP 2026-07-28 §4.3
// obligates an intermediary to forward, uninterpreted, whenever it does not
// itself recognize a given header name. Every test here drives the real
// HandleMCPProxy route end to end against a fake upstream that records every
// header it receives.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/valyala/fasthttp"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/pkg/crypto"
)

// ---- 1. Passthrough: exact values, unmodified ------------------------------

// TestCollectMCPParamHeaders_PassedThroughUnmodified proves the core §4.3
// obligation: Mcp-Param-Region and Mcp-Param-Tenant, sent by a caller VoidLLM
// itself does not interpret, reach the upstream with EXACTLY these values,
// byte for byte. Rot the moment the allowlist shrinks back to only the four
// standard headers (mcp.HeaderProtocolVersion/Method/Name/SessionID) — the
// upstream would then see neither header at all — or the value is re-encoded
// somewhere on the way (e.g. accidentally run through EncodeHeaderValue,
// which would leave plain ASCII like "us-west1" untouched but would be wrong
// in principle: §4.3 says forward, not re-derive).
func TestCollectMCPParamHeaders_PassedThroughUnmodified(t *testing.T) {
	t.Parallel()

	var gotRegion, gotTenant string
	var sawRegion, sawTenant bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRegion, sawRegion = r.Header.Get("Mcp-Param-Region"), r.Header.Get("Mcp-Param-Region") != ""
		gotTenant, sawTenant = r.Header.Get("Mcp-Param-Tenant"), r.Header.Get("Mcp-Param-Tenant") != ""
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestCollectMCPParamHeaders_PassedThroughUnmodified?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "param-hdr-passthrough")
	key := addMCPTestKey(t, keyCache, org.ID)

	s := createExternalMCPServer(t, database, "param-passthrough-server", upstream.URL)
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPostWithHeaders(t, app, "param-passthrough-server", key,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		map[string]string{
			"Mcp-Param-Region": "us-west1",
			"Mcp-Param-Tenant": "acme",
		})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	if !sawRegion || gotRegion != "us-west1" {
		t.Errorf("upstream Mcp-Param-Region = %q (present=%v), want %q", gotRegion, sawRegion, "us-west1")
	}
	if !sawTenant || gotTenant != "acme" {
		t.Errorf("upstream Mcp-Param-Tenant = %q (present=%v), want %q", gotTenant, sawTenant, "acme")
	}
}

// ---- 2. Invalid names/values never reach upstream, request still succeeds --

// TestCollectMCPParamHeaders_InvalidRejectedButRequestSucceeds verifies
// collectMCPParamHeaders' rule 2 (and the surviving, non-length half of rule
// 3): an invalid Mcp-Param-{Name} header NAME, or a value that is empty or
// not visible ASCII, is silently dropped (never reaches the upstream), AND
// the request is answered normally (HTTP 200), not with an error status. A
// value over MaxParamHeaderValueLength is deliberately NOT covered here any
// more — see TestCollectMCPParamHeaders_ValueTooLong_Rejected, which used to
// be a case in this very table asserting the opposite (HTTP 200, silent
// drop) until docs/mcp-v2.md review round Fund 3 made that the bug: a value
// this proxy cannot forward in full must fail the whole request closed, the
// same way too many headers already did, rather than silently omit it and
// let the header set disagree with the JSON-RPC body it travels alongside.
// A valid header sent alongside the invalid one in the same request proves
// the rejection is scoped to the one bad header, not the whole family.
func TestCollectMCPParamHeaders_InvalidRejectedButRequestSucceeds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		invalidName    string
		invalidValue   string
		wantAbsentName string // header name that must be ABSENT upstream
	}{
		{
			name:           "empty suffix after the Mcp-Param- prefix is not a valid name",
			invalidName:    "Mcp-Param-",
			invalidValue:   "should-never-arrive",
			wantAbsentName: "Mcp-Param-",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			var sawInvalid bool
			var gotValid string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				if r.Header.Get(tc.invalidName) != "" {
					sawInvalid = true
				}
				gotValid = r.Header.Get("Mcp-Param-Valid")
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			}))
			t.Cleanup(upstream.Close)

			dsn := "file:TestCollectMCPParamHeaders_Invalid_" + sanitizeTestName(tc.name) + "?mode=memory&cache=private"
			app, database, keyCache := setupMCPProxyApp(t, dsn)
			org := mustCreateTestOrg(t, database, "param-hdr-invalid-"+sanitizeTestName(tc.name))
			key := addMCPTestKey(t, keyCache, org.ID)

			alias := "param-invalid-" + sanitizeTestName(tc.name)
			s := createExternalMCPServer(t, database, alias, upstream.URL)
			if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
				t.Fatalf("SetOrgMCPAccess: %v", err)
			}

			resp := proxyPostWithHeaders(t, app, alias, key,
				`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
				map[string]string{
					tc.invalidName:    tc.invalidValue,
					"Mcp-Param-Valid": "still-works",
				})
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusOK {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 200 (the request itself must still be answered normally); body: %s", resp.StatusCode, raw)
			}

			mu.Lock()
			defer mu.Unlock()
			if sawInvalid {
				t.Errorf("upstream saw the invalid header %q, want it silently dropped", tc.wantAbsentName)
			}
			if gotValid != "still-works" {
				t.Errorf("upstream Mcp-Param-Valid = %q, want %q (a valid header sent alongside the invalid one must still be forwarded)", gotValid, "still-works")
			}
		})
	}
}

// ---- 2b. A too-long value rejects the whole request, like too many headers -

// TestCollectMCPParamHeaders_ValueTooLong_Rejected REPLACES the
// "a value over MaxParamHeaderValueLength is rejected" case that used to
// live in TestCollectMCPParamHeaders_InvalidRejectedButRequestSucceeds'
// table, where it asserted HTTP 200 and a silent drop. docs/mcp-v2.md review
// round Fund 3 identified that as the bug: a value too long to mirror in
// full left the outbound Mcp-Param-* header set silently missing information
// the JSON-RPC body's arguments still carried, while the sibling "too many
// headers" case already failed the whole request closed for the identical
// underlying problem (unable to forward in full). This test asserts the
// corrected, fail-closed behavior instead, mirroring
// TestCollectMCPParamHeaders_TooMany_Rejected's shape: HTTP 400, a JSON-RPC
// mcp.CodeParamHeaderValueTooLong error, the upstream never contacted at
// all, and an error message that names only the configured limit — never
// the header name or any fragment of the oversized value.
func TestCollectMCPParamHeaders_ValueTooLong_Rejected(t *testing.T) {
	t.Parallel()

	const sentinelValue = "should-never-leak-into-the-error-message"
	overLong := sentinelValue + strings.Repeat("a", mcp.MaxParamHeaderValueLength)

	var mu sync.Mutex
	var upstreamCalled bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		upstreamCalled = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestCollectMCPParamHeaders_ValueTooLong_Rejected?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "param-hdr-toolong")
	key := addMCPTestKey(t, keyCache, org.ID)

	const alias = "param-toolong-server"
	s := createExternalMCPServer(t, database, alias, upstream.URL)
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPostWithHeaders(t, app, alias, key,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		map[string]string{"Mcp-Param-Toolong": overLong})
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, raw)
	}

	mu.Lock()
	called := upstreamCalled
	mu.Unlock()
	if called {
		t.Error("upstream was called, want the request rejected at VoidLLM's own edge before any upstream call")
	}

	mcpResp := decodeMCPErrorBody(t, io.NopCloser(bytes.NewReader(raw)))
	if mcpResp.Error == nil {
		t.Fatal("expected JSON-RPC error, got nil")
	}
	if mcpResp.Error.Code != mcp.CodeParamHeaderValueTooLong {
		t.Errorf("Error.Code = %d, want %d (CodeParamHeaderValueTooLong)", mcpResp.Error.Code, mcp.CodeParamHeaderValueTooLong)
	}
	if strings.Contains(mcpResp.Error.Message, "Toolong") || strings.Contains(mcpResp.Error.Message, sentinelValue) {
		t.Errorf("Error.Message %q leaks the header name or value, want only the configured limit", mcpResp.Error.Message)
	}
}

// ---- 3. Exceeding the cap rejects the whole request, at the exact boundary -

// mcpParamHeaderKV is one header this test sends, in an explicit order it
// controls directly on the wire (see sendMCPProxyOrderedHeaders).
type mcpParamHeaderKV struct {
	name  string
	value string
}

// sendMCPProxyOrderedHeaders drives HandleMCPProxy with the given extra
// headers set in EXACTLY the given order, bypassing net/http's own request
// writer entirely: net/http always transmits headers sorted by canonical key
// (net/http/header.go's Header.WriteSubset calls sortedKeyValues), so
// req.Header.Set combined with httptest.NewRequest/app.Test can never
// actually put two different requests on the wire in two different header
// orders — every such request already arrives at collectMCPParamHeaders in
// the same (alphabetical) order regardless of insertion order. To exercise
// send-order independence for real, this builds a *fasthttp.Request directly
// (valyala/fasthttp's RequestHeader.Set appends to an internal slice in
// INSERTION order and its own iteration in .All() preserves that order — see
// header.go's setArg/appendArg), then drives fiber's own fasthttp.RequestHandler
// (app.Handler()) with a *fasthttp.RequestCtx built via the library's own
// Init, exactly the way fasthttp's own test suite constructs a synthetic
// context.
func sendMCPProxyOrderedHeaders(t *testing.T, app *fiber.App, alias, key, body string, headers []mcpParamHeaderKV) (status int, respBody []byte) {
	t.Helper()

	var req fasthttp.Request
	req.Header.SetMethod(http.MethodPost)
	req.SetRequestURI("/api/v1/mcp/" + alias)
	req.Header.SetContentType("application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.SetBodyString(body)
	for _, h := range headers {
		req.Header.Set(h.name, h.value)
	}

	var ctx fasthttp.RequestCtx
	ctx.Init(&req, nil, nil)

	app.Handler()(&ctx)

	return ctx.Response.StatusCode(), append([]byte(nil), ctx.Response.Body()...)
}

// TestCollectMCPParamHeaders_TooMany_Rejected REPLACES the former
// TestCollectMCPParamHeaders_CapAt16_DeterministicAcrossOrder, which
// asserted that of 20 validly-named, validly-valued Mcp-Param-* headers,
// exactly the 16 lexicographically-first by lowercased name reached the
// upstream — i.e. that collectMCPParamHeaders silently forwarded a
// deterministically-chosen SUBSET rather than the whole set. That was the
// worse of two designs (see collectMCPParamHeaders' own doc): forwarding
// fewer headers than the caller sent, while still executing the request,
// lets the header set silently diverge from what the JSON-RPC body's
// argument set implies, and any resulting HeaderMismatch the upstream
// raises under §4.5 is then undiagnosable from the caller's side of this
// proxy — nothing told them 4 of their 20 headers were dropped.
//
// This test asserts the corrected behavior instead: 17 validly-named,
// validly-valued Mcp-Param-* headers (K00..K16) — one more than
// mcp.MaxParamHeaders (16) — reject the WHOLE request with HTTP 400 and
// JSON-RPC mcp.CodeTooManyParamHeaders, and the upstream is never contacted
// at all. This is checked across the same two disagreeing wire orders the
// former test used to prove the OLD subset-selection was deterministic —
// kept here because the invariant under test is now "always rejected", and
// disagreeing send orders is exactly what would reveal a regression back
// toward order-dependent, silently-partial forwarding.
func TestCollectMCPParamHeaders_TooMany_Rejected(t *testing.T) {
	t.Parallel()

	const n = 17 // one more than mcp.MaxParamHeaders (16)
	var kvs []mcpParamHeaderKV
	for i := 0; i < n; i++ {
		kvs = append(kvs, mcpParamHeaderKV{
			name:  fmt.Sprintf("Mcp-Param-K%02d", i),
			value: fmt.Sprintf("v%02d", i),
		})
	}

	// Two distinct send orders: ascending (as built above) and a reversed
	// order, so the two runs disagree about which header fasthttp's own
	// iterator would visit first.
	ascending := append([]mcpParamHeaderKV(nil), kvs...)
	descending := make([]mcpParamHeaderKV, len(kvs))
	for i, kv := range kvs {
		descending[len(kvs)-1-i] = kv
	}

	runs := []struct {
		name  string
		order []mcpParamHeaderKV
	}{
		{name: "ascending send order", order: ascending},
		{name: "descending send order", order: descending},
	}

	for _, run := range runs {
		t.Run(run.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			var upstreamCalled bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				upstreamCalled = true
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			}))
			t.Cleanup(upstream.Close)

			dsn := "file:TestCollectMCPParamHeaders_TooMany_" + sanitizeTestName(run.name) + "?mode=memory&cache=private"
			app, database, keyCache := setupMCPProxyApp(t, dsn)
			org := mustCreateTestOrg(t, database, "param-hdr-toomany-"+sanitizeTestName(run.name))
			key := addMCPTestKey(t, keyCache, org.ID)

			alias := "param-toomany-" + sanitizeTestName(run.name)
			s := createExternalMCPServer(t, database, alias, upstream.URL)
			if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
				t.Fatalf("SetOrgMCPAccess: %v", err)
			}

			status, body := sendMCPProxyOrderedHeaders(t, app, alias, key,
				`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, run.order)
			if status != fiber.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", status, body)
			}

			mu.Lock()
			called := upstreamCalled
			mu.Unlock()
			if called {
				t.Error("upstream was called, want the request rejected at VoidLLM's own edge before any upstream call")
			}

			mcpResp := decodeMCPErrorBody(t, io.NopCloser(bytes.NewReader(body)))
			if mcpResp.Error == nil {
				t.Fatal("expected JSON-RPC error, got nil")
			}
			if mcpResp.Error.Code != mcp.CodeTooManyParamHeaders {
				t.Errorf("Error.Code = %d, want %d (CodeTooManyParamHeaders)", mcpResp.Error.Code, mcp.CodeTooManyParamHeaders)
			}
			// The error message must name only the count and the configured
			// limit — never a header name or value, which §4.3 treats as
			// tool argument content (see collectMCPParamHeaders' own doc).
			for i := 0; i < n; i++ {
				frag := fmt.Sprintf("K%02d", i)
				if strings.Contains(mcpResp.Error.Message, frag) {
					t.Errorf("Error.Message %q contains header-name fragment %q, want only the count and limit", mcpResp.Error.Message, frag)
				}
				frag = fmt.Sprintf("v%02d", i)
				if strings.Contains(mcpResp.Error.Message, frag) {
					t.Errorf("Error.Message %q contains header-value fragment %q, want only the count and limit", mcpResp.Error.Message, frag)
				}
			}
		})
	}
}

// TestCollectMCPParamHeaders_ExactlyMax_Accepted is the boundary case
// TestCollectMCPParamHeaders_TooMany_Rejected's "one more" is defined
// relative to: exactly mcp.MaxParamHeaders (16) validly-named,
// validly-valued Mcp-Param-* headers must still be forwarded in full, not
// rejected — collectMCPParamHeaders' rule 6 triggers on MORE than the
// limit, never AT it.
func TestCollectMCPParamHeaders_ExactlyMax_Accepted(t *testing.T) {
	t.Parallel()

	const n = mcp.MaxParamHeaders // 16
	headers := make(map[string]string, n)
	for i := 0; i < n; i++ {
		headers[fmt.Sprintf("Mcp-Param-K%02d", i)] = fmt.Sprintf("v%02d", i)
	}

	var mu sync.Mutex
	seen := make(map[string]string)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("Mcp-Param-K%02d", i)
			if v := r.Header.Get(name); v != "" {
				seen[strings.ToLower(name)] = v
			}
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestCollectMCPParamHeaders_ExactlyMax_Accepted?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "param-hdr-exactmax")
	key := addMCPTestKey(t, keyCache, org.ID)

	const alias = "param-exactmax-server"
	s := createExternalMCPServer(t, database, alias, upstream.URL)
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPostWithHeaders(t, app, alias, key,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, headers)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (exactly mcp.MaxParamHeaders must still be forwarded in full); body: %s", resp.StatusCode, raw)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // draining is enough

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != n {
		t.Fatalf("upstream saw %d Mcp-Param-K* headers, want exactly %d", len(seen), n)
	}
	for i := 0; i < n; i++ {
		name := strings.ToLower(fmt.Sprintf("mcp-param-k%02d", i))
		want := fmt.Sprintf("v%02d", i)
		got, ok := seen[name]
		if !ok {
			t.Errorf("upstream missing %q, want value %q", name, want)
			continue
		}
		if got != want {
			t.Errorf("upstream %q = %q, want %q", name, got, want)
		}
	}
}

// ---- 4. Auth-header collision: the server's own credential wins -----------

// createMCPServerWithHeaderAuth inserts an MCP server directly into the
// database with AuthType "header" and AuthHeader set to headerName, bypassing
// the Admin API's own reservation check (isReservedMCPHeader,
// mcp_servers.go) entirely — simulating a server registered before that
// validation existed (a legitimate historical state the DB layer itself
// still allows; see mcp_servers.go's CreateMCPServer/UpdateMCPServer, which
// perform no such check). The server is first created with AuthType "none"
// so its ID is known, then updated in place with the real encrypted
// credential — mirroring buildAdHocTransport's own decryption AAD exactly
// via admin.MCPServerAAD.
func createMCPServerWithHeaderAuth(t *testing.T, database *db.DB, alias, upstreamURL, headerName, plaintextToken string) string {
	t.Helper()
	ctx := context.Background()

	s, err := database.CreateMCPServer(ctx, db.CreateMCPServerParams{
		Name:     "Legacy Header-Auth " + alias,
		Alias:    alias,
		URL:      upstreamURL,
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("CreateMCPServer(%q): %v", alias, err)
	}

	encrypted, err := crypto.EncryptString(plaintextToken, testEncryptionKey, admin.MCPServerAAD(s.ID))
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}

	authType := "header"
	if _, err := database.UpdateMCPServer(ctx, s.ID, db.UpdateMCPServerParams{
		AuthType:     &authType,
		AuthHeader:   &headerName,
		AuthTokenEnc: &encrypted,
	}); err != nil {
		t.Fatalf("UpdateMCPServer(%q): %v", alias, err)
	}
	return s.ID
}

// TestCollectMCPParamHeaders_AuthHeaderCollision_CredentialPreserved
// simulates a server whose configured auth_header happens to be
// "Mcp-Param-Token" — impossible via today's Admin API (isReservedMCPHeader
// rejects it), but a real state for a server registered before that
// validation existed. A caller sending its own "Mcp-Param-Token" header must
// never be able to overwrite this server's own credential: rawPost and
// Forward apply authentication to the outbound request BEFORE merging hdr's
// entries in (see their shared doc comment, review finding C4), so without
// collectMCPParamHeaders' rule 5 — dropping a caller-supplied Mcp-Param-*
// header whose name collides with the server's own auth_header —
// forwardHeaders would hand the caller's own value straight into hdr, and
// that ordering would let it clobber the real credential regardless.
func TestCollectMCPParamHeaders_AuthHeaderCollision_CredentialPreserved(t *testing.T) {
	t.Parallel()

	const realCredential = "legacy-server-real-credential-9f2a"
	const attackerValue = "attacker-supplied-value"

	var gotToken string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("Mcp-Param-Token")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestCollectMCPParamHeaders_AuthHeaderCollision?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "param-hdr-authcollision")
	key := addMCPTestKey(t, keyCache, org.ID)

	const alias = "legacy-header-auth-server"
	serverID := createMCPServerWithHeaderAuth(t, database, alias, upstream.URL, "Mcp-Param-Token", realCredential)
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{serverID}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPostWithHeaders(t, app, alias, key,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		map[string]string{"Mcp-Param-Token": attackerValue})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	if gotToken != realCredential {
		t.Errorf("upstream Mcp-Param-Token = %q, want the server's own credential %q (a caller must never override auth_header via Mcp-Param-*)",
			gotToken, realCredential)
	}
}

// ---- 5. Privacy: never routed, never logged, never labeled ----------------

// TestCollectMCPParamHeaders_PrivacyNeverLogged enforces the documented
// invariant on forwardHeaders/collectMCPParamHeaders (mcp_proxy.go): a
// Mcp-Param-{Name} value is a tool argument, forwarded uninterpreted, and
// MUST NEVER be read to make a routing, rate-limiting, metrics-label, or
// usage-logging decision. This test sends a conspicuous sentinel value and
// then inspects EVERY slog record emitted during the request and EVERY
// Prometheus label value currently registered — across the whole default
// registry, not just the MCP metrics — for that sentinel. It goes red the
// instant any code path starts keying a log field, a metric label, or any
// other observable decision on a Mcp-Param-* value.
func TestCollectMCPParamHeaders_PrivacyNeverLogged(t *testing.T) {
	t.Parallel()

	const sentinel = "SENTINEL-TENANT-9f3a1c-do-not-log-or-label-me-5f2b"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(upstream.Close)

	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	dsn := "file:TestCollectMCPParamHeaders_PrivacyNeverLogged?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyAppWithLogger(t, dsn, logger)
	org := mustCreateTestOrg(t, database, "param-hdr-privacy")
	key := addMCPTestKey(t, keyCache, org.ID)

	const alias = "param-privacy-server"
	s := createExternalMCPServer(t, database, alias, upstream.URL)
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPostWithHeaders(t, app, alias, key,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		map[string]string{"Mcp-Param-Tenant": sentinel})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // draining is enough

	if strings.Contains(logBuf.String(), sentinel) {
		t.Errorf("log output contains the Mcp-Param-Tenant sentinel value — it must never be logged:\n%s", logBuf.String())
	}

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if strings.Contains(lp.GetValue(), sentinel) {
					t.Errorf("metric family %q carries the sentinel value in label %q — a Mcp-Param-* value must never become a metric label",
						mf.GetName(), lp.GetName())
				}
			}
		}
	}
}
