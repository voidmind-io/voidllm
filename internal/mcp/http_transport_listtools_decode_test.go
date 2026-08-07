package mcp_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
