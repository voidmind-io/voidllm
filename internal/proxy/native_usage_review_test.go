package proxy

import "testing"

// TestNativeStream_MessageDeltaCumulativeInput verifies that cumulative input
// and cache figures in message_delta win over smaller start values, and that
// smaller delta values never lower them.
func TestNativeStream_MessageDeltaCumulativeInput(t *testing.T) {
	t.Parallel()
	n := &nativeStream{format: formatMessages}
	feed(n,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":50,"cache_creation_input_tokens":0,"output_tokens":1}}}`, ``,
		`data: {"type":"message_delta","delta":{},"usage":{"input_tokens":12,"cache_read_input_tokens":40,"cache_creation_input_tokens":7,"output_tokens":30}}`, ``,
	)
	got := n.usage()
	want := UsageInfo{PromptTokens: 12 + 50 + 7, CompletionTokens: 30, TotalTokens: 12 + 50 + 7 + 30, CachedReadTokens: 50, CacheWriteTokens: 7}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}
