package admin_test

import (
	"testing"

	"github.com/voidmind-io/voidllm/internal/api/admin"
)

// TestAcceptHeaderParsing_TableDriven is the single table-driven test for the
// Accept-header parsing logic shared by acceptsSSE (admin.AcceptsSSE) and
// acceptsOnlySSE (admin.AcceptsOnlySSE) — both built on the same
// parseAcceptEntry (mcp_handler.go). It pins down the full contract RFC 9110
// requires and MCP Streamable HTTP §4.1 depends on:
//
//   - media-type comparison is case-insensitive (RFC 9110 §8.3.1)
//   - a "q" parameter of exactly 0 means "not acceptable" (RFC 9110 §12.4.2),
//     and a caller listing "text/event-stream, application/json;q=0" is
//     therefore SSE-ONLY, not "accepts both" — it explicitly declined JSON
//   - any non-zero q, however small, still counts as "acceptable"
//   - "q" is recognized regardless of its position among a media type's
//     other parameters (RFC 9110 §12.4.2's own example puts a non-q
//     parameter first)
//   - a missing "q" parameter defaults to 1 (RFC 9110 §12.4.2)
//   - whitespace around "=" and ";" and the parameter NAME's own casing
//     ("Q" as well as "q") do not affect parsing
//
// Every case checks BOTH functions, since they share exactly this parsing
// logic but answer different questions from it (acceptsSSE: "would text/
// event-stream be delivered at all"; acceptsOnlySSE: "is this caller a
// legacy client that never adopted the modern requirement to accept both
// media types").
func TestAcceptHeaderParsing_TableDriven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		accept      string
		wantSSE     bool
		wantOnlySSE bool
	}{
		// ---- text/event-stream in every casing is recognized -------------
		{
			name:    "lowercase text/event-stream",
			accept:  "text/event-stream",
			wantSSE: true, wantOnlySSE: true,
		},
		{
			name:    "fully upper-cased TEXT/EVENT-STREAM",
			accept:  "TEXT/EVENT-STREAM",
			wantSSE: true, wantOnlySSE: true,
		},
		{
			name:    "title-cased Text/Event-Stream",
			accept:  "Text/Event-Stream",
			wantSSE: true, wantOnlySSE: true,
		},
		{
			name:    "arbitrarily mixed-case tExT/eVeNt-StReAm",
			accept:  "tExT/eVeNt-StReAm",
			wantSSE: true, wantOnlySSE: true,
		},

		// ---- application/json;q=0 means "not accepted" --------------------
		{
			// The central regression case (docs/mcp-v2.md review finding A3):
			// a client sending this is EXPLICITLY declining JSON, so it must
			// be classified the same as a client that never mentioned JSON
			// at all — SSE-only, not "accepts both".
			name:    "SSE plus explicitly zero-weighted JSON is classified SSE-ONLY",
			accept:  "text/event-stream, application/json;q=0",
			wantSSE: true, wantOnlySSE: true,
		},
		{
			name:    "zero-weighted JSON first, SSE second — order does not matter",
			accept:  "application/json;q=0, text/event-stream",
			wantSSE: true, wantOnlySSE: true,
		},
		{
			name:    "q=0 written as 0.0 also means not accepted",
			accept:  "text/event-stream, application/json;q=0.0",
			wantSSE: true, wantOnlySSE: true,
		},
		{
			// A wildcard that would otherwise cover application/json is
			// ALSO explicitly declined via q=0 — must not count either.
			name:    "SSE plus a zero-weighted wildcard is still classified SSE-ONLY",
			accept:  "text/event-stream, */*;q=0",
			wantSSE: true, wantOnlySSE: true,
		},

		// ---- q=0.5 and other non-zero values count as accepted ------------
		{
			name:    "SSE itself weighted 0.5 is still accepted (non-zero)",
			accept:  "text/event-stream;q=0.5",
			wantSSE: true, wantOnlySSE: true,
		},
		{
			name:    "SSE weighted a tiny non-zero value is still accepted",
			accept:  "text/event-stream;q=0.001",
			wantSSE: true, wantOnlySSE: true,
		},
		{
			// JSON accepted at q=0.5 (non-zero) means the caller DOES accept
			// JSON too — the modern, spec-compliant shape — so it is no
			// longer SSE-only, even though SSE itself is still accepted.
			name:    "SSE plus JSON weighted 0.5 is NOT classified SSE-only (JSON is genuinely accepted)",
			accept:  "text/event-stream, application/json;q=0.5",
			wantSSE: true, wantOnlySSE: false,
		},

		// ---- q at an arbitrary parameter position --------------------------
		{
			name:    "q after another parameter (charset first)",
			accept:  "text/event-stream;charset=utf-8;q=0.8",
			wantSSE: true, wantOnlySSE: true,
		},
		{
			name:    "q before another parameter",
			accept:  "text/event-stream;q=0.8;charset=utf-8",
			wantSSE: true, wantOnlySSE: true,
		},
		{
			name:    "q sandwiched between two unrelated parameters",
			accept:  "text/event-stream;level=1;q=0.9;charset=utf-8",
			wantSSE: true, wantOnlySSE: true,
		},

		// ---- missing q means 1 ---------------------------------------------
		{
			name:    "missing q defaults to 1 (equivalent to an explicit q=1)",
			accept:  "text/event-stream",
			wantSSE: true, wantOnlySSE: true,
		},
		{
			name:    "explicit q=1 behaves identically to a missing q",
			accept:  "text/event-stream;q=1",
			wantSSE: true, wantOnlySSE: true,
		},
		{
			name:    "explicit q=1.0 also behaves identically to a missing q",
			accept:  "text/event-stream;q=1.0",
			wantSSE: true, wantOnlySSE: true,
		},

		// ---- whitespace and case in parameter names ------------------------
		{
			name:    "whitespace around ';' and '=' in the q parameter",
			accept:  "text/event-stream ; q = 0.9",
			wantSSE: true, wantOnlySSE: true,
		},
		{
			name:    "uppercase Q parameter name",
			accept:  "text/event-stream;Q=0.9",
			wantSSE: true, wantOnlySSE: true,
		},
		{
			name:    "mixed-case Q with generous surrounding whitespace",
			accept:  "text/event-stream ;  Q  =  0.5  ",
			wantSSE: true, wantOnlySSE: true,
		},
		{
			name:    "uppercase Q=0 still means not accepted",
			accept:  "text/event-stream;Q=0",
			wantSSE: false, wantOnlySSE: false,
		},
		{
			name:    "leading/trailing whitespace around the whole comma-joined list",
			accept:  "  text/event-stream  ,  application/json  ",
			wantSSE: true, wantOnlySSE: false,
		},

		// ---- baseline / edge cases -----------------------------------------
		{
			name:    "application/json alone: no SSE at all",
			accept:  "application/json",
			wantSSE: false, wantOnlySSE: false,
		},
		{
			name:    "both media types, the modern conformant shape",
			accept:  "application/json, text/event-stream",
			wantSSE: true, wantOnlySSE: false,
		},
		{
			name:    "a bare wildcard covers JSON too, so not SSE-only",
			accept:  "text/event-stream, */*",
			wantSSE: true, wantOnlySSE: false,
		},
		{
			name:    "application/* wildcard also covers JSON, so not SSE-only",
			accept:  "text/event-stream, application/*",
			wantSSE: true, wantOnlySSE: false,
		},
		{
			name:    "empty Accept header: neither SSE nor SSE-only",
			accept:  "",
			wantSSE: false, wantOnlySSE: false,
		},
		{
			name:    "q=0 on the only SSE entry: not accepted at all, and therefore not SSE-only either",
			accept:  "text/event-stream;q=0",
			wantSSE: false, wantOnlySSE: false,
		},
		{
			name:    "q=0 on SSE with JSON present at full weight: neither SSE nor SSE-only",
			accept:  "text/event-stream;q=0, application/json",
			wantSSE: false, wantOnlySSE: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := admin.AcceptsSSE(tc.accept); got != tc.wantSSE {
				t.Errorf("AcceptsSSE(%q) = %v, want %v", tc.accept, got, tc.wantSSE)
			}
			if got := admin.AcceptsOnlySSE(tc.accept); got != tc.wantOnlySSE {
				t.Errorf("AcceptsOnlySSE(%q) = %v, want %v", tc.accept, got, tc.wantOnlySSE)
			}
		})
	}
}
