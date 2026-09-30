package mcp_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers the sibling of docs/mcp-v2.md Fund 7 inside oauth.go:
// OAuthTokenManager.fetchToken's own response-body read is bounded by
// oauthResponseMaxBytes using the identical "+1 byte" LimitReader trick
// rawPost uses for rawPostMaxBodyBytes, for the identical reason (see
// oauthResponseMaxBytes' own doc) — a response exceeding the limit must be
// rejected outright, not silently truncated and handed to json.Unmarshal as
// if it were the complete document.

// oauthTestConfig returns an mcp.OAuthConfig pointed at tokenURL with
// otherwise-arbitrary Client Credentials Flow values — fetchToken never
// validates ClientID/ClientSecret against anything on this path, they are
// only form-encoded into the outbound request.
func oauthTestConfig(tokenURL string) mcp.OAuthConfig {
	return mcp.OAuthConfig{
		TokenURL:     tokenURL,
		ClientID:     "test-client",
		ClientSecret: "test-secret",
	}
}

// TestOAuthTokenManager_FetchToken_BodyOverLimit_Rejected verifies that a
// token-endpoint response body one byte over oauthResponseMaxBytes is
// rejected with an error, never silently truncated and parsed as if it were
// the complete document.
func TestOAuthTokenManager_FetchToken_BodyOverLimit_Rejected(t *testing.T) {
	t.Parallel()

	oversized := bytes.Repeat([]byte(" "), int(mcp.OAuthResponseMaxBytes)+1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(oversized)
	}))
	t.Cleanup(srv.Close)

	mgr := mcp.NewOAuthTokenManager(nil)
	_, err := mgr.GetToken(context.Background(), "over-limit-server", oauthTestConfig(srv.URL))
	if err == nil {
		t.Fatal("GetToken() error = nil, want an error for a token response exceeding oauthResponseMaxBytes " +
			"(it must be rejected, not silently truncated)")
	}
}

// TestOAuthTokenManager_FetchToken_BodyExactlyAtLimit_Accepted is the
// boundary counterpart: a response body of EXACTLY oauthResponseMaxBytes —
// not one byte over — must still be accepted and parsed successfully. The
// body is real, valid JSON padded with whitespace so its exact byte length
// can be controlled precisely while remaining parseable.
func TestOAuthTokenManager_FetchToken_BodyExactlyAtLimit_Accepted(t *testing.T) {
	t.Parallel()

	const accessToken = "exactly-at-limit-token"
	suffix := `","expires_in":3600}`
	prefix := fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","padding":"`, accessToken)
	// Padding fills the gap between prefix+suffix and the exact byte limit
	// with an innocuous JSON string value, so the whole document remains
	// valid JSON at precisely mcp.OAuthResponseMaxBytes bytes.
	fixedLen := len(prefix) + len(suffix)
	if int64(fixedLen) > mcp.OAuthResponseMaxBytes {
		t.Fatalf("test setup: fixed JSON scaffolding (%d bytes) already exceeds OAuthResponseMaxBytes (%d)", fixedLen, mcp.OAuthResponseMaxBytes)
	}
	padding := bytes.Repeat([]byte("a"), int(mcp.OAuthResponseMaxBytes)-fixedLen)
	body := prefix + string(padding) + suffix
	if int64(len(body)) != mcp.OAuthResponseMaxBytes {
		t.Fatalf("test setup: body length = %d, want exactly %d", len(body), mcp.OAuthResponseMaxBytes)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	mgr := mcp.NewOAuthTokenManager(nil)
	token, err := mgr.GetToken(context.Background(), "exact-limit-server", oauthTestConfig(srv.URL))
	if err != nil {
		t.Fatalf("GetToken() error = %v, want nil for a token response of exactly OAuthResponseMaxBytes", err)
	}
	if token != accessToken {
		t.Errorf("GetToken() = %q, want %q", token, accessToken)
	}
}

// TestOAuthTokenManager_FetchToken_BodyOverLimit_NotAConnectionError is a
// small sanity check distinguishing the byte-limit rejection from an
// unrelated transport failure — it must not be reported via a sentinel that
// could be confused with, say, context cancellation.
func TestOAuthTokenManager_FetchToken_BodyOverLimit_NotAConnectionError(t *testing.T) {
	t.Parallel()

	oversized := bytes.Repeat([]byte(" "), int(mcp.OAuthResponseMaxBytes)+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(oversized)
	}))
	t.Cleanup(srv.Close)

	mgr := mcp.NewOAuthTokenManager(nil)
	_, err := mgr.GetToken(context.Background(), "over-limit-server-2", oauthTestConfig(srv.URL))
	if err == nil {
		t.Fatal("GetToken() error = nil, want an error")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("GetToken() error = %v, want a byte-limit rejection, not a context error", err)
	}
}
