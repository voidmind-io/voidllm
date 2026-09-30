package mcp_test

import (
	"testing"

	"github.com/voidmind-io/voidllm/internal/jsonx"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file differentially tests countToolsListPageTools (http_transport.go)
// against the exact decode shape ListTools itself uses (Result.Tools
// []Tool, decoded via jsonx.Unmarshal) across a table of adversarial and
// ordinary tools/list response bodies. For every body, the invariant this
// whole pre-decode walk exists to guarantee is: the counter's verdict must
// never UNDER-report how many tools the real decode would end up holding in
// target.Result.Tools — either the counter agrees (or conservatively
// over-counts), or it reports exceeded, or it fails closed with an error.
// Any of those three outcomes is safe; a counter that reports fewer tools
// than the real decode actually produces, without erroring or flagging
// exceeded, is exactly the fail-open class of bug this package's own
// countToolsListPageTools doc now documents at length.
//
// realToolsCount reads len(target.Result.Tools) regardless of whether
// jsonx.Unmarshal itself returned an error: this repo's own jsonx.Unmarshal
// (sonic's ConfigStd) has been observed to still partially populate its
// target before reporting an error for some malformed shapes (e.g. trailing
// garbage after the top-level value) — see countToolsListPageTools' own doc
// for why trailing bytes are safe to ignore. Using whatever ended up in the
// target either way, rather than only when Unmarshal succeeded cleanly,
// makes this table's invariant check strictly MORE conservative (a stronger
// test), and uniform across every row without needing per-case special
// handling for which adversarial shapes cause the real decode to error.

// toolsListRPCShape mirrors the anonymous struct ListTools itself decodes a
// tools/list page's body into (http_transport.go), field for field and tag
// for tag, so this file's differential comparison exercises the identical
// decode behavior ListTools relies on.
type toolsListRPCShape struct {
	Result struct {
		Tools      []mcp.Tool `json:"tools"`
		NextCursor *string    `json:"nextCursor"`
	} `json:"result"`
	Error *struct {
		Code int `json:"code"`
	} `json:"error"`
}

// realToolsCount runs the SAME decode ListTools itself performs against
// body and returns how many tools ended up in Result.Tools — see this
// file's own package doc for why this is read regardless of whether
// Unmarshal returned an error.
func realToolsCount(body []byte) int {
	var target toolsListRPCShape
	_ = jsonx.Unmarshal(body, &target) // error deliberately ignored — see package doc.
	return len(target.Result.Tools)
}

// differentialCase is one row of the adversarial-body table below.
type differentialCase struct {
	name string
	body []byte
}

// TestCountToolsListPageTools_Differential_NeverUndercountsRealDecode drives
// countToolsListPageTools against every adversarial body in the table below
// and asserts, for each, that it never silently under-reports how many
// tools the real decode (realToolsCount) would actually see — see this
// file's own package doc for the exact three-way safe-outcome invariant.
// limit is generous (far above every row's own small element count) so
// "exceeded" only ever fires as a genuine safety net, never as an artifact
// of an undersized limit masking a real disagreement.
func TestCountToolsListPageTools_Differential_NeverUndercountsRealDecode(t *testing.T) {
	t.Parallel()

	const limit = 1_000

	cases := []differentialCase{
		{
			name: "duplicate result keys, last wins",
			body: []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"a"}]},"result":{"tools":[{"name":"b"},{"name":"c"}]}}`),
		},
		{
			name: "duplicate tools keys, summed conservatively",
			body: []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"a"}],"tools":[{"name":"b"},{"name":"c"}]}}`),
		},
		{
			name: "tools case variants (tools/Tools/TOOLS)",
			body: []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"a"}],"Tools":[{"name":"b"},{"name":"c"}]}}`),
		},
		{
			name: "result case variant (Result)",
			body: []byte(`{"jsonrpc":"2.0","id":1,"Result":{"tools":[{"name":"a"},{"name":"b"}]}}`),
		},
		{
			name: "escaped tools key",
			body: []byte(`{"jsonrpc":"2.0","id":1,"result":{"` + jsonUnicodeEscape('t') + `ools":[{"name":"a"},{"name":"b"}]}}`),
		},
		{
			name: "escaped result key",
			body: []byte(`{"jsonrpc":"2.0","id":1,"` + jsonUnicodeEscape('r') + `esult":{"tools":[{"name":"a"}]}}`),
		},
		{
			name: "non-object result (array)",
			body: []byte(`{"jsonrpc":"2.0","id":1,"result":[1,2,3]}`),
		},
		{
			name: "non-object result (scalar string)",
			body: []byte(`{"jsonrpc":"2.0","id":1,"result":"not an object"}`),
		},
		{
			name: "non-array tools (object)",
			body: []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":{"foo":"bar"}}}`),
		},
		{
			name: "non-array tools (scalar)",
			body: []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":42}}`),
		},
		{
			name: "scalar array elements",
			body: []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[1,2,3]}}`),
		},
		{
			name: "trailing garbage after top-level object",
			body: []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"a"}]}}   trailing-garbage-here`),
		},
		{
			name: "leading BOM",
			body: append([]byte("\xEF\xBB\xBF"), []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"a"},{"name":"b"}]}}`)...),
		},
		{
			name: "moderate nesting before result (does not trip the depth guard)",
			body: []byte(`{"jsonrpc":"2.0","id":1,"junk":` + deepArrayLiteral(5) + `,"result":{"tools":[{"name":"a"},{"name":"b"}]}}`),
		},
		{
			name: "moderate nesting inside result before tools",
			body: []byte(`{"jsonrpc":"2.0","id":1,"result":{"junk":` + deepArrayLiteral(5) + `,"tools":[{"name":"a"},{"name":"b"}]}}`),
		},
		{
			name: "error and result both present",
			body: []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom"},"result":{"tools":[{"name":"a"}]}}`),
		},
		{
			name: "empty tools array",
			body: []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
		},
		{
			name: "no result key at all",
			body: []byte(`{"jsonrpc":"2.0","id":1,"other":{"tools":[{"name":"a"}]}}`),
		},
		{
			name: "null result",
			body: []byte(`{"jsonrpc":"2.0","id":1,"result":null}`),
		},
		{
			name: "null tools",
			body: []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":null}}`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			real := realToolsCount(tc.body)
			count, exceeded, err := mcp.CountToolsListPageTools(tc.body, limit)

			safe := err != nil || exceeded || count >= real
			if !safe {
				t.Errorf("UNSAFE: CountToolsListPageTools(...) = (count=%d, exceeded=%v, err=%v), real decode produced %d tools — the counter under-reported without erroring or flagging exceeded",
					count, exceeded, err, real)
			}
			t.Logf("count=%d exceeded=%v err=%v real=%d", count, exceeded, err, real)
		})
	}
}

// TestCountToolsListPageTools_Differential_OrdinaryBodiesMatchExactly is the
// stricter counterpart for well-formed, unambiguous bodies (no duplicate or
// case-variant keys, no adversarial shapes): here the counter's count must
// match the real decode's tools count EXACTLY, not merely be >=, proving the
// conservative-overcount cases exercised above are specific to genuinely
// ambiguous input, not a general imprecision in ordinary operation.
func TestCountToolsListPageTools_Differential_OrdinaryBodiesMatchExactly(t *testing.T) {
	t.Parallel()

	const limit = 1_000

	cases := []differentialCase{
		{
			name: "single tool",
			body: []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"a","inputSchema":{"type":"object"}}]}}`),
		},
		{
			name: "several tools",
			body: []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"a"},{"name":"b"},{"name":"c"}],"nextCursor":"next"}}`),
		},
		{
			name: "empty tools array",
			body: []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`),
		},
		{
			name: "no result key at all",
			body: []byte(`{"jsonrpc":"2.0","id":1,"other":"value"}`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			real := realToolsCount(tc.body)
			count, exceeded, err := mcp.CountToolsListPageTools(tc.body, limit)
			if err != nil {
				t.Fatalf("CountToolsListPageTools(...) error = %v, want nil for an unambiguous, well-formed body", err)
			}
			if exceeded {
				t.Fatalf("CountToolsListPageTools(...) exceeded = true, want false for a body well under limit=%d", limit)
			}
			if count != real {
				t.Errorf("CountToolsListPageTools(...) count = %d, want exactly %d (the real decode's own count) for an unambiguous body", count, real)
			}
		})
	}
}
