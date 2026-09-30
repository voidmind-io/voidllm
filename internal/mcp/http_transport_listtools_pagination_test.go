package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// This file covers HTTPTransport.ListTools' nextCursor pagination: following
// every page an upstream hands back, aggregating tools across them in order,
// and forwarding params.cursor exactly as documented on ListTools itself.

// toolsListWireRequest decodes just enough of an outbound tools/list JSON-RPC
// request to inspect whether params (and, within it, a "cursor" key) were
// sent at all — Params is a *struct so its own nilness distinguishes "no
// params key on the wire" from "params present but empty", and Cursor is a
// **string, via a nested wrapper, so a present-but-null cursor decodes
// differently from an absent key. json.RawMessage is used for "_meta" since
// this file only needs to know whether the key is present, not its content.
type toolsListWireRequest struct {
	Method string `json:"method"`
	Params *struct {
		Cursor *string         `json:"cursor"`
		Meta   json.RawMessage `json:"_meta"`
	} `json:"params"`
}

// decodeToolsListRequest decodes body as a toolsListWireRequest, failing the
// test on any JSON error.
func decodeToolsListRequest(t *testing.T, body []byte) toolsListWireRequest {
	t.Helper()
	var req toolsListWireRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode outgoing tools/list request: %v\nbody: %s", err, body)
	}
	return req
}

// ---- Modern era: 3 pages, cursors "a", "" (empty, still a real cursor),
// then an absent nextCursor ends the fetch. --------------------------------

// TestListTools_Modern_ThreePages_AggregatedInOrder_CursorForwarded drives a
// three-page modern-era upstream and verifies: every page's tools are
// aggregated, in page order; the first request carries no "cursor" key at
// all (but still carries params._meta, since dialect2026Client.Prepare always
// adds it); and every later request carries params.cursor set to exactly the
// previous page's nextCursor — including when that cursor is the empty
// string, which MCP's pagination contract treats as a real, continuable
// cursor, not "no more pages" (only an absent/null nextCursor ends the
// fetch — see the adjacent TestListTools_NextCursorMissingOrNull_EndsFetch).
func TestListTools_Modern_ThreePages_AggregatedInOrder_CursorForwarded(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var requests []toolsListWireRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := decodeToolsListRequest(t, body)

		mu.Lock()
		requests = append(requests, req)
		n := len(requests)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"page0_tool","inputSchema":{"type":"object"}}],"nextCursor":"a"}}`)
		case 2:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"page1_tool","inputSchema":{"type":"object"}}],"nextCursor":""}}`)
		case 3:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"page2_tool","inputSchema":{"type":"object"}}]}}`)
		default:
			t.Errorf("unexpected request #%d", n)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}

	wantNames := []string{"page0_tool", "page1_tool", "page2_tool"}
	if len(listing.Tools) != len(wantNames) {
		t.Fatalf("len(Tools) = %d, want %d", len(listing.Tools), len(wantNames))
	}
	for i, want := range wantNames {
		if listing.Tools[i].Name != want {
			t.Errorf("Tools[%d].Name = %q, want %q (aggregation must preserve page order)", i, listing.Tools[i].Name, want)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("upstream received %d requests, want 3", len(requests))
	}

	// Request 1: no cursor key at all, but _meta is still present (modern
	// era always adds it via dialect2026Client.Prepare).
	if requests[0].Params == nil {
		t.Fatal("request 1 params = nil, want a params object carrying _meta")
	}
	if requests[0].Params.Cursor != nil {
		t.Errorf("request 1 params.cursor = %v, want absent (no cursor to send yet)", *requests[0].Params.Cursor)
	}
	if len(requests[0].Params.Meta) == 0 {
		t.Error("request 1 params._meta missing, want it present on every modern-era request")
	}

	// Request 2: params.cursor must equal page 1's nextCursor ("a") exactly.
	if requests[1].Params == nil || requests[1].Params.Cursor == nil {
		t.Fatal("request 2 params.cursor missing, want \"a\"")
	}
	if *requests[1].Params.Cursor != "a" {
		t.Errorf("request 2 params.cursor = %q, want %q", *requests[1].Params.Cursor, "a")
	}
	if len(requests[1].Params.Meta) == 0 {
		t.Error("request 2 params._meta missing")
	}

	// Request 3: params.cursor must equal page 2's nextCursor (the empty
	// string) exactly — not absent, not skipped.
	if requests[2].Params == nil || requests[2].Params.Cursor == nil {
		t.Fatal("request 3 params.cursor missing, want the empty string")
	}
	if *requests[2].Params.Cursor != "" {
		t.Errorf("request 3 params.cursor = %q, want the empty string (a real, continuable cursor)", *requests[2].Params.Cursor)
	}
	if len(requests[2].Params.Meta) == 0 {
		t.Error("request 3 params._meta missing")
	}
}

// TestListTools_Legacy_ThreePages_AggregatedInOrder_CursorForwarded is the
// legacy-era counterpart: the same three-page/empty-cursor shape, but with
// the initialize/notifications/initialized handshake ListTools performs
// automatically under the legacy era, and no params._meta anywhere —
// legacyClientDialect.Prepare forwards ListTools' own request bytes
// unmodified (see ListTools' own doc).
func TestListTools_Legacy_ThreePages_AggregatedInOrder_CursorForwarded(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var toolsListRequests []toolsListWireRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		w.Header().Set("Content-Type", "application/json")
		switch rpc.Method {
		case "initialize":
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			req := decodeToolsListRequest(t, body)
			mu.Lock()
			toolsListRequests = append(toolsListRequests, req)
			n := len(toolsListRequests)
			mu.Unlock()

			switch n {
			case 1:
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"page0_tool","inputSchema":{"type":"object"}}],"nextCursor":"a"}}`)
			case 2:
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"page1_tool","inputSchema":{"type":"object"}}],"nextCursor":""}}`)
			case 3:
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"page2_tool","inputSchema":{"type":"object"}}]}}`)
			default:
				t.Errorf("unexpected tools/list request #%d", n)
				w.WriteHeader(http.StatusInternalServerError)
			}
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}

	wantNames := []string{"page0_tool", "page1_tool", "page2_tool"}
	if len(listing.Tools) != len(wantNames) {
		t.Fatalf("len(Tools) = %d, want %d", len(listing.Tools), len(wantNames))
	}
	for i, want := range wantNames {
		if listing.Tools[i].Name != want {
			t.Errorf("Tools[%d].Name = %q, want %q", i, listing.Tools[i].Name, want)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(toolsListRequests) != 3 {
		t.Fatalf("upstream received %d tools/list requests, want 3", len(toolsListRequests))
	}

	if toolsListRequests[0].Params != nil {
		t.Errorf("request 1 params = %+v, want nil (legacy era, no cursor to send, no _meta ever)", toolsListRequests[0].Params)
	}
	if toolsListRequests[1].Params == nil || toolsListRequests[1].Params.Cursor == nil || *toolsListRequests[1].Params.Cursor != "a" {
		t.Errorf("request 2 params = %+v, want cursor \"a\"", toolsListRequests[1].Params)
	}
	if toolsListRequests[1].Params != nil && len(toolsListRequests[1].Params.Meta) != 0 {
		t.Error("request 2 params._meta present, want absent under the legacy era")
	}
	if toolsListRequests[2].Params == nil || toolsListRequests[2].Params.Cursor == nil || *toolsListRequests[2].Params.Cursor != "" {
		t.Errorf("request 3 params = %+v, want cursor \"\" (the empty string)", toolsListRequests[2].Params)
	}
}

// ---- Missing/null nextCursor both end the fetch ----------------------------

// TestListTools_NextCursorMissingOrNull_EndsFetch verifies MCP's pagination
// contract: nextCursor entirely ABSENT from the result object and an
// explicit "nextCursor": null are both treated as "no more pages" —
// identically — since ListTools decodes NextCursor as *string and only ever
// continues when that pointer is non-nil.
func TestListTools_NextCursorMissingOrNull_EndsFetch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		response string
	}{
		{
			name:     "nextCursor key entirely absent",
			response: `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"only_tool","inputSchema":{"type":"object"}}]}}`,
		},
		{
			name:     "nextCursor explicitly null",
			response: `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"only_tool","inputSchema":{"type":"object"}}],"nextCursor":null}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits int
			var mu sync.Mutex

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				hits++
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.response)
			}))
			t.Cleanup(srv.Close)

			tr := newModernTransport(srv.URL, "none", "", "")
			listing, err := tr.ListTools(context.Background())
			if err != nil {
				t.Fatalf("ListTools() error = %v, want nil", err)
			}
			if len(listing.Tools) != 1 || listing.Tools[0].Name != "only_tool" {
				t.Errorf("Tools = %+v, want exactly [only_tool]", listing.Tools)
			}

			mu.Lock()
			defer mu.Unlock()
			if hits != 1 {
				t.Errorf("upstream received %d requests, want exactly 1 (missing/null nextCursor must end the fetch)", hits)
			}
		})
	}
}

// ---- x-mcp-header filtering runs once, on the aggregate -------------------

// TestListTools_XMCPHeaderTools_AcrossPages_FilteredOnceOnAggregate verifies
// ListTools' own doc: FilterHeaderParamTools runs exactly once, on the fully
// aggregated and deduplicated tool list, never per page. It drives a
// two-page fetch where page 1 carries a tool with a VALID x-mcp-header
// annotation and page 2 carries both a plain tool and a tool whose
// annotation VIOLATES MCP 2026-07-28 §4.3 (an annotation nested under
// oneOf) — a violation FilterHeaderParamTools can only detect by inspecting
// the actual schema, proving the aggregate (not each page in isolation) is
// what gets filtered. The valid tool's binding must appear in HeaderParams
// exactly once, and the violating tool must be excluded from Tools
// entirely, with the plain tool passed through unchanged.
func TestListTools_XMCPHeaderTools_AcrossPages_FilteredOnceOnAggregate(t *testing.T) {
	t.Parallel()

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[`+
				`{"name":"lookup_region","inputSchema":{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}}`+
				`],"nextCursor":"next"}}`)
		case 2:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[`+
				`{"name":"plain_tool","inputSchema":{"type":"object","properties":{"q":{"type":"string"}}}},`+
				`{"name":"broken_tool","inputSchema":{"type":"object","oneOf":[{"type":"string","x-mcp-header":"Foo"}]}}`+
				`]}}`)
		default:
			t.Errorf("unexpected request #%d", n)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}

	byName := make(map[string]struct{}, len(listing.Tools))
	for _, tl := range listing.Tools {
		byName[tl.Name] = struct{}{}
	}
	if _, ok := byName["broken_tool"]; ok {
		t.Error("Tools contains broken_tool, want it excluded (violates §4.3)")
	}
	if _, ok := byName["lookup_region"]; !ok {
		t.Error("Tools is missing lookup_region")
	}
	if _, ok := byName["plain_tool"]; !ok {
		t.Error("Tools is missing plain_tool")
	}
	if len(listing.Tools) != 2 {
		t.Errorf("len(Tools) = %d, want 2 (lookup_region and plain_tool only)", len(listing.Tools))
	}

	if len(listing.HeaderParams) != 1 {
		t.Fatalf("len(HeaderParams) = %d, want exactly 1 (lookup_region's binding, counted once, not once per page)", len(listing.HeaderParams))
	}
	if _, ok := listing.HeaderParams["lookup_region"]; !ok {
		t.Errorf("HeaderParams = %+v, want a \"lookup_region\" entry", listing.HeaderParams)
	}
	if _, ok := listing.HeaderParams["broken_tool"]; ok {
		t.Error("HeaderParams contains an entry for the excluded broken_tool")
	}
}
