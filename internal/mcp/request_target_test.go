package mcp_test

import (
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// TestTargetParamKey verifies the exact per-method mapping MCP Streamable
// HTTP §4.2 (docs/mcp-v2.md §4.2) requires between a modern-era method and
// the params field its Mcp-Name header mirrors: tools/call and prompts/get
// both mirror params.name, resources/read mirrors params.uri instead, and
// every other method carries no Mcp-Name header at all.
func TestTargetParamKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		method  string
		wantKey string
		wantOK  bool
	}{
		{method: "tools/call", wantKey: "name", wantOK: true},
		{method: "prompts/get", wantKey: "name", wantOK: true},
		{method: "resources/read", wantKey: "uri", wantOK: true},
		{method: "tools/list", wantKey: "", wantOK: false},
		{method: "resources/list", wantKey: "", wantOK: false},
		{method: "prompts/list", wantKey: "", wantOK: false},
		{method: "server/discover", wantKey: "", wantOK: false},
		{method: "subscriptions/listen", wantKey: "", wantOK: false},
		{method: "initialize", wantKey: "", wantOK: false},
		{method: "ping", wantKey: "", wantOK: false},
		{method: "", wantKey: "", wantOK: false},
		{method: "no/such/method", wantKey: "", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.method, func(t *testing.T) {
			t.Parallel()

			gotKey, gotOK := mcp.TargetParamKey(tc.method)
			if gotOK != tc.wantOK {
				t.Errorf("TargetParamKey(%q) ok = %v, want %v", tc.method, gotOK, tc.wantOK)
			}
			if gotKey != tc.wantKey {
				t.Errorf("TargetParamKey(%q) key = %q, want %q", tc.method, gotKey, tc.wantKey)
			}
		})
	}
}
