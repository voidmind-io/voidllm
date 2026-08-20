package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/config"
)

// This file exercises /v2/* routing: it must be a true alias of /v1/* on
// a.proxyApp only, with the same route-local auth middleware, the same
// upstream-path allowlist, and the same fail-closed boundary behaviour. All
// tests here use newTunnelTestApp (playground_tunnel_test.go), which calls
// the real setupRoutes. Tests built on a hand-constructed Fiber app instead
// of setupRoutes would only prove the test helper's own wiring, not
// production routing — that is why every test here goes through a real app.

// v2Upstream starts an httptest.Server that records the last request it
// received and always replies with a rerank-shaped JSON body.
func v2Upstream(t *testing.T) (*httptest.Server, *tunnelCapturedRequest) {
	t.Helper()
	captured := &tunnelCapturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Method = r.Method
		captured.Path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"x","results":[]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

// TestV2Routing_RerankReachesHandler: POST /v2/rerank with a valid key
// reaches Handle and is forwarded upstream as bare "rerank" — identical
// normalisation to /v1/rerank.
func TestV2Routing_RerankReachesHandler(t *testing.T) {
	t.Parallel()

	upstream, captured := v2Upstream(t)
	a := newTunnelTestApp(t, upstream.URL, 19100, 0, config.ProxyConfig{})
	rawKey := issueTunnelTestKey(t, a, auth.KeyInfo{ID: "key-v2-1", OrgID: "org-1", Role: "member"})

	req := httptest.NewRequest(http.MethodPost, "/v2/rerank",
		strings.NewReader(`{"model":"test-model","query":"q","documents":["a","b"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawKey)

	resp, err := a.proxyApp.Test(req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("proxyApp.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, http.StatusOK, body)
	}
	if captured.Path != "/rerank" {
		t.Errorf("upstream path = %q, want %q (v1 and v2 must normalise identically)", captured.Path, "/rerank")
	}
}

// TestV2Routing_NoKeyUnauthorized: POST /v2/rerank with no key must 401 and
// never reach the upstream. Asserted against a real setupRoutes app so
// route-local middleware omission would be caught — a globally-applied
// app.Use auth helper would NOT catch this class of bug, since it would
// authenticate the request regardless of which route matched.
func TestV2Routing_NoKeyUnauthorized(t *testing.T) {
	t.Parallel()

	upstream, captured := v2Upstream(t)
	a := newTunnelTestApp(t, upstream.URL, 19101, 0, config.ProxyConfig{})

	req := httptest.NewRequest(http.MethodPost, "/v2/rerank",
		strings.NewReader(`{"model":"test-model","query":"q","documents":["a"]}`))
	req.Header.Set("Content-Type", "application/json")
	// Deliberately no Authorization header.

	resp, err := a.proxyApp.Test(req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("proxyApp.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, http.StatusUnauthorized, body)
	}
	if captured.Path != "" {
		t.Fatalf("SECURITY: /v2/rerank reached the upstream without auth; upstream saw path %q", captured.Path)
	}
}

// TestV2Routing_DualPort_AbsentFromAdminApp: in dual-port mode /v2/* must be
// absent from the admin app, exactly mirroring /v1/*'s existing absence.
func TestV2Routing_DualPort_AbsentFromAdminApp(t *testing.T) {
	t.Parallel()

	upstream, captured := v2Upstream(t)
	a := newTunnelTestApp(t, upstream.URL, 19102, 19402, config.ProxyConfig{})
	rawKey := issueTunnelTestKey(t, a, auth.KeyInfo{ID: "key-v2-2", OrgID: "org-1", Role: "member"})

	req := httptest.NewRequest(http.MethodPost, "/v2/rerank",
		strings.NewReader(`{"model":"test-model","query":"q","documents":["a"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawKey)

	resp, err := a.adminApp.Test(req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("adminApp.Test: %v", err)
	}
	defer resp.Body.Close()

	// No POST route for /v2/* exists on the admin app — only the GET "/*" SPA
	// catch-all — so Fiber replies 405, and decisively the proxy handler was
	// never invoked.
	if resp.StatusCode != http.StatusMethodNotAllowed {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d (/v2/* must not be routed on the admin app), body = %s",
			resp.StatusCode, http.StatusMethodNotAllowed, body)
	}
	if captured.Path != "" {
		t.Fatalf("SECURITY: /v2/* reached the proxy handler on the admin app in dual-port mode; upstream saw path %q", captured.Path)
	}
}

// TestV2Routing_ModelsMirrorsV1: GET /v2/models must return the same model
// list as GET /v1/models, since GET /v2/models is registered as a dedicated
// route (mirroring ModelsHandler) BEFORE the /v2/* catch-all — without that
// ordering, an unmatched GET /v2/models would fall into the catch-all and be
// treated as a proxy request (400 "model field is required") instead of
// returning the model list.
func TestV2Routing_ModelsMirrorsV1(t *testing.T) {
	t.Parallel()

	upstream, _ := v2Upstream(t)
	a := newTunnelTestApp(t, upstream.URL, 19103, 0, config.ProxyConfig{})
	rawKey := issueTunnelTestKey(t, a, auth.KeyInfo{ID: "key-v2-3", OrgID: "org-1", Role: "member"})

	v1Req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	v1Req.Header.Set("Authorization", "Bearer "+rawKey)
	v1Resp, err := a.proxyApp.Test(v1Req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("proxyApp.Test (/v1/models): %v", err)
	}
	defer v1Resp.Body.Close()
	v1Body, err := io.ReadAll(v1Resp.Body)
	if err != nil {
		t.Fatalf("read /v1/models body: %v", err)
	}

	v2Req := httptest.NewRequest(http.MethodGet, "/v2/models", nil)
	v2Req.Header.Set("Authorization", "Bearer "+rawKey)
	v2Resp, err := a.proxyApp.Test(v2Req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("proxyApp.Test (/v2/models): %v", err)
	}
	defer v2Resp.Body.Close()
	v2Body, err := io.ReadAll(v2Resp.Body)
	if err != nil {
		t.Fatalf("read /v2/models body: %v", err)
	}

	if v1Resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d, want %d, body = %s", v1Resp.StatusCode, http.StatusOK, v1Body)
	}
	if v2Resp.StatusCode != v1Resp.StatusCode {
		t.Fatalf("GET /v2/models status = %d, want %d (same as /v1/models), body = %s", v2Resp.StatusCode, v1Resp.StatusCode, v2Body)
	}
	if string(v2Body) != string(v1Body) {
		t.Fatalf("GET /v2/models body = %s, want same as /v1/models body = %s", v2Body, v1Body)
	}
}

// TestV2Routing_Traversal_FailsClosed: POST /v2/../v1/rerank must never
// reach the upstream past the allowlist. It does NOT alias to /v1/rerank:
// Fiber v3's router matches on the raw, unnormalised request path
// (fasthttp's own path-normalisation, which does collapse "/foo/../"
// segments, is never consulted for route matching), so the literal string
// "/v2/../v1/rerank" matches the /v2/* route, not /v1/*. buildUpstreamRequest
// then strips the "/v2/" prefix and is left with "../v1/rerank", which
// path.Clean does not resolve (a leading ".." on a relative path is left
// as-is) and the allowlist rejects with 400. The request still fails closed
// and never reaches the upstream — just via the allowlist gate, not via any
// route-level canonicalisation to /v1/rerank.
func TestV2Routing_Traversal_FailsClosed(t *testing.T) {
	t.Parallel()

	upstream, captured := v2Upstream(t)
	a := newTunnelTestApp(t, upstream.URL, 19104, 0, config.ProxyConfig{})
	rawKey := issueTunnelTestKey(t, a, auth.KeyInfo{ID: "key-v2-4", OrgID: "org-1", Role: "member"})

	req := httptest.NewRequest(http.MethodPost, "/v2/../v1/rerank",
		strings.NewReader(`{"model":"test-model","query":"q","documents":["a"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawKey)

	resp, err := a.proxyApp.Test(req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("proxyApp.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d (must fail closed via the allowlist; it must not be rewritten to /v1/rerank and proxied), body = %s",
			resp.StatusCode, http.StatusBadRequest, body)
	}
	if captured.Path != "" {
		t.Fatalf("SECURITY: /v2/../v1/rerank reached the upstream; saw path %q", captured.Path)
	}
}

// TestV2Routing_BareV2FailsClosed: GET /v2 (no trailing slash) must never
// reach the upstream. Fiber's wildcard route "/v2/*" matches the bare "/v2"
// (empty wildcard segment), just as "/v1/*" already matches bare "/v1" today
// — this is pre-existing Fiber wildcard semantics, not something this change
// introduces. So GET /v2 DOES reach Handle, and fails there at model-field
// validation with 400 (no body was sent), not with a 404 from an unmatched
// route. It still fails closed — 400, never proxied — just via a different
// gate than a naive "unmatched route" read would predict.
func TestV2Routing_BareV2FailsClosed(t *testing.T) {
	t.Parallel()

	upstream, captured := v2Upstream(t)
	a := newTunnelTestApp(t, upstream.URL, 19105, 0, config.ProxyConfig{})
	rawKey := issueTunnelTestKey(t, a, auth.KeyInfo{ID: "key-v2-5", OrgID: "org-1", Role: "member"})

	req := httptest.NewRequest(http.MethodGet, "/v2", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)

	resp, err := a.proxyApp.Test(req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("proxyApp.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d (routed via wildcard match, then rejected downstream — not a routing 404), body = %s",
			resp.StatusCode, http.StatusBadRequest, body)
	}
	if captured.Path != "" {
		t.Fatalf("SECURITY: bare /v2 reached the upstream; saw path %q", captured.Path)
	}
}

// TestV2Routing_TrailingSlashFailsClosed: POST /v2/ cleans to a
// non-allowlisted upstream path ("." per path.Clean of "") and must be
// rejected by the allowlist with 400, not silently proxied.
func TestV2Routing_TrailingSlashFailsClosed(t *testing.T) {
	t.Parallel()

	upstream, captured := v2Upstream(t)
	a := newTunnelTestApp(t, upstream.URL, 19106, 0, config.ProxyConfig{})
	rawKey := issueTunnelTestKey(t, a, auth.KeyInfo{ID: "key-v2-6", OrgID: "org-1", Role: "member"})

	req := httptest.NewRequest(http.MethodPost, "/v2/",
		strings.NewReader(`{"model":"test-model"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawKey)

	resp, err := a.proxyApp.Test(req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("proxyApp.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, http.StatusBadRequest, body)
	}
	if captured.Path != "" {
		t.Fatalf("SECURITY: /v2/ reached the upstream; saw path %q", captured.Path)
	}
}

// TestV2Routing_UnknownPathRejectedNotFound: /v2/bogus is routed (reaches
// Handle, per the catch-all) but rejected by the allowlist with 400, not a
// 404 — proving the request reached the allowlist gate.
func TestV2Routing_UnknownPathRejectedNotFound(t *testing.T) {
	t.Parallel()

	upstream, captured := v2Upstream(t)
	a := newTunnelTestApp(t, upstream.URL, 19107, 0, config.ProxyConfig{})
	rawKey := issueTunnelTestKey(t, a, auth.KeyInfo{ID: "key-v2-7", OrgID: "org-1", Role: "member"})

	req := httptest.NewRequest(http.MethodPost, "/v2/bogus",
		strings.NewReader(`{"model":"test-model"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawKey)

	resp, err := a.proxyApp.Test(req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("proxyApp.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d (routed then allowlist-rejected, not a routing 404), body = %s",
			resp.StatusCode, http.StatusBadRequest, body)
	}
	if captured.Path != "" {
		t.Fatalf("SECURITY: /v2/bogus reached the upstream; saw path %q", captured.Path)
	}
}

// TestV2Routing_SinglePort_GetRerankUnauthorized: this is the one
// user-visible behaviour change introduced by mounting /v2/*. Before this
// change, single-port mode had no /v2/* route at all, so an unmatched GET
// request fell through to the SPA's own "Get("/*")" catch-all and got a 200
// with index.html — a request to a non-existent proxy endpoint looked like
// a successful page load. After this change, GET /v2/rerank matches the new
// /v2/* mount, which requires auth same as /v1/*, so a caller with no key
// now gets a 401 instead of a misleading 200. Pin the post-change behaviour
// so a future SPA or route-ordering change cannot silently resurrect the
// 200-with-HTML answer without a test noticing.
func TestV2Routing_SinglePort_GetRerankUnauthorized(t *testing.T) {
	t.Parallel()

	upstream, captured := v2Upstream(t)
	a := newTunnelTestApp(t, upstream.URL, 19108, 0, config.ProxyConfig{})
	if a.adminApp != nil {
		t.Fatalf("adminApp should be nil in single-port mode")
	}

	req := httptest.NewRequest(http.MethodGet, "/v2/rerank", nil)
	// Deliberately no Authorization header.

	resp, err := a.proxyApp.Test(req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("proxyApp.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d (GET /v2/rerank must no longer fall through to the SPA catch-all), body = %s",
			resp.StatusCode, http.StatusUnauthorized, body)
	}
	if captured.Path != "" {
		t.Fatalf("SECURITY: GET /v2/rerank reached the upstream; saw path %q", captured.Path)
	}
}
