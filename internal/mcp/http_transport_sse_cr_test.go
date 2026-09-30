package mcp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file adds the CR-only line-ending regression case for
// extractSSEResult (http_transport.go) that motivated splitSSEBody's
// extraction into a shared helper with sseEventReader (sse_reader.go, see
// splitSSEBody's own doc): extractSSEResult's line-ending normalization
// tolerates a bare "\r" exactly as it does "\r\n" and "\n", per the WHATWG
// "Server-Sent Events" interpretation algorithm this package already relies
// on (extractSSEResult's own doc). This exercises that normalization through
// the BUFFERED path (ListTools -> Call -> rawPost -> extractSSEResult),
// mirroring http_transport_sse_buffered_test.go's existing coverage of the
// same call path for other framing shapes, but has never itself been
// exercised with bare-CR line endings.

// newCRModernTransport builds a modern-era-pinned HTTPTransport, identical in
// shape to newSSEModernTransport (http_transport_sse_buffered_test.go) and
// newModernTransport (http_transport_test.go) — both unexported to their own
// files — so this file defines its own copy rather than depending on either.
func newCRModernTransport(endpoint string) *mcp.HTTPTransport {
	return mcp.NewHTTPTransport(endpoint, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20260728, testStreamIdleTimeout)
}

// TestListTools_SSEUpstream_BareCRLineEndings_ResultStillExtracted is the
// CR-only regression case: an upstream whose SSE stream uses a bare "\r" as
// its line terminator throughout — no "\n" anywhere in the response body at
// all — must still be parsed correctly, exactly as if it had used "\n" or
// "\r\n".
func TestListTools_SSEUpstream_BareCRLineEndings_ResultStillExtracted(t *testing.T) {
	t.Parallel()

	const toolsListResult = `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"Search the web","inputSchema":{"type":"object"}}]}}`

	// Built with bare "\r" as the ONLY line terminator: "event: message",
	// "data: <result>", and the blank dispatching line are all separated by
	// "\r" rather than "\n" or "\r\n".
	body := "event: message\r" + "data: " + toolsListResult + "\r\r"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	tr := newCRModernTransport(srv.URL)
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil (bare-CR line endings must be tolerated exactly like LF/CRLF)", err)
	}
	if len(listing.Tools) != 1 || listing.Tools[0].Name != "search" {
		t.Errorf("ListTools() tools = %+v, want [{search ...}]", listing.Tools)
	}
}
