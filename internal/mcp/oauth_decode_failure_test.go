package mcp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers the oauth.go half of docs/mcp-v2.md review round Fund 4:
// discoverTokenURL and fetchToken used to return the raw encoding/json
// decode error directly (or wrapped via %w) when an authorization server's
// response failed to parse. encoding/json's SyntaxError and
// UnmarshalTypeError messages can quote a fragment of the offending input
// (e.g. "invalid character '<' looking for beginning of value"), which would
// put response content from a server VoidLLM does not control into the
// returned error. Both now return a fixed error class instead.

// TestOAuthTokenManager_FetchToken_DecodeFailure_NeverLeaksUpstreamBytes
// drives a fake token endpoint that returns syntactically invalid JSON with
// an embedded, conspicuous sentinel, and asserts the sentinel never appears
// in GetToken's returned error text.
func TestOAuthTokenManager_FetchToken_DecodeFailure_NeverLeaksUpstreamBytes(t *testing.T) {
	t.Parallel()

	const sentinel = "SENTINEL-TOKEN-RESPONSE-3d8f1a-do-not-leak-me"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Malformed JSON (unterminated object) with the sentinel embedded
		// right at the syntax error.
		_, _ = w.Write([]byte(`{"access_token":"` + sentinel + `"`))
	}))
	t.Cleanup(srv.Close)

	mgr := mcp.NewOAuthTokenManager(nil)
	_, err := mgr.GetToken(context.Background(), "decode-failure-server", oauthTestConfig(srv.URL))
	if err == nil {
		t.Fatal("GetToken() error = nil, want a decode error for malformed JSON")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("GetToken() error leaks the token response body: %v", err)
	}
}

// TestOAuthTokenManager_DiscoverTokenURL_DecodeFailure_NeverLeaksUpstreamBytes
// is the discoverTokenURL half of the same fix: a malformed RFC 8414
// discovery response must not leak its bytes into GetToken's returned error
// either.
func TestOAuthTokenManager_DiscoverTokenURL_DecodeFailure_NeverLeaksUpstreamBytes(t *testing.T) {
	t.Parallel()

	const sentinel = "SENTINEL-DISCOVERY-RESPONSE-6c4e2b-do-not-leak-me"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/.well-known/oauth-authorization-server") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token_endpoint":"` + sentinel + `"`)) // truncated, invalid JSON
	}))
	t.Cleanup(srv.Close)

	mgr := mcp.NewOAuthTokenManager(nil)
	cfg := mcp.OAuthConfig{ServerURL: srv.URL, ClientID: "test-client", ClientSecret: "test-secret"}
	_, err := mgr.GetToken(context.Background(), "discovery-decode-failure-server", cfg)
	if err == nil {
		t.Fatal("GetToken() error = nil, want a discovery decode error for malformed JSON")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("GetToken() error leaks the discovery response body: %v", err)
	}
}
