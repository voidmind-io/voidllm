package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	"github.com/voidmind-io/voidllm/internal/proxy"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// This file is the full-stack counterpart of internal/mcp's
// session_registry_test.go: it drives the actual attack through the
// real HTTP proxy route (POST /api/v1/mcp/:alias, HandleMCPProxy) with a
// PERSISTENT transport shared across requests via Handler.MCPTransportCache
// — exactly the production wiring (proxy.MCPTransportCache), where one
// *mcp.HTTPTransport, and therefore one *eraBinding, is shared by every
// organization with access to a given global MCP server. Without a
// persistent transport each proxied request would build its own throwaway
// HTTPTransport (see mcp_proxy_adhoc_transport_test.go) and there would be no
// shared state left to attack in the first place.

// setupMCPProxyAppWithTransportCache is like setupMCPProxyApp but wires
// Handler.MCPTransportCache to a real *proxy.MCPTransportCache, so requests
// across multiple proxyPost calls share one persistent *mcp.HTTPTransport per
// server — the property this file's tests exist to exercise. Callers must
// call loadMCPTransportCache after registering and granting access to every
// server the test needs, before making any request that depends on state
// persisting across requests.
func setupMCPProxyAppWithTransportCache(t *testing.T, dsn string) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo], *proxy.MCPTransportCache) {
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
	transportCache := proxy.NewMCPTransportCache(testEncryptionKey, true, 5*time.Second, 5*time.Second, noopLogger(t))
	t.Cleanup(transportCache.Close)

	handler := &admin.Handler{
		DB:                  database,
		HMACSecret:          testHMACSecret,
		EncryptionKey:       testEncryptionKey,
		KeyCache:            keyCache,
		License:             license.NewHolder(license.Verify("", true)),
		Log:                 noopLogger(t),
		MCPServer:           mcp.NewServer("voidllm", "test"),
		MCPCallTimeout:      5 * time.Second,
		MCPAllowPrivateURLs: true, // tests use loopback httptest servers
		MCPTransportCache:   transportCache,
		// Without this, HandleMCPProxy's session check fails CLOSED (see
		// Handler.MCPSessionRegistry's own doc): every caller-supplied
		// Mcp-Session-Id — including one this exact caller was just issued a
		// moment ago — would be silently dropped, breaking legacy session
		// continuity for every test in this file that expects a KNOWN
		// session to be relayed. Production wiring (internal/app) always
		// sets this; tests must mirror that or they exercise a
		// configuration this handler never actually runs with.
		MCPSessionRegistry: mcp.NewSessionRegistry(),
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return app, database, keyCache, transportCache
}

// loadMCPTransportCache reloads transportCache from the current contents of
// database, so every registered, active server has a persistent transport
// ready before the test starts making requests against it.
func loadMCPTransportCache(t *testing.T, database *db.DB, transportCache *proxy.MCPTransportCache) {
	t.Helper()
	servers, err := database.ListMCPServers(context.Background())
	if err != nil {
		t.Fatalf("ListMCPServers: %v", err)
	}
	transportCache.LoadAll(servers)
}

// setupMCPProxyAppWithLogger is like setupMCPProxyApp but wires Handler.Log
// to the given logger instead of a discarding one, so a test can inspect
// what was actually logged for a single proxied request. Transports are
// ad-hoc (no MCPTransportCache), which is fine for a single-request test —
// see setupMCPProxyAppWithTransportCache for the persistent-transport
// variant multi-request tests need instead.
func setupMCPProxyAppWithLogger(t *testing.T, dsn string, logger *slog.Logger) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo]) {
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
		Log:                 logger,
		MCPServer:           mcp.NewServer("voidllm", "test"),
		MCPCallTimeout:      5 * time.Second,
		MCPAllowPrivateURLs: true,
		// See setupMCPProxyAppWithTransportCache's identical field for why
		// this is required, not optional, for a legitimate session to
		// survive the fail-closed check in HandleMCPProxy.
		MCPSessionRegistry: mcp.NewSessionRegistry(),
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return app, database, keyCache
}

// syncBuffer is a concurrency-safe bytes.Buffer, for a slog.Handler's writer
// that may be invoked from more than one goroutine (e.g. HandleMCPProxy's
// SendStreamWriter goroutine) during a single test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// twoOrgFullStackSessionHandler is the full-stack counterpart of
// internal/mcp's twoOrgSessionHandler: it mints a fresh, sequentially
// numbered Mcp-Session-Id whenever it receives a request with no inbound
// Mcp-Session-Id header, and otherwise sets no response header at all. Every
// request's inbound header value is appended, in order, to seen. mu guards
// seen and the mint counter.
func twoOrgFullStackSessionHandler(seen *[]string, mu *sync.Mutex) http.Handler {
	var mintCount int
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inbound := r.Header.Get("Mcp-Session-Id")
		mu.Lock()
		*seen = append(*seen, inbound)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if inbound == "" {
			mu.Lock()
			mintCount++
			session := fmt.Sprintf("fullstack-session-%d", mintCount)
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", session)
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	})
}

// TestMCPProxy_CrossOrgSessionGuessing_Blocked_FullStack is the full-stack
// counterpart of internal/mcp's TestForward_Legacy_CrossOrgSessionGuessing_
// Blocked: it drives the exact same attack sequence through the real HTTP
// proxy route with two real organizations, two real API keys, and one real
// global MCP server registered in the database, sharing one persistent
// transport exactly as production does.
//
//  1. Org A's first request (no session) — the upstream mints S1.
//  2. Org A's follow-up carrying S1 — the upstream actually sees S1.
//  3. Org B sends a request carrying Org A's own S1 — the upstream must see
//     NO session at all, and Org B's client gets an ordinary 200, not an
//     error (an error would itself be an oracle for S1's existence).
//  4. Org B initializes its own S2, distinct from S1, and can use it. S1
//     remains unreachable for Org B even afterward.
func TestMCPProxy_CrossOrgSessionGuessing_Blocked_FullStack(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seenByUpstream []string

	upstream := httptest.NewServer(twoOrgFullStackSessionHandler(&seenByUpstream, &mu))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_CrossOrgSessionGuessing_Blocked_FullStack?mode=memory&cache=private"
	app, database, keyCache, transportCache := setupMCPProxyAppWithTransportCache(t, dsn)

	orgA := mustCreateTestOrg(t, database, "cross-org-attack-a")
	orgB := mustCreateTestOrg(t, database, "cross-org-attack-b")
	keyA := addMCPTestKey(t, keyCache, orgA.ID)
	keyB := addMCPTestKey(t, keyCache, orgB.ID)

	const alias = "cross-org-attack-server"
	serverID := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2025-03-26")
	if err := database.SetOrgMCPAccess(context.Background(), orgA.ID, []string{serverID}); err != nil {
		t.Fatalf("SetOrgMCPAccess(orgA): %v", err)
	}
	if err := database.SetOrgMCPAccess(context.Background(), orgB.ID, []string{serverID}); err != nil {
		t.Fatalf("SetOrgMCPAccess(orgB): %v", err)
	}
	loadMCPTransportCache(t, database, transportCache)

	const body = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	// Step 1: Org A's first request — no inbound session — upstream mints S1.
	respA1 := proxyPost(t, app, alias, keyA, body)
	s1 := respA1.Header.Get("Mcp-Session-Id")
	respA1.Body.Close()
	if s1 == "" {
		t.Fatal("Org A's first request: upstream did not mint a session, test setup is broken")
	}

	// Step 2: Org A follows up with S1 — the upstream must actually see it.
	respA2 := proxyPostWithHeaders(t, app, alias, keyA, body, map[string]string{"Mcp-Session-Id": s1})
	respA2.Body.Close()

	mu.Lock()
	if len(seenByUpstream) != 2 || seenByUpstream[1] != s1 {
		mu.Unlock()
		t.Fatalf("Org A follow-up: upstream saw %v, want the second entry to be Org A's own session %q", seenByUpstream, s1)
	}
	mu.Unlock()

	// Step 3: THE ATTACK — Org B sends a request carrying Org A's own S1,
	// guessed or reused from a predictable legacy server's session scheme.
	respAttack := proxyPostWithHeaders(t, app, alias, keyB, body, map[string]string{"Mcp-Session-Id": s1})
	if respAttack.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(respAttack.Body)
		respAttack.Body.Close()
		t.Fatalf("Org B attack attempt: status = %d, want 200 (dropping, not rejecting the unknown session); body: %s", respAttack.StatusCode, raw)
	}
	respAttack.Body.Close()

	mu.Lock()
	if len(seenByUpstream) != 3 {
		mu.Unlock()
		t.Fatalf("upstream saw %d requests after the attack attempt, want 3", len(seenByUpstream))
	}
	gotAttack := seenByUpstream[2]
	mu.Unlock()
	if gotAttack == s1 {
		t.Fatalf("upstream saw Org A's own session %q on Org B's request through the real HTTP proxy route — "+
			"cross-org session guessing was NOT blocked", s1)
	}
	if gotAttack != "" {
		t.Errorf("upstream saw Mcp-Session-Id = %q on Org B's guessed request, want empty (dropped, not substituted)", gotAttack)
	}

	// Step 4: Org B initializes its own session S2, distinct from S1.
	respB1 := proxyPost(t, app, alias, keyB, body)
	s2 := respB1.Header.Get("Mcp-Session-Id")
	respB1.Body.Close()
	if s2 == "" {
		t.Fatal("Org B's own first request: upstream did not mint a session, test setup is broken")
	}
	if s2 == s1 {
		t.Fatalf("Org B was issued the SAME session as Org A (%q) — sessions are not isolated across orgs", s1)
	}

	respB2 := proxyPostWithHeaders(t, app, alias, keyB, body, map[string]string{"Mcp-Session-Id": s2})
	respB2.Body.Close()

	mu.Lock()
	if len(seenByUpstream) != 5 || seenByUpstream[4] != s2 {
		mu.Unlock()
		t.Fatalf("Org B follow-up: upstream saw %v, want the fifth entry to be Org B's own session %q", seenByUpstream, s2)
	}
	mu.Unlock()

	// S1 remains permanently unreachable for Org B, even after Org B has its
	// own, legitimately-issued session.
	respRetry := proxyPostWithHeaders(t, app, alias, keyB, body, map[string]string{"Mcp-Session-Id": s1})
	respRetry.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(seenByUpstream) != 6 {
		t.Fatalf("upstream saw %d requests after the retry, want 6", len(seenByUpstream))
	}
	if seenByUpstream[5] == s1 {
		t.Fatalf("Org B reached Org A's session (%q) on a later attempt through the real HTTP proxy route — "+
			"isolation only held once, not permanently", s1)
	}
}

// TestMCPProxy_UnknownSession_NoOracle_FullStack is the full-stack
// counterpart of internal/mcp's TestForward_Legacy_UnknownSession_NoOracle:
// a request carrying a fabricated Mcp-Session-Id must produce a response
// indistinguishable — same status code, same body — from a request carrying
// no session header at all, through the real HTTP proxy route.
func TestMCPProxy_UnknownSession_NoOracle_FullStack(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var gotHeaders []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHeaders = append(gotHeaders, r.Header.Get("Mcp-Session-Id"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_UnknownSession_NoOracle_FullStack?mode=memory&cache=private"
	app, database, keyCache, transportCache := setupMCPProxyAppWithTransportCache(t, dsn)
	org := mustCreateTestOrg(t, database, "no-oracle-fullstack")
	key := addMCPTestKey(t, keyCache, org.ID)

	const alias = "no-oracle-fullstack-server"
	serverID := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2025-03-26")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{serverID}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}
	loadMCPTransportCache(t, database, transportCache)

	const body = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	respFabricated := proxyPostWithHeaders(t, app, alias, key, body, map[string]string{"Mcp-Session-Id": "totally-fabricated-session-guess"})
	fabricatedStatus := respFabricated.StatusCode
	fabricatedBody, err := io.ReadAll(respFabricated.Body)
	respFabricated.Body.Close()
	if err != nil {
		t.Fatalf("read fabricated-session response body: %v", err)
	}

	respNone := proxyPost(t, app, alias, key, body)
	noneStatus := respNone.StatusCode
	noneBody, err := io.ReadAll(respNone.Body)
	respNone.Body.Close()
	if err != nil {
		t.Fatalf("read no-session response body: %v", err)
	}

	if fabricatedStatus != noneStatus {
		t.Errorf("status with a fabricated session = %d, status with no session = %d, want identical (no oracle)", fabricatedStatus, noneStatus)
	}
	if string(fabricatedBody) != string(noneBody) {
		t.Errorf("body with a fabricated session = %q, body with no session = %q, want identical", fabricatedBody, noneBody)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotHeaders) != 2 || gotHeaders[0] != "" || gotHeaders[1] != "" {
		t.Errorf("upstream saw Mcp-Session-Id headers = %v, want both empty — the fabricated session must never reach the upstream, identically to the no-session case", gotHeaders)
	}
}

// TestMCPProxy_UnknownSessionHeader_NeverLogged verifies the zero-knowledge
// logging guarantee Forward's own doc claims for a discarded session header
// (docs/mcp-v2.md §11.5: a session ID is a bearer credential): an inbound
// Mcp-Session-Id that fails the forwardedSessionKnown check must never appear
// anywhere in VoidLLM's own logs — not the full value, not a truncated
// prefix, not at Debug level. A conspicuous sentinel is used as the session
// ID and a real *slog.Logger (Debug level, writing to a buffer this test
// inspects) is wired in as Handler.Log for the whole request.
func TestMCPProxy_UnknownSessionHeader_NeverLogged(t *testing.T) {
	t.Parallel()

	const sentinelSession = "SENTINEL-SESSION-NEVER-ISSUED-9f3a1c-do-not-log-me-5f2b"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(upstream.Close)

	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	dsn := "file:TestMCPProxy_UnknownSessionHeader_NeverLogged?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyAppWithLogger(t, dsn, logger)
	org := mustCreateTestOrg(t, database, "no-log-session")
	key := addMCPTestKey(t, keyCache, org.ID)

	const alias = "no-log-session-server"
	serverID := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2025-03-26")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{serverID}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPostWithHeaders(t, app, alias, key, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		map[string]string{"Mcp-Session-Id": sentinelSession})
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	resp.Body.Close()

	logged := logBuf.String()
	if strings.Contains(logged, sentinelSession) {
		t.Errorf("log output contains the dropped session ID %q in full, want it never logged", sentinelSession)
	}
	// A future change that logs only a prefix ("truncated, not full") would
	// still violate the contract — check for that too.
	const prefixLen = 24
	if strings.Contains(logged, sentinelSession[:prefixLen]) {
		t.Errorf("log output contains a %d-byte truncated prefix of the dropped session ID, want no trace of it at all", prefixLen)
	}
}

// addMCPTestKeyWithID is like addMCPTestKey but lets the caller pin
// auth.KeyInfo.ID to a specific value, instead of the fixed
// "mcp-proxy-key-id" every addMCPTestKey-minted key shares. Needed whenever a
// test must distinguish between two DIFFERENT keys belonging to the SAME
// organization: mcp.NewClientSessionScope is keyed on (OrgID, key ID), so two
// keys sharing addMCPTestKey's hardcoded ID would collide into the same scope
// and make such a test meaningless.
func addMCPTestKeyWithID(t *testing.T, keyCache *cache.Cache[string, auth.KeyInfo], orgID, keyID string) string {
	t.Helper()
	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	hash := keygen.Hash(plaintext, testHMACSecret)
	keyCache.Set(hash, auth.KeyInfo{
		ID:      keyID,
		KeyType: keygen.KeyTypeUser,
		Role:    auth.RoleMember,
		OrgID:   orgID,
		Name:    "mcp proxy test key " + keyID,
	})
	return plaintext
}

// ---- The dual-era hole: binding resolves modern but the same endpoint -----
// ---- still serves headerless legacy traffic --------------------------------

// dualEraUpstreamHandler returns an httptest handler for an upstream that
// answers server/discover with a genuine MODERN result — so
// *mcp.HTTPTransport's own era probe (resolveBinding, docs/mcp-v2.md §4.6)
// settles on EraModern for this server — while STILL accepting a headerless
// legacy "initialize" at the very same endpoint, minting a fresh
// Mcp-Session-Id for it exactly as a dual-era server is explicitly permitted
// to do (docs/mcp-v2.md §4.7: "Er DARF beide Ären gleichzeitig auf demselben
// Endpoint bedienen"). Every request's JSON-RPC method and inbound
// Mcp-Session-Id header are recorded, in order. mu guards both slices and
// mintCount.
func dualEraUpstreamHandler(seenMethods, seenInbound *[]string, mu *sync.Mutex) http.Handler {
	var mintCount int
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)
		inbound := r.Header.Get("Mcp-Session-Id")

		mu.Lock()
		*seenMethods = append(*seenMethods, rpc.Method)
		*seenInbound = append(*seenInbound, inbound)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch rpc.Method {
		case "server/discover":
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":"discover-1","result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{},"ttlMs":0,"cacheScope":"public"}}`)
		case "initialize":
			if inbound == "" {
				mu.Lock()
				mintCount++
				session := fmt.Sprintf("dual-era-session-%d", mintCount)
				mu.Unlock()
				w.Header().Set("Mcp-Session-Id", session)
			}
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`)
		default:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		}
	})
}

// TestMCPProxy_DualEraUpstream_ModernBindingStillLeaksLegacySessions_WithoutHeaderGatedCheck
// is the single most important regression test in this file: it reproduces
// exactly the hole an intermediate, since-reverted revision of the
// session-isolation fix left open (see mcp.HTTPTransport.Forward's own doc,
// the "Forward is session-blind" section in internal/mcp's
// http_transport_forward_test.go, and mcp.SessionRegistry's own doc). A
// dual-era upstream answers server/discover with a genuine MODERN result —
// so THIS *mcp.HTTPTransport's era resolution (shared across every
// organization via the persistent MCPTransportCache) settles on EraModern —
// while still accepting a headerless legacy "initialize" at the very same
// endpoint, exactly as docs/mcp-v2.md §4.7 permits a dual-era server to do.
//
// Against the intermediate, WRONG design — which gated the session check on
// `eraBinding.Era == EraLegacy` — this exact scenario would have skipped the
// check entirely: the resolved binding LOOKS modern (server/discover
// succeeded), so the check never ran for it, and Org A's legacy session
// would have sailed straight through to Org B on nothing but a guess. The
// check now gates on whether the CALLER's own request carries an
// Mcp-Session-Id header at all — completely independent of what era
// anything resolved to — so this exact scenario must still be blocked.
func TestMCPProxy_DualEraUpstream_ModernBindingStillLeaksLegacySessions_WithoutHeaderGatedCheck(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seenMethods, seenInbound []string

	upstream := httptest.NewServer(dualEraUpstreamHandler(&seenMethods, &seenInbound, &mu))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_DualEraUpstream_ModernBindingStillLeaksLegacySessions?mode=memory&cache=private"
	app, database, keyCache, transportCache := setupMCPProxyAppWithTransportCache(t, dsn)

	orgA := mustCreateTestOrg(t, database, "dual-era-org-a")
	orgB := mustCreateTestOrg(t, database, "dual-era-org-b")
	keyA := addMCPTestKey(t, keyCache, orgA.ID)
	keyB := addMCPTestKeyWithID(t, keyCache, orgB.ID, "dual-era-key-b")

	const alias = "dual-era-server"
	// Deliberately UNPINNED (createExternalMCPServer, not the "Pinned"
	// variant): the whole point of this test is that era resolution runs for
	// real, via a genuine server/discover round trip, and settles on
	// EraModern.
	serverID := createExternalMCPServer(t, database, alias, upstream.URL)
	if err := database.SetOrgMCPAccess(context.Background(), orgA.ID, []string{serverID}); err != nil {
		t.Fatalf("SetOrgMCPAccess(orgA): %v", err)
	}
	if err := database.SetOrgMCPAccess(context.Background(), orgB.ID, []string{serverID}); err != nil {
		t.Fatalf("SetOrgMCPAccess(orgB): %v", err)
	}
	loadMCPTransportCache(t, database, transportCache)

	// Org A opens with a plain legacy "initialize" — no MCP-Protocol-Version
	// header, no modern _meta — exactly what a real legacy MCP client SDK
	// sends. This is the FIRST request through the persistent transport, so
	// resolveBinding also runs its own server/discover probe ahead of it;
	// that probe's own request carries no Mcp-Session-Id and is not itself
	// interesting here.
	const legacyInit = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"legacy-client","version":"1.0"}}}`
	respA := proxyPost(t, app, alias, keyA, legacyInit)
	if respA.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(respA.Body)
		respA.Body.Close()
		t.Fatalf("Org A initialize: status = %d, want 200; body: %s", respA.StatusCode, raw)
	}
	s1 := respA.Header.Get("Mcp-Session-Id")
	respA.Body.Close()
	if s1 == "" {
		t.Fatal("Org A initialize: upstream did not mint a session, test setup is broken")
	}

	// Sanity check on the test's own premise: the upstream really was probed
	// with server/discover — otherwise this test would not actually be
	// exercising the dual-era scenario it claims to.
	mu.Lock()
	var sawDiscover bool
	for _, m := range seenMethods {
		if m == "server/discover" {
			sawDiscover = true
			break
		}
	}
	mu.Unlock()
	if !sawDiscover {
		t.Fatal("upstream never received a server/discover probe — test setup is broken, era resolution did not run")
	}

	// THE ATTACK: Org B carries Org A's own S1. Against the intermediate,
	// era-gated design, this binding "looks" modern, so the check would never
	// run and S1 would leak straight through.
	respAttack := proxyPostWithHeaders(t, app, alias, keyB, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		map[string]string{"Mcp-Session-Id": s1})
	if respAttack.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(respAttack.Body)
		respAttack.Body.Close()
		t.Fatalf("Org B attack attempt: status = %d, want 200 (dropped, not rejected); body: %s", respAttack.StatusCode, raw)
	}
	respAttack.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(seenInbound) == 0 || len(seenMethods) == 0 {
		t.Fatal("upstream saw no requests at all, test setup is broken")
	}
	lastInbound := seenInbound[len(seenInbound)-1]
	lastMethod := seenMethods[len(seenMethods)-1]
	if lastMethod != "tools/list" {
		t.Fatalf("last request upstream saw had method %q, want %q — test bookkeeping is broken", lastMethod, "tools/list")
	}
	if lastInbound == s1 {
		t.Fatalf("upstream saw Org A's own legacy session %q on Org B's request, even though the binding resolved "+
			"to the MODERN era — the check must gate on header presence, not on binding era; this is exactly the "+
			"hole an intermediate revision of the fix left open", s1)
	}
	if lastInbound != "" {
		t.Errorf("upstream saw Mcp-Session-Id = %q on Org B's guessed request, want empty (dropped, not substituted)", lastInbound)
	}
}

// ---- Ad-hoc transport (no MCPTransportCache) does not destroy the session -

// TestMCPProxy_NoTransportCache_AdHocTransport_SessionSurvivesAcrossRequests
// is the regression test for the second design correction docs/mcp-v2.md
// names: running WITHOUT Handler.MCPTransportCache is an explicitly
// supported mode (every request builds its own ad-hoc *mcp.HTTPTransport via
// buildAdHocTransport, closed again once that single request finishes — see
// setupMCPProxyAppWithLogger, which never sets MCPTransportCache). Before the
// fix, the session bookkeeping lived on the per-request *eraBinding itself,
// so it was discarded the moment that ad-hoc transport was closed — a
// legitimate session minted while answering request 1 was already gone by
// the time request 2 arrived. It now lives on Handler.MCPSessionRegistry,
// held independently of any single transport's lifetime, so it must survive.
func TestMCPProxy_NoTransportCache_AdHocTransport_SessionSurvivesAcrossRequests(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seenInbound []string
	var mintCount int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inbound := r.Header.Get("Mcp-Session-Id")
		mu.Lock()
		seenInbound = append(seenInbound, inbound)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if inbound == "" {
			mu.Lock()
			mintCount++
			session := fmt.Sprintf("adhoc-session-%d", mintCount)
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", session)
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_NoTransportCache_AdHocTransport_SessionSurvives?mode=memory&cache=private"
	// setupMCPProxyAppWithLogger deliberately configures NO MCPTransportCache
	// — see its own doc — so every proxied request below builds and closes
	// its own, independent ad-hoc *mcp.HTTPTransport.
	app, database, keyCache := setupMCPProxyAppWithLogger(t, dsn, noopLogger(t))
	org := mustCreateTestOrg(t, database, "adhoc-session-survival")
	key := addMCPTestKey(t, keyCache, org.ID)

	const alias = "adhoc-session-survival-server"
	serverID := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2025-03-26")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{serverID}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	const body = `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	resp1 := proxyPost(t, app, alias, key, body)
	if resp1.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp1.Body)
		resp1.Body.Close()
		t.Fatalf("first request: status = %d, want 200; body: %s", resp1.StatusCode, raw)
	}
	s1 := resp1.Header.Get("Mcp-Session-Id")
	resp1.Body.Close()
	if s1 == "" {
		t.Fatal("first request: upstream did not mint a session, test setup is broken")
	}

	// Second request, through a BRAND NEW ad-hoc transport (there is no
	// cache), carries the session the first ad-hoc transport's own upstream
	// call received.
	resp2 := proxyPostWithHeaders(t, app, alias, key, body, map[string]string{"Mcp-Session-Id": s1})
	if resp2.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		t.Fatalf("second request: status = %d, want 200; body: %s", resp2.StatusCode, raw)
	}
	resp2.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(seenInbound) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(seenInbound))
	}
	if seenInbound[1] != s1 {
		t.Errorf("upstream saw Mcp-Session-Id = %q on the second (ad-hoc-transport) request, want %q — the session "+
			"must survive across separate ad-hoc transports, since it now lives on Handler.MCPSessionRegistry, not "+
			"on any one *mcp.HTTPTransport", seenInbound[1], s1)
	}
}

// ---- Scope is (organization, API key), not organization alone -------------

// TestMCPProxy_ScopeIsOrgAndKey_SiblingKeySameOrg_ChurnDoesNotEvictOthersSessions
// is the regression test for scoping SessionRegistry lookups one level finer
// than the organization alone (mcp.NewClientSessionScope's own doc): TWO
// keys belonging to the SAME organization must never share — or evict — each
// other's tracked sessions. Key 1 mints 65 sessions (one more than
// maxSessionsPerScope), evicting key 1's own oldest session; key 2's single,
// unrelated session must remain valid throughout and afterward.
func TestMCPProxy_ScopeIsOrgAndKey_SiblingKeySameOrg_ChurnDoesNotEvictOthersSessions(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seenInbound []string
	var mintCount int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inbound := r.Header.Get("Mcp-Session-Id")
		mu.Lock()
		seenInbound = append(seenInbound, inbound)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if inbound == "" {
			mu.Lock()
			mintCount++
			session := fmt.Sprintf("sibling-key-session-%d", mintCount)
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", session)
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_ScopeIsOrgAndKey_SiblingKeySameOrg?mode=memory&cache=private"
	app, database, keyCache, transportCache := setupMCPProxyAppWithTransportCache(t, dsn)
	org := mustCreateTestOrg(t, database, "sibling-key-org")
	key1 := addMCPTestKeyWithID(t, keyCache, org.ID, "sibling-key-1")
	key2 := addMCPTestKeyWithID(t, keyCache, org.ID, "sibling-key-2")

	const alias = "sibling-key-server"
	serverID := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2025-03-26")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{serverID}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}
	loadMCPTransportCache(t, database, transportCache)

	const body = `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	// Key 2 primes its own single session first.
	resp2Prime := proxyPost(t, app, alias, key2, body)
	if resp2Prime.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp2Prime.Body)
		resp2Prime.Body.Close()
		t.Fatalf("key2 priming: status = %d, want 200; body: %s", resp2Prime.StatusCode, raw)
	}
	sessionKey2 := resp2Prime.Header.Get("Mcp-Session-Id")
	resp2Prime.Body.Close()
	if sessionKey2 == "" {
		t.Fatal("key2 priming: upstream did not mint a session, test setup is broken")
	}

	// Key 1 mints 65 sessions in a row — headerless requests, so the upstream
	// mints a fresh one every time — one more than maxSessionsPerScope,
	// evicting key 1's OWN oldest entry.
	const key1SessionCount = 65
	for i := 0; i < key1SessionCount; i++ {
		resp := proxyPost(t, app, alias, key1, body)
		if resp.StatusCode != fiber.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("key1 mint #%d: status = %d, want 200; body: %s", i, resp.StatusCode, raw)
		}
		resp.Body.Close()
	}

	// Key 2's own session, untouched by any of key 1's churn, must still be
	// relayed — proving the scope is (org, key), not org alone, since a
	// naive org-only scope would have had key 1's 65 mints evict it long ago.
	respKey2Follow := proxyPostWithHeaders(t, app, alias, key2, body, map[string]string{"Mcp-Session-Id": sessionKey2})
	if respKey2Follow.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(respKey2Follow.Body)
		respKey2Follow.Body.Close()
		t.Fatalf("key2 follow-up: status = %d, want 200; body: %s", respKey2Follow.StatusCode, raw)
	}
	respKey2Follow.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	last := seenInbound[len(seenInbound)-1]
	if last != sessionKey2 {
		t.Errorf("upstream saw Mcp-Session-Id = %q on key2's follow-up after key1 minted %d sessions, want %q — "+
			"key1's churn must never evict key2's own session, since the scope is (org, key), not org alone",
			last, key1SessionCount, sessionKey2)
	}
}

// ---- Fail-closed: a missing MCPSessionRegistry drops every session, -------
// ---- even the caller's own, just-issued one --------------------------------

// TestMCPProxy_MissingSessionRegistry_FailsClosed_DropsEvenTheCallersOwnSession
// is a deliberate regression guard against silently disabling tenant
// isolation: a Handler constructed WITHOUT MCPSessionRegistry (nil, its zero
// value) must never fall back to "relay everything unchecked" — that would
// be exactly the pre-fix behavior this whole mechanism replaced, reintroduced
// by nothing more than a forgotten field during Handler construction.
// Fail-closed instead means EVERY caller-supplied session is treated as
// unrecognized, INCLUDING one this exact caller was issued a moment ago by
// this exact upstream. Production wiring (internal/app) always sets this
// field; this test exists so that if it is ever forgotten there, the
// regression shows up as an immediate, loud correctness break (legacy
// sessions stop working at all) rather than a silent security hole nothing
// else would catch.
func TestMCPProxy_MissingSessionRegistry_FailsClosed_DropsEvenTheCallersOwnSession(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seenInbound []string
	var mintCount int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inbound := r.Header.Get("Mcp-Session-Id")
		mu.Lock()
		seenInbound = append(seenInbound, inbound)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if inbound == "" {
			mu.Lock()
			mintCount++
			session := fmt.Sprintf("fail-closed-session-%d", mintCount)
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", session)
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_MissingSessionRegistry_FailsClosed?mode=memory&cache=private"
	// setupMCPProxyApp (unlike the two helpers above) deliberately leaves
	// Handler.MCPSessionRegistry at its nil zero value — exactly the
	// misconfiguration this test guards against.
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "fail-closed-org")
	key := addMCPTestKey(t, keyCache, org.ID)

	const alias = "fail-closed-server"
	serverID := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2025-03-26")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{serverID}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	const body = `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	resp1 := proxyPost(t, app, alias, key, body)
	if resp1.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp1.Body)
		resp1.Body.Close()
		t.Fatalf("first request: status = %d, want 200; body: %s", resp1.StatusCode, raw)
	}
	s1 := resp1.Header.Get("Mcp-Session-Id")
	resp1.Body.Close()
	if s1 == "" {
		t.Fatal("first request: upstream did not mint a session, test setup is broken")
	}

	resp2 := proxyPostWithHeaders(t, app, alias, key, body, map[string]string{"Mcp-Session-Id": s1})
	if resp2.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		t.Fatalf("second request: status = %d, want 200 (dropped, not rejected); body: %s", resp2.StatusCode, raw)
	}
	resp2.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(seenInbound) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(seenInbound))
	}
	if seenInbound[1] == s1 {
		t.Fatalf("upstream saw the caller's own just-issued session %q on its own follow-up request, despite "+
			"Handler.MCPSessionRegistry being nil — want fail-closed: EVERY session dropped when the registry is "+
			"missing, not merely cross-tenant ones", s1)
	}
	if seenInbound[1] != "" {
		t.Errorf("upstream saw Mcp-Session-Id = %q on the second request, want empty", seenInbound[1])
	}
}

// ---- No oracle: a real (but foreign) session and a fabricated one are -----
// ---- indistinguishable from carrying no header at all ----------------------

// TestMCPProxy_ForeignVsFabricatedVsNoSession_AllThreeIndistinguishable
// extends TestMCPProxy_UnknownSession_NoOracle_FullStack: it is not enough
// for a FABRICATED session ID to be indistinguishable from no session at
// all — a REAL session ID, genuinely issued by this exact upstream but to a
// DIFFERENT organization, must be equally indistinguishable. If it weren't —
// say, a different status code or body for "this session exists, but not for
// you" versus "no such session anywhere" — an attacker could use that
// difference to enumerate which guessed session IDs are real, narrowing a
// brute-force search even though no individual guess is ever honored.
func TestMCPProxy_ForeignVsFabricatedVsNoSession_AllThreeIndistinguishable(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seenInbound []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inbound := r.Header.Get("Mcp-Session-Id")
		mu.Lock()
		seenInbound = append(seenInbound, inbound)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if inbound == "" {
			w.Header().Set("Mcp-Session-Id", "foreign-orgs-real-session")
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_ForeignVsFabricatedVsNoSession?mode=memory&cache=private"
	app, database, keyCache, transportCache := setupMCPProxyAppWithTransportCache(t, dsn)
	orgForeign := mustCreateTestOrg(t, database, "no-oracle-foreign-org")
	orgVictim := mustCreateTestOrg(t, database, "no-oracle-victim-org")
	keyForeign := addMCPTestKey(t, keyCache, orgForeign.ID)
	keyVictim := addMCPTestKeyWithID(t, keyCache, orgVictim.ID, "no-oracle-victim-key")

	const alias = "no-oracle-extended-server"
	serverID := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2025-03-26")
	if err := database.SetOrgMCPAccess(context.Background(), orgForeign.ID, []string{serverID}); err != nil {
		t.Fatalf("SetOrgMCPAccess(foreign): %v", err)
	}
	if err := database.SetOrgMCPAccess(context.Background(), orgVictim.ID, []string{serverID}); err != nil {
		t.Fatalf("SetOrgMCPAccess(victim): %v", err)
	}
	loadMCPTransportCache(t, database, transportCache)

	const body = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	// The "foreign" org gets a REAL session, genuinely issued by this exact
	// upstream.
	respForeign := proxyPost(t, app, alias, keyForeign, body)
	realForeignSession := respForeign.Header.Get("Mcp-Session-Id")
	respForeign.Body.Close()
	if realForeignSession == "" {
		t.Fatal("foreign org priming: upstream did not mint a session, test setup is broken")
	}

	// Now compare three requests from the VICTIM org: one carrying the real
	// foreign session, one carrying a fabricated one, and one carrying none.
	respRealForeign := proxyPostWithHeaders(t, app, alias, keyVictim, body, map[string]string{"Mcp-Session-Id": realForeignSession})
	realForeignStatus := respRealForeign.StatusCode
	realForeignBody, err := io.ReadAll(respRealForeign.Body)
	respRealForeign.Body.Close()
	if err != nil {
		t.Fatalf("read real-foreign-session response body: %v", err)
	}

	respFabricated := proxyPostWithHeaders(t, app, alias, keyVictim, body, map[string]string{"Mcp-Session-Id": "totally-fabricated-guess"})
	fabricatedStatus := respFabricated.StatusCode
	fabricatedBody, err := io.ReadAll(respFabricated.Body)
	respFabricated.Body.Close()
	if err != nil {
		t.Fatalf("read fabricated-session response body: %v", err)
	}

	respNone := proxyPost(t, app, alias, keyVictim, body)
	noneStatus := respNone.StatusCode
	noneBody, err := io.ReadAll(respNone.Body)
	respNone.Body.Close()
	if err != nil {
		t.Fatalf("read no-session response body: %v", err)
	}

	if realForeignStatus != noneStatus || fabricatedStatus != noneStatus {
		t.Errorf("status codes = real-foreign:%d fabricated:%d none:%d, want all three identical",
			realForeignStatus, fabricatedStatus, noneStatus)
	}
	if string(realForeignBody) != string(noneBody) || string(fabricatedBody) != string(noneBody) {
		t.Errorf("bodies differ: real-foreign=%q fabricated=%q none=%q, want all three identical",
			realForeignBody, fabricatedBody, noneBody)
	}

	mu.Lock()
	defer mu.Unlock()
	// seenInbound[0] is the priming call for the foreign org (headerless).
	// The remaining three correspond, in order, to the real-foreign,
	// fabricated, and no-session requests above — all must reach the
	// upstream with NO session header, indistinguishably.
	if len(seenInbound) != 4 {
		t.Fatalf("upstream saw %d requests, want 4", len(seenInbound))
	}
	for i, label := range []string{"real-foreign", "fabricated", "none"} {
		if got := seenInbound[i+1]; got != "" {
			t.Errorf("%s request: upstream saw Mcp-Session-Id = %q, want empty", label, got)
		}
	}
}

// ---- Concurrency: many parallel requests from different scopes, -race -----

// TestMCPProxy_ManyParallelScopes_ConcurrentRequests_NoMixing drives many
// concurrent proxied requests, spread across several distinct (organization,
// key) scopes, through the real HTTP proxy route and a single PERSISTENT
// transport — the exact production shape (proxy.MCPTransportCache shares one
// *mcp.HTTPTransport, and therefore one Handler.MCPSessionRegistry entry per
// server, across every caller). Each scope primes its own session first,
// then repeatedly checks it is relayed while a NEIGHBOR scope's session is
// never relayed in its place. Run with -race.
//
// The upstream always answers with an Mcp-Session-Id response header — the
// inbound value echoed back verbatim if non-empty, or a freshly minted one
// otherwise — rather than a custom header: HandleMCPProxy's transparent
// pass-through only ever mirrors an ALLOWLISTED set of response headers back
// to the caller (copyMCPResponseHeaders, mcp_headers.go), and an invented
// header name would be silently dropped there, making it useless as a signal
// of what the upstream actually received.
//
// t.Fatalf/t.Errorf are never called from the worker goroutines below —
// only from the main test goroutine, via errCh — because *testing.T.FailNow
// (which Fatalf calls) is documented as unsafe to call from any goroutine
// other than the one running the test function itself.
func TestMCPProxy_ManyParallelScopes_ConcurrentRequests_NoMixing(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var mintCount int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inbound := r.Header.Get("Mcp-Session-Id")
		w.Header().Set("Content-Type", "application/json")
		if inbound != "" {
			w.Header().Set("Mcp-Session-Id", inbound)
		} else {
			mu.Lock()
			mintCount++
			session := fmt.Sprintf("concurrent-proxy-session-%d", mintCount)
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", session)
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_ManyParallelScopes_ConcurrentRequests?mode=memory&cache=private"
	app, database, keyCache, transportCache := setupMCPProxyAppWithTransportCache(t, dsn)

	const alias = "concurrent-scopes-server"
	serverID := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2025-03-26")

	const numScopes = 10
	keys := make([]string, numScopes)
	for i := 0; i < numScopes; i++ {
		org := mustCreateTestOrg(t, database, fmt.Sprintf("concurrent-scope-org-%d", i))
		keys[i] = addMCPTestKeyWithID(t, keyCache, org.ID, fmt.Sprintf("concurrent-scope-key-%d", i))
		if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{serverID}); err != nil {
			t.Fatalf("SetOrgMCPAccess(scope %d): %v", i, err)
		}
	}
	loadMCPTransportCache(t, database, transportCache)

	const body = `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	// Prime each scope's own session sequentially, before the concurrent
	// phase, so every scope already has a known-good session to assert
	// against below.
	ownSession := make([]string, numScopes)
	for i := 0; i < numScopes; i++ {
		resp := proxyPost(t, app, alias, keys[i], body)
		if resp.StatusCode != fiber.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("priming scope %d: status = %d, want 200; body: %s", i, resp.StatusCode, raw)
		}
		ownSession[i] = resp.Header.Get("Mcp-Session-Id")
		resp.Body.Close()
		if ownSession[i] == "" {
			t.Fatalf("priming scope %d: upstream did not mint a session, test setup is broken", i)
		}
	}

	const roundsPerScope = 15
	var wg sync.WaitGroup
	errCh := make(chan string, numScopes*roundsPerScope)

	for i := 0; i < numScopes; i++ {
		i := i
		for r := 0; r < roundsPerScope; r++ {
			wg.Add(1)
			go func(r int) {
				defer wg.Done()

				// Alternate between this scope's own, legitimately-issued
				// session and a NEIGHBOR scope's session it was never issued
				// — the neighbor's traffic must never leak in.
				foreignIdx := (i + 1) % numScopes
				useOwn := r%2 == 0
				sid := ownSession[i]
				if !useOwn {
					sid = ownSession[foreignIdx]
				}

				req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/"+alias, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+keys[i])
				req.Header.Set("Mcp-Session-Id", sid)
				resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
				if err != nil {
					errCh <- fmt.Sprintf("scope %d round %d: app.Test error = %v", i, r, err)
					return
				}
				got := resp.Header.Get("Mcp-Session-Id")
				gotBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()

				if resp.StatusCode != fiber.StatusOK {
					errCh <- fmt.Sprintf("scope %d round %d: status = %d, want 200; body: %s", i, r, resp.StatusCode, gotBody)
					return
				}

				if useOwn && got != ownSession[i] {
					errCh <- fmt.Sprintf("scope %d round %d: upstream saw/echoed %q for scope's OWN session, want %q", i, r, got, ownSession[i])
				}
				// !useOwn: the upstream must never see the FOREIGN scope's
				// session — it either mints a brand new one (any value other
				// than ownSession[foreignIdx]) or, in principle, could echo an
				// empty one; what it must NEVER do is see or echo back the
				// foreign scope's own session value.
				if !useOwn && got == ownSession[foreignIdx] {
					errCh <- fmt.Sprintf("scope %d round %d: upstream saw the FOREIGN scope's session %q, want it dropped before reaching the upstream", i, r, got)
				}
			}(r)
		}
	}

	wg.Wait()
	close(errCh)

	var failures int
	for msg := range errCh {
		failures++
		if failures <= 20 {
			t.Error(msg)
		}
	}
	if failures > 20 {
		t.Errorf("... and %d more failures", failures-20)
	}
}
