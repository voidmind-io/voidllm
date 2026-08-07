package admin_test

import (
	"testing"

	"github.com/voidmind-io/voidllm/internal/api/admin"
)

// TestAcceptsSSE is the direct, table-driven unit test for the built-in MCP
// server's Accept-header parser (mcp_handler.go's acceptsSSE, exposed here as
// admin.AcceptsSSE). This is the gap left once the proxy path
// (HandleMCPProxy) stopped having any Accept-based branch of its own — see
// export_test.go's AcceptsSSE doc — so the built-in path's own parsing logic,
// including the comma-separated and quality-parameter cases a real modern
// client sends (docs/mcp-v2.md §4.1: Accept MUST list both application/json
// and text/event-stream), needs coverage that does not depend on going
// through a full HTTP round trip for every case.
func TestAcceptsSSE(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		accept string
		want   bool
	}{
		{
			name:   "exact match",
			accept: "text/event-stream",
			want:   true,
		},
		{
			name:   "modern client sends both, JSON first (docs/mcp-v2.md §4.1)",
			accept: "application/json, text/event-stream",
			want:   true,
		},
		{
			name:   "modern client sends both, event-stream first",
			accept: "text/event-stream, application/json",
			want:   true,
		},
		{
			name:   "quality parameter on the matching media type",
			accept: "text/event-stream;q=0.9",
			want:   true,
		},
		{
			name:   "quality parameter with a space before it",
			accept: "text/event-stream ;q=0.9",
			want:   true,
		},
		{
			name:   "leading/trailing whitespace around a comma-joined list",
			accept: "  application/json ,  text/event-stream  ",
			want:   true,
		},
		{
			name:   "application/json alone does not match",
			accept: "application/json",
			want:   false,
		},
		{
			name:   "wildcard does not match — only the exact media type counts",
			accept: "*/*",
			want:   false,
		},
		{
			name:   "empty Accept header does not match",
			accept: "",
			want:   false,
		},
		{
			name:   "unrelated multi-value list without text/event-stream",
			accept: "text/html, application/xhtml+xml, application/xml;q=0.9, */*;q=0.8",
			want:   false,
		},
		{
			// This was the bug (docs/mcp-v2.md review finding A3): RFC 9110
			// §8.3.1 is explicit that "the type and subtype tokens are
			// case-insensitive", so a media type comparison here must ignore
			// case just like every other part of Accept-header matching.
			// Before the fix this case asserted the opposite (want: false),
			// which meant a perfectly conformant client that happened to
			// title-case its own Accept header (some HTTP libraries do) was
			// silently treated as not wanting SSE at all.
			name:   "REGRESSION: differently-cased media type IS case-insensitively matched (RFC 9110 §8.3.1)",
			accept: "Text/Event-Stream",
			want:   true,
		},
		{
			name:   "fully upper-cased media type also matches",
			accept: "TEXT/EVENT-STREAM",
			want:   true,
		},
		{
			name:   "mixed casing mid-token still matches",
			accept: "tExT/eVeNt-StReAm",
			want:   true,
		},
		{
			name:   "differently-cased media type buried among other accepted types still matches",
			accept: "application/json, Text/Event-Stream, */*",
			want:   true,
		},
		{
			// RFC 9110 §12.4.2: "a weight of 0 means 'not acceptable'". A
			// client sending this is EXPLICITLY declining SSE, so it must not
			// be treated the same as a bare, unweighted entry.
			name:   "REGRESSION: an explicit q=0 on text/event-stream means NOT acceptable (RFC 9110 §12.4.2)",
			accept: "text/event-stream;q=0",
			want:   false,
		},
		{
			name:   "q=0 written as 0.0 also means not acceptable",
			accept: "text/event-stream;q=0.0",
			want:   false,
		},
		{
			name:   "q=0 on the ONLY entry, with other unrelated accepted types present, still means not acceptable",
			accept: "application/json, text/event-stream;q=0, text/html",
			want:   false,
		},
		{
			name:   "a non-zero q on text/event-stream still counts as acceptable",
			accept: "text/event-stream;q=0.001",
			want:   true,
		},
		{
			name:   "text/event-stream buried among several other accepted types",
			accept: "application/json, text/html, text/event-stream, */*",
			want:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := admin.AcceptsSSE(tc.accept); got != tc.want {
				t.Errorf("AcceptsSSE(%q) = %v, want %v", tc.accept, got, tc.want)
			}
		})
	}
}
