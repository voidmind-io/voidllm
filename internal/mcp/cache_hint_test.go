package mcp_test

import (
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// TestParseCacheHint covers every branch of parseCacheHint's interpretation
// of a resultType:"complete" response's ttlMs/cacheScope fields per MCP
// 2026-07-28 §5 (docs/mcp-v2.md §5). parseCacheHint is called only for that
// one response shape. §5 makes ttlMs mandatory and defines a missing value
// as 0 ("Fehlt es, gilt 0") — but only WITHIN an already-present
// CacheableResult hint: a response where cacheScope (or ttlMs itself) is
// present establishes that the hint is being offered, and it is only in
// that case that an absent or malformed ttlMs normalizes to a "set" 0
// rather than being treated as no hint at all. Each such case below isolates
// one rule: an ordinary integer, negative clamping (still "set", normalized
// to 0), a ttlMs absent while cacheScope is present (still "set", normalized
// to 0 per §5's own default within a present hint), and the two
// malformed-shape cases (a JSON string, and a non-integer number) that must
// ALSO normalize to a "set" 0 rather than no hint — cacheScope is present in
// all of these, which is what establishes the hint at all. cacheScope
// itself is covered alongside: only the two recognized literal values are
// honored, anything else — including a plausible-sounding but unrecognized
// "shared" — leaves Scope empty, without affecting TTLMsSet.
//
// The "both fields absent" case is the one REVERSED by this test, in the
// opposite direction from the three "normalizes to a set 0" cases above: an
// earlier revision of this fix set TTLMsSet unconditionally for every
// resultType:"complete" response, treating a response with NEITHER ttlMs
// NOR cacheScope the same as one that offers a hint with a missing ttlMs.
// That collapsed two different upstream behaviors into one: a modern server
// that implements CacheableResult but omits ttlMs (§5's own "defaults to
// 0" case, correctly a "set" 0) and a modern server that does not implement
// CacheableResult at all — nearly every server as of this revision's
// release — which was then wrongly floored to minToolFetchInterval (one
// second) instead of falling back to the operator-configured TTL, turning
// GetTools' per-request call pattern in Code Mode into up to one upstream
// tools/list fetch per second per such server. This case now asserts the
// corrected behavior: TTLMsSet false, so resolveTTL falls back to its own
// configured maxAge exactly as it does for a legacy response.
func TestParseCacheHint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		ttlMsRaw      []byte
		cacheScopeRaw []byte
		wantTTLMs     int64
		wantTTLMsSet  bool
		wantScope     string
	}{
		{
			name:          "ordinary ttlMs plus cacheScope public",
			ttlMsRaw:      []byte(`5000`),
			cacheScopeRaw: []byte(`"public"`),
			wantTTLMs:     5000,
			wantTTLMsSet:  true,
			wantScope:     mcp.CacheScopePublic,
		},
		{
			name:         "negative ttlMs is clamped to 0 but remains \"set\"",
			ttlMsRaw:     []byte(`-1`),
			wantTTLMs:    0,
			wantTTLMsSet: true,
		},
		{
			name:          "ttlMs absent but cacheScope present: the hint IS offered, so ttlMs normalizes to a set 0 (MCP 2026-07-28 §5: \"Fehlt es, gilt 0\")",
			ttlMsRaw:      nil,
			cacheScopeRaw: []byte(`"public"`),
			wantTTLMs:     0,
			wantTTLMsSet:  true,
			wantScope:     mcp.CacheScopePublic,
		},
		{
			name:          "ttlMs as a JSON string is not a bare integer: normalizes to a set 0 (cacheScope present establishes the hint)",
			ttlMsRaw:      []byte(`"5000"`),
			cacheScopeRaw: []byte(`"public"`),
			wantTTLMs:     0,
			wantTTLMsSet:  true,
			wantScope:     mcp.CacheScopePublic,
		},
		{
			name:          "ttlMs as a non-integer number is not a bare integer: normalizes to a set 0 (cacheScope present establishes the hint)",
			ttlMsRaw:      []byte(`5000.5`),
			cacheScopeRaw: []byte(`"public"`),
			wantTTLMs:     0,
			wantTTLMsSet:  true,
			wantScope:     mcp.CacheScopePublic,
		},
		{
			name:          "cacheScope \"shared\" is not a recognized value: Scope left empty",
			cacheScopeRaw: []byte(`"shared"`),
			wantTTLMs:     0,
			wantTTLMsSet:  true,
			wantScope:     "",
		},
		{
			name:          "cacheScope private is honored",
			cacheScopeRaw: []byte(`"private"`),
			wantTTLMs:     0,
			wantTTLMsSet:  true,
			wantScope:     mcp.CacheScopePrivate,
		},
		{
			// REVERSED: this case previously asserted wantTTLMsSet: true,
			// on the theory that parseCacheHint is only ever called for a
			// resultType:"complete" response, which "always carries a hint
			// per §5". That conflated §5's default for a MISSING ttlMs
			// WITHIN a present hint with the absence of the hint itself — a
			// modern server that implements CacheableResult at all sets at
			// least cacheScope, or ttlMs, on the wire; a server that sends
			// NEITHER is not offering a hint, it is a server that has not
			// implemented CacheableResult, which describes nearly every MCP
			// server as of this revision's release. See parseCacheHint's own
			// doc and this test's own doc above for the full history.
			name:         "both fields absent means no hint was offered at all: TTLMsSet false, not a \"set\" 0 (REVERSED from the prior assertion)",
			wantTTLMs:    0,
			wantTTLMsSet: false,
			wantScope:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := mcp.ParseCacheHint(tc.ttlMsRaw, tc.cacheScopeRaw)
			if got.TTLMs != tc.wantTTLMs {
				t.Errorf("TTLMs = %d, want %d", got.TTLMs, tc.wantTTLMs)
			}
			if got.TTLMsSet != tc.wantTTLMsSet {
				t.Errorf("TTLMsSet = %v, want %v", got.TTLMsSet, tc.wantTTLMsSet)
			}
			if got.Scope != tc.wantScope {
				t.Errorf("Scope = %q, want %q", got.Scope, tc.wantScope)
			}
		})
	}
}
