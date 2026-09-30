package mcp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers the fail-closed contract for authType "oauth" when the
// transport was built without a usable oauth manager and config — the shape
// MCPTransportCache.LoadAll produces when it could not decrypt a server's
// stored OAuth client secret (e.g. after an encryption key rotation) but
// still constructs the transport rather than dropping the server entirely.
// Before errOAuthNotConfigured existed, rawPost and Forward silently sent the
// request with no Authorization header at all in this situation — the
// opposite of what an operator configuring OAuth auth expects. Both call
// sites must now fail before ever reaching the upstream. See
// errOAuthNotConfigured's own doc in http_transport.go.

// oauthMisconfiguredServerID is deliberately distinctive so the "no
// config/identifier content in the error" assertions below are actually
// checking something, not vacuously true because the value happens to look
// like ordinary text.
const oauthMisconfiguredServerID = "server-id-must-never-leak-into-error-9f3c1b2a"

// newUnconfiguredOAuthTransport builds an HTTPTransport with authType
// "oauth" but a nil oauth manager and nil oauth config — exactly the shape
// MCPTransportCache.LoadAll produces on a decrypt failure — pinned to the
// modern era so resolveBinding never itself makes a network call: Call skips
// stateFor/ensureWarm entirely for EraModern (see Call's doc), and Forward
// never warms up on any era, so in both cases the very first HTTP attempt
// this transport would ever make is the one the oauth check must block.
func newUnconfiguredOAuthTransport(endpoint string) *mcp.HTTPTransport {
	return mcp.NewHTTPTransport(endpoint, "oauth", "", "", 5*time.Second, true,
		oauthMisconfiguredServerID, nil, nil,
		mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20260728, testStreamIdleTimeout)
}

// TestHTTPTransport_OAuthNotConfigured_FailsClosed_NeverContactsUpstream is
// the regression test for both call sites (rawPost, reached via Call, and
// Forward's own duplicate auth switch) failing closed instead of relaying a
// request with no Authorization header. The upstream hit counter — not just
// the returned error — is the property under test: a transport that returns
// errOAuthNotConfigured AFTER already sending the request would pass an
// error-only assertion just as well, but would still have leaked the
// unauthenticated request to the upstream.
func TestHTTPTransport_OAuthNotConfigured_FailsClosed_NeverContactsUpstream(t *testing.T) {
	t.Parallel()

	tests := []struct {
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

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)) //nolint:errcheck // test server
			}))
			t.Cleanup(srv.Close)

			tr := newUnconfiguredOAuthTransport(srv.URL)

			err := tc.call(t, tr)
			if err == nil {
				t.Fatal("error = nil, want errOAuthNotConfigured for an oauth transport missing its manager/config")
			}
			if !errors.Is(err, mcp.ErrOAuthNotConfigured) {
				t.Errorf("errors.Is(err, ErrOAuthNotConfigured) = false, err = %v, want true", err)
			}

			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Errorf("upstream received %d request(s), want 0 — a missing oauth manager/config must fail "+
					"BEFORE any request is sent, never after sending one with no Authorization header", got)
			}

			msg := err.Error()
			if strings.Contains(msg, srv.URL) {
				t.Errorf("error message %q contains the upstream URL, want none", msg)
			}
			if strings.Contains(msg, oauthMisconfiguredServerID) {
				t.Errorf("error message %q contains the server identifier, want none", msg)
			}
		})
	}
}

// TestHTTPTransport_OAuthNotConfigured_OnlyGatesOAuthAuthType is the
// counter-proof for the fail-closed check above: it must be specific to
// authType "oauth" and must not affect any other auth type, or a transport
// with a merely nil oauthManager/oauthConfig (the zero value for every
// non-oauth transport, see NewHTTPTransport's doc) would be broken outright.
// Both the buffered and streaming paths must still reach the upstream
// normally under "bearer".
func TestHTTPTransport_OAuthNotConfigured_OnlyGatesOAuthAuthType(t *testing.T) {
	t.Parallel()

	tests := []struct {
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

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)) //nolint:errcheck // test server
			}))
			t.Cleanup(srv.Close)

			// authType "bearer" with a real, non-empty token — the ordinary,
			// correctly configured case, built with the same nil
			// oauthMgr/oauthCfg every non-oauth transport carries.
			tr := mcp.NewHTTPTransport(srv.URL, "bearer", "", "a-real-bearer-token", 5*time.Second, true,
				"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20260728, testStreamIdleTimeout)

			if err := tc.call(t, tr); err != nil {
				t.Fatalf("%s: error = %v, want nil — the oauth fail-closed check must never trigger for authType "+
					"\"bearer\"", tc.name, err)
			}

			if got := atomic.LoadInt32(&hits); got != 1 {
				t.Errorf("upstream received %d request(s), want exactly 1 — a non-oauth transport must reach the "+
					"upstream normally", got)
			}
		})
	}
}
