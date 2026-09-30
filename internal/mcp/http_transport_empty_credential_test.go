package mcp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers the fail-closed contract for authType "bearer"/"header"
// when the credential that would actually be sent is empty —
// errEmptyAuthCredential in http_transport.go. Before this sentinel existed,
// only "header" checked that authHeader (the header NAME) was non-empty,
// never that authToken (the header VALUE) was — so a transport built with an
// empty authToken still sent "Authorization: Bearer " (or an equally empty
// configured header) instead of failing the call outright. This mirrors
// http_transport_oauth_test.go's structure for the sibling
// errOAuthNotConfigured check: both call sites (rawPost, reached via Call,
// and Forward's own duplicate auth switch) must fail BEFORE the upstream
// ever sees the request.

// newEmptyCredentialTransport builds an HTTPTransport with the given
// authType/authHeader/authToken combination, pinned to the modern era so
// resolveBinding never itself makes a network call: Call skips
// stateFor/ensureWarm entirely for EraModern, and Forward never warms up on
// any era, so in both cases the very first HTTP attempt this transport would
// ever make is the one the empty-credential check must block.
func newEmptyCredentialTransport(endpoint, authType, authHeader, authToken string) *mcp.HTTPTransport {
	return mcp.NewHTTPTransport(endpoint, authType, authHeader, authToken, 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20260728, testStreamIdleTimeout)
}

// TestHTTPTransport_EmptyAuthCredential_FailsClosed_NeverContactsUpstream is
// the regression test for both call sites (rawPost, reached via Call, and
// Forward's own duplicate auth switch) failing closed instead of relaying a
// request with an empty credential-carrying header. The upstream hit
// counter — not just the returned error — is the property under test: a
// transport that returns errEmptyAuthCredential AFTER already sending the
// request would pass an error-only assertion just as well, but would still
// have leaked the unauthenticated request to the upstream.
func TestHTTPTransport_EmptyAuthCredential_FailsClosed_NeverContactsUpstream(t *testing.T) {
	t.Parallel()

	authCases := []struct {
		name       string
		authType   string
		authHeader string
		authToken  string
	}{
		{name: "bearer with empty token", authType: "bearer", authHeader: "", authToken: ""},
		{name: "header with header name set and empty token value", authType: "header", authHeader: "X-API-Key", authToken: ""},
	}

	pathCases := []struct {
		name string
		call func(t *testing.T, tr *mcp.HTTPTransport) error
	}{
		{
			name: "Call (buffered path, via rawPost)",
			call: func(t *testing.T, tr *mcp.HTTPTransport) error {
				t.Helper()
				_, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
				return err
			},
		},
		{
			name: "Forward (streaming path)",
			call: func(t *testing.T, tr *mcp.HTTPTransport) error {
				t.Helper()
				res, err := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), mcp.MapHeader{})
				if res != nil {
					res.Body.Close()
				}
				return err
			},
		},
	}

	for _, ac := range authCases {
		for _, pc := range pathCases {
			t.Run(ac.name+"/"+pc.name, func(t *testing.T) {
				t.Parallel()

				var hits int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					atomic.AddInt32(&hits, 1)
					w.Header().Set("Content-Type", "application/json")
					w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)) //nolint:errcheck // test server
				}))
				t.Cleanup(srv.Close)

				tr := newEmptyCredentialTransport(srv.URL, ac.authType, ac.authHeader, ac.authToken)

				err := pc.call(t, tr)
				if err == nil {
					t.Fatal("error = nil, want errEmptyAuthCredential for a transport whose credential value is empty")
				}
				if !errors.Is(err, mcp.ErrEmptyAuthCredential) {
					t.Errorf("errors.Is(err, ErrEmptyAuthCredential) = false, err = %v, want true", err)
				}

				if got := atomic.LoadInt32(&hits); got != 0 {
					t.Errorf("upstream received %d request(s), want 0 — an empty credential must fail BEFORE any "+
						"request is sent, never after sending one carrying an empty credential header", got)
				}
			})
		}
	}
}

// TestHTTPTransport_EmptyAuthCredential_OnlyGatesEmptyCredential is the
// counter-proof for the fail-closed check above: it must be specific to an
// EMPTY credential value and must not affect a transport configured with a
// real, non-empty one — both for authType "bearer" and "header" — or a
// correctly configured server would be broken outright. Both the buffered
// and streaming paths must reach the upstream normally, and the credential
// value/header name actually sent must match what was configured.
func TestHTTPTransport_EmptyAuthCredential_OnlyGatesEmptyCredential(t *testing.T) {
	t.Parallel()

	authCases := []struct {
		name       string
		authType   string
		authHeader string
		authToken  string
	}{
		{name: "bearer with a real token", authType: "bearer", authHeader: "", authToken: "a-real-bearer-token"},
		{name: "header with a real header name and value", authType: "header", authHeader: "X-API-Key", authToken: "a-real-api-key"},
	}

	pathCases := []struct {
		name string
		call func(t *testing.T, tr *mcp.HTTPTransport) error
	}{
		{
			name: "Call (buffered path)",
			call: func(t *testing.T, tr *mcp.HTTPTransport) error {
				t.Helper()
				_, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
				return err
			},
		},
		{
			name: "Forward (streaming path)",
			call: func(t *testing.T, tr *mcp.HTTPTransport) error {
				t.Helper()
				res, err := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), mcp.MapHeader{})
				if res != nil {
					res.Body.Close()
				}
				return err
			},
		},
	}

	for _, ac := range authCases {
		for _, pc := range pathCases {
			t.Run(ac.name+"/"+pc.name, func(t *testing.T) {
				t.Parallel()

				var hits int32
				var gotAuthorization, gotCustomHeader string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					atomic.AddInt32(&hits, 1)
					gotAuthorization = r.Header.Get("Authorization")
					if ac.authHeader != "" {
						gotCustomHeader = r.Header.Get(ac.authHeader)
					}
					w.Header().Set("Content-Type", "application/json")
					w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)) //nolint:errcheck // test server
				}))
				t.Cleanup(srv.Close)

				tr := newEmptyCredentialTransport(srv.URL, ac.authType, ac.authHeader, ac.authToken)

				if err := pc.call(t, tr); err != nil {
					t.Fatalf("%s: error = %v, want nil — the empty-credential check must never trigger for a "+
						"non-empty credential", pc.name, err)
				}

				if got := atomic.LoadInt32(&hits); got != 1 {
					t.Fatalf("upstream received %d request(s), want exactly 1 — a correctly configured transport "+
						"must reach the upstream normally", got)
				}

				switch ac.authType {
				case "bearer":
					want := "Bearer " + ac.authToken
					if gotAuthorization != want {
						t.Errorf("Authorization header = %q, want %q", gotAuthorization, want)
					}
				case "header":
					if gotCustomHeader != ac.authToken {
						t.Errorf("%s header = %q, want %q", ac.authHeader, gotCustomHeader, ac.authToken)
					}
				}
			})
		}
	}
}

// TestHTTPTransport_EmptyAuthCredential_NoneAuthType_NeverGated is a small,
// deliberate sanity check that authType "none" — which carries no credential
// at all by design — is never affected by this check: it must reach the
// upstream normally with neither Authorization nor any custom header set.
func TestHTTPTransport_EmptyAuthCredential_NoneAuthType_NeverGated(t *testing.T) {
	t.Parallel()

	var hits int32
	var gotAuthorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)

	tr := newEmptyCredentialTransport(srv.URL, "none", "", "")
	_, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err != nil {
		t.Fatalf("Call() error = %v, want nil for authType \"none\"", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream received %d request(s), want exactly 1", got)
	}
	if gotAuthorization != "" {
		t.Errorf("Authorization header = %q, want empty for authType \"none\"", gotAuthorization)
	}
}
