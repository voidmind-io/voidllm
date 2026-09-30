package mcp_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// This file covers docs/mcp-v2.md review round Fund 4: HTTPTransport.ListTools
// used to wrap its tools/list decode failure with fmt.Errorf("...: %w", err),
// embedding internal/jsonx's own decode error — which, on a syntax error,
// quotes a window of the SOURCE bytes around the failure position. An
// upstream returning deliberately malformed JSON with embedded content could
// therefore put that content into whatever log line a caller built from
// err.Error() (e.g. internal/app/code_mode.go's toolErr.Error() logging).
// ListTools now returns a fixed error class instead — mirroring the fix
// ToolHeaderParams' own decode-error handling already had.

// TestHTTPTransport_ListTools_DecodeFailure_NeverLeaksUpstreamBytes drives a
// fake upstream that returns syntactically invalid JSON with an embedded,
// conspicuous sentinel string as its tools/list response, and asserts the
// sentinel never appears anywhere in ListTools' returned error text.
func TestHTTPTransport_ListTools_DecodeFailure_NeverLeaksUpstreamBytes(t *testing.T) {
	t.Parallel()

	const sentinel = "SENTINEL-UPSTREAM-BYTES-7f2a9c-do-not-leak-me"
	// Deliberately malformed JSON (an unterminated object) with the sentinel
	// embedded right at the syntax error, exactly the shape a JSON decoder's
	// own "unexpected token near ..." message would otherwise quote.
	malformed := `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"` + sentinel + `"` // truncated, invalid JSON

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, malformed)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want a decode error for malformed JSON")
	}
	if listing != nil {
		t.Errorf("ListTools() listing = %v, want nil on decode failure", listing)
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("ListTools() error leaks upstream response bytes: %v", err)
	}
}

// TestHTTPTransport_ListTools_DecodeFailure_OnSecondPage_ReturnsError_NoPartialList
// is the mid-pagination counterpart of the test above: the FIRST page
// succeeds and carries a nextCursor, but the SECOND page's response fails to
// decode as JSON-RPC. ListTools must fail the whole fetch — discarding the
// first page's already-read tool — rather than returning a partial listing
// built from page 1 alone, and the decode error must stay upstream-byte-free
// on this path exactly as it does on the first page.
func TestHTTPTransport_ListTools_DecodeFailure_OnSecondPage_ReturnsError_NoPartialList(t *testing.T) {
	t.Parallel()

	const sentinel = "SENTINEL-UPSTREAM-BYTES-PAGE2-do-not-leak-me"
	malformed := `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"` + sentinel + `"` // truncated, invalid JSON

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"page1_tool","inputSchema":{"type":"object"}}],"nextCursor":"next"}}`)
		case 2:
			fmt.Fprint(w, malformed)
		default:
			t.Errorf("unexpected request #%d", n)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want a decode error for a malformed second page")
	}
	if listing != nil {
		t.Errorf("ListTools() listing = %v, want nil — no partial listing built from page 1 alone", listing)
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("ListTools() error leaks upstream response bytes: %v", err)
	}
}

// TestHTTPTransport_ListTools_JSONRPCError_OnSecondPage_ReturnsError_NoPartialList
// is the JSON-RPC-level counterpart: the first page succeeds, but the second
// page answers with a well-formed JSON-RPC error object instead of a result.
// ListTools must fail the whole fetch, and the error must carry only the
// numeric JSON-RPC code — never the upstream-controlled, free-form error
// message.
func TestHTTPTransport_ListTools_JSONRPCError_OnSecondPage_ReturnsError_NoPartialList(t *testing.T) {
	t.Parallel()

	const sentinel = "SENTINEL-UPSTREAM-ERROR-MESSAGE-do-not-leak-me"

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"page1_tool","inputSchema":{"type":"object"}}],"nextCursor":"next"}}`)
		case 2:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":%q}}`, sentinel)
		default:
			t.Errorf("unexpected request #%d", n)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want an error for a JSON-RPC error on the second page")
	}
	if listing != nil {
		t.Errorf("ListTools() listing = %v, want nil — no partial listing built from page 1 alone", listing)
	}
	if !strings.Contains(err.Error(), "code -32000") {
		t.Errorf("error = %q, want it to include the numeric JSON-RPC code", err.Error())
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("error = %q, leaks the upstream's own free-form error message", err.Error())
	}
}
