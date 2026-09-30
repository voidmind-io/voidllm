package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers server.go's tools/list cursor rejection: VoidLLM's own
// built-in Server never paginates (handleToolsList's own doc), so any
// params.cursor at all — regardless of value — is invalid input, rejected
// with CodeInvalidParams before handleToolsList ever runs, in both eras.

// ---- Every cursor shape is rejected, in both eras ---------------------------

// TestServer_ToolsList_CursorParam_RejectedBothEras drives params.cursor
// carrying an empty string, a non-empty string, an explicit JSON null, and a
// non-string value, and verifies every one of them is rejected with
// CodeInvalidParams under both the legacy and modern eras — hasCursorParam's
// own doc is explicit that ANY cursor key at all is invalid here, regardless
// of what it holds.
func TestServer_ToolsList_CursorParam_RejectedBothEras(t *testing.T) {
	t.Parallel()

	cursorCases := []struct {
		name       string
		legacyBody string
		modernBody string
	}{
		{
			name:       "empty string cursor",
			legacyBody: `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":""}}`,
			modernBody: modernRequestBody(1, "tools/list", map[string]any{"cursor": ""}, nil),
		},
		{
			name:       "non-empty string cursor",
			legacyBody: `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":"some-opaque-cursor"}}`,
			modernBody: modernRequestBody(1, "tools/list", map[string]any{"cursor": "some-opaque-cursor"}, nil),
		},
		{
			name:       "explicit null cursor",
			legacyBody: `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":null}}`,
			modernBody: modernRequestBody(1, "tools/list", map[string]any{"cursor": nil}, nil),
		},
		{
			name:       "non-string (numeric) cursor",
			legacyBody: `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":5}}`,
			modernBody: modernRequestBody(1, "tools/list", map[string]any{"cursor": 5}, nil),
		},
	}

	for _, tc := range cursorCases {
		t.Run(tc.name+"/legacy", func(t *testing.T) {
			t.Parallel()
			s := newTestServer("voidllm", "0.1.0")
			resp := callRawHdr(t, s, context.Background(), tc.legacyBody, mcp.MapHeader{})
			assertErrorCode(t, resp, mcp.CodeInvalidParams)
		})
		t.Run(tc.name+"/modern", func(t *testing.T) {
			t.Parallel()
			s := newTestServer("voidllm", "0.1.0")
			resp := callRawHdr(t, s, context.Background(), tc.modernBody, modernHeader())
			assertErrorCode(t, resp, mcp.CodeInvalidParams)
		})
	}
}

// ---- Modern era: the cursor rejection carries HTTP 400, legacy stays 200 ---

// TestServer_ToolsList_CursorParam_Modern_HTTP400_Legacy_HTTP200 verifies
// dispatch's explicit statusHintFor override for this one violation: under
// the modern era a cursor on tools/list is reported via HintBadRequest (HTTP
// 400 for an HTTP-aware caller), while under the legacy era the exact same
// CodeInvalidParams stays the ordinary HintOK (HTTP 200) — CodeInvalidParams
// alone does not determine the hint (see hintForError's own doc); only this
// specific dispatch-level override forces HintBadRequest for the modern era.
func TestServer_ToolsList_CursorParam_Modern_HTTP400_Legacy_HTTP200(t *testing.T) {
	t.Parallel()

	t.Run("modern era: HintBadRequest", func(t *testing.T) {
		t.Parallel()
		s := newTestServer("voidllm", "0.1.0")
		body := modernRequestBody(1, "tools/list", map[string]any{"cursor": "x"}, nil)
		result := s.Handle(context.Background(), []byte(body), modernHeader())

		if result.Hint != mcp.HintBadRequest {
			t.Errorf("Hint = %v, want HintBadRequest", result.Hint)
		}
		var resp mcp.Response
		if err := json.Unmarshal(result.Body, &resp); err != nil {
			t.Fatalf("unmarshal response: %v\nbody: %s", err, result.Body)
		}
		if resp.Error == nil || resp.Error.Code != mcp.CodeInvalidParams {
			t.Fatalf("Error = %+v, want CodeInvalidParams", resp.Error)
		}
	})

	t.Run("legacy era: HintOK", func(t *testing.T) {
		t.Parallel()
		s := newTestServer("voidllm", "0.1.0")
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":"x"}}`
		result := s.Handle(context.Background(), []byte(body), mcp.MapHeader{})

		if result.Hint != mcp.HintOK {
			t.Errorf("Hint = %v, want HintOK (CodeInvalidParams is an ordinary protocol error under the legacy era)", result.Hint)
		}
		var resp mcp.Response
		if err := json.Unmarshal(result.Body, &resp); err != nil {
			t.Fatalf("unmarshal response: %v\nbody: %s", err, result.Body)
		}
		if resp.Error == nil || resp.Error.Code != mcp.CodeInvalidParams {
			t.Fatalf("Error = %+v, want CodeInvalidParams", resp.Error)
		}
	})
}

// ---- No params / params without a cursor: unaffected -----------------------

// TestServer_ToolsList_NoCursor_Unaffected verifies tools/list with no params
// at all, and with params present but carrying no "cursor" key, both still
// succeed normally in both eras — the guard is specifically keyed on the
// presence of the "cursor" key, not on params being present at all.
func TestServer_ToolsList_NoCursor_Unaffected(t *testing.T) {
	t.Parallel()

	registerOneTool := func(s *mcp.Server) {
		s.RegisterTool(mcp.Tool{Name: "t1", InputSchema: mcp.ObjectSchema(nil)},
			func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
				return mcp.TextResult("ok"), nil
			})
	}

	t.Run("legacy, no params field at all", func(t *testing.T) {
		t.Parallel()
		s := newTestServer("voidllm", "0.1.0")
		registerOneTool(s)
		resp := callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		assertNoError(t, resp)
		m := resultMap(t, resp)
		tools, _ := m["tools"].([]any)
		if len(tools) != 1 {
			t.Errorf("tools = %v, want 1 tool", tools)
		}
	})

	t.Run("legacy, params present without cursor", func(t *testing.T) {
		t.Parallel()
		s := newTestServer("voidllm", "0.1.0")
		registerOneTool(s)
		resp := callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
		assertNoError(t, resp)
	})

	t.Run("modern, params carries only _meta (no cursor)", func(t *testing.T) {
		t.Parallel()
		s := newTestServer("voidllm", "0.1.0")
		registerOneTool(s)
		body := modernRequestBody(1, "tools/list", nil, nil)
		resp := callRawHdr(t, s, context.Background(), body, modernHeader())
		assertNoError(t, resp)
		m := resultMap(t, resp)
		tools, _ := m["tools"].([]any)
		if len(tools) != 1 {
			t.Errorf("tools = %v, want 1 tool", tools)
		}
	})
}

// ---- The response never carries a nextCursor -------------------------------

// TestServer_ToolsList_ResponseNeverContainsNextCursor verifies
// handleToolsList's own documented property directly: the built-in server
// never paginates, so its tools/list result never carries a "nextCursor" key
// at all (missing, not merely null) — checked in both eras, and with more
// than one tool registered so an implementation that silently truncated a
// long list to a "first page" would have something to truncate.
func TestServer_ToolsList_ResponseNeverContainsNextCursor(t *testing.T) {
	t.Parallel()

	registerTools := func(s *mcp.Server) {
		for _, name := range []string{"a_tool", "b_tool", "c_tool"} {
			s.RegisterTool(mcp.Tool{Name: name, InputSchema: mcp.ObjectSchema(nil)},
				func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
					return mcp.TextResult("ok"), nil
				})
		}
	}

	t.Run("legacy", func(t *testing.T) {
		t.Parallel()
		s := newTestServer("voidllm", "0.1.0")
		registerTools(s)
		resp := callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		assertNoError(t, resp)
		m := resultMap(t, resp)
		if _, ok := m["nextCursor"]; ok {
			t.Errorf("result = %v, must not contain \"nextCursor\" — this server never paginates", m)
		}
	})

	t.Run("modern", func(t *testing.T) {
		t.Parallel()
		s := newTestServer("voidllm", "0.1.0")
		registerTools(s)
		body := modernRequestBody(1, "tools/list", nil, nil)
		resp := callRawHdr(t, s, context.Background(), body, modernHeader())
		assertNoError(t, resp)
		m := resultMap(t, resp)
		if _, ok := m["nextCursor"]; ok {
			t.Errorf("result = %v, must not contain \"nextCursor\" — this server never paginates", m)
		}
	})
}
