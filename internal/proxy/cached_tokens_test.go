package proxy

// cached_tokens_test.go covers the cached-token normalization introduced for
// #179: OpenAI and Gemini report cached tokens as a SUBSET of their
// prompt-token total, while Anthropic reports them IN ADDITION TO
// input_tokens. See UsageInfo's doc comment in adapter.go for the full
// per-provider reconciliation this file exercises.

import (
	"encoding/json"
	"testing"
)

// ---- extractUsage (buffered, passthrough OpenAI-shaped bodies) -------------

// TestExtractUsage_CachedTokens verifies that extractUsage recovers
// CachedReadTokens and CacheWriteTokens from usage.prompt_tokens_details
// without disturbing PromptTokens (OpenAI reports cached_tokens as a subset,
// so PromptTokens is used exactly as the upstream reported it).
func TestExtractUsage_CachedTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		body           string
		wantPrompt     int
		wantCompletion int
		wantTotal      int
		wantCachedRead int
		wantCacheWrite int
	}{
		{
			name:           "cached_tokens present as subset of prompt_tokens",
			body:           `{"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40}}}`,
			wantPrompt:     100,
			wantCompletion: 20,
			wantTotal:      120,
			wantCachedRead: 40,
			wantCacheWrite: 0,
		},
		{
			name:           "cache_creation_tokens (proxy-internal extension) also recovered",
			body:           `{"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40,"cache_creation_tokens":15}}}`,
			wantPrompt:     100,
			wantCompletion: 20,
			wantTotal:      120,
			wantCachedRead: 40,
			wantCacheWrite: 15,
		},
		{
			name:           "no prompt_tokens_details object leaves cache counts at zero",
			body:           `{"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`,
			wantPrompt:     100,
			wantCompletion: 20,
			wantTotal:      120,
			wantCachedRead: 0,
			wantCacheWrite: 0,
		},
		{
			name:           "prompt_tokens_details present but all-zero",
			body:           `{"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":0}}}`,
			wantPrompt:     100,
			wantCompletion: 20,
			wantTotal:      120,
			wantCachedRead: 0,
			wantCacheWrite: 0,
		},
		{
			name:           "no usage field at all returns zero UsageInfo",
			body:           `{"choices":[]}`,
			wantPrompt:     0,
			wantCompletion: 0,
			wantTotal:      0,
			wantCachedRead: 0,
			wantCacheWrite: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := extractUsage([]byte(tc.body))
			if got.PromptTokens != tc.wantPrompt {
				t.Errorf("PromptTokens = %d, want %d", got.PromptTokens, tc.wantPrompt)
			}
			if got.CompletionTokens != tc.wantCompletion {
				t.Errorf("CompletionTokens = %d, want %d", got.CompletionTokens, tc.wantCompletion)
			}
			if got.TotalTokens != tc.wantTotal {
				t.Errorf("TotalTokens = %d, want %d", got.TotalTokens, tc.wantTotal)
			}
			if got.CachedReadTokens != tc.wantCachedRead {
				t.Errorf("CachedReadTokens = %d, want %d", got.CachedReadTokens, tc.wantCachedRead)
			}
			if got.CacheWriteTokens != tc.wantCacheWrite {
				t.Errorf("CacheWriteTokens = %d, want %d", got.CacheWriteTokens, tc.wantCacheWrite)
			}
		})
	}
}

// ---- streamUsageExtractor.observe (streaming, passthrough OpenAI-shaped) ---

// TestStreamUsageExtractor_ObserveCachedTokens verifies that observe recovers
// cached/cache-write counts from an SSE data line's prompt_tokens_details,
// and that a later observe() call fully replaces (not merges with) the prior
// lastUsage — matching the pre-existing behavior for prompt/completion tokens.
func TestStreamUsageExtractor_ObserveCachedTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		lines          []string
		wantPrompt     int
		wantCachedRead int
		wantCacheWrite int
	}{
		{
			name:           "no usage line observed leaves zero UsageInfo",
			lines:          []string{`data: {"choices":[{"delta":{"content":"hi"}}]}`},
			wantPrompt:     0,
			wantCachedRead: 0,
			wantCacheWrite: 0,
		},
		{
			name: "final chunk carries cached_tokens subset",
			lines: []string{
				`data: {"choices":[{"delta":{"content":"hi"}}]}`,
				`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":80,"completion_tokens":10,"total_tokens":90,"prompt_tokens_details":{"cached_tokens":30}}}`,
			},
			wantPrompt:     80,
			wantCachedRead: 30,
			wantCacheWrite: 0,
		},
		{
			name: "cache_creation_tokens recovered alongside cached_tokens",
			lines: []string{
				`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":80,"completion_tokens":10,"total_tokens":90,"prompt_tokens_details":{"cached_tokens":30,"cache_creation_tokens":12}}}`,
			},
			wantPrompt:     80,
			wantCachedRead: 30,
			wantCacheWrite: 12,
		},
		{
			name: "a later usage line without details resets cache counts to zero",
			lines: []string{
				`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":80,"completion_tokens":10,"total_tokens":90,"prompt_tokens_details":{"cached_tokens":30}}}`,
				`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":80,"completion_tokens":10,"total_tokens":90}}`,
			},
			wantPrompt:     80,
			wantCachedRead: 0,
			wantCacheWrite: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var ex streamUsageExtractor
			for _, line := range tc.lines {
				ex.observe([]byte(line))
			}

			if ex.lastUsage.PromptTokens != tc.wantPrompt {
				t.Errorf("PromptTokens = %d, want %d", ex.lastUsage.PromptTokens, tc.wantPrompt)
			}
			if ex.lastUsage.CachedReadTokens != tc.wantCachedRead {
				t.Errorf("CachedReadTokens = %d, want %d", ex.lastUsage.CachedReadTokens, tc.wantCachedRead)
			}
			if ex.lastUsage.CacheWriteTokens != tc.wantCacheWrite {
				t.Errorf("CacheWriteTokens = %d, want %d", ex.lastUsage.CacheWriteTokens, tc.wantCacheWrite)
			}
		})
	}
}

// ---- Gemini: buffered TransformResponse -----------------------------------

// TestGeminiTransformResponse_CachedTokens verifies that cachedContentTokenCount
// is surfaced in the transformed response's usage.prompt_tokens_details as a
// SUBSET of prompt_tokens (PromptTokens itself is left unchanged), and that the
// round trip through extractUsage recovers the same counts — this is the exact
// path handleBufferedResponse exercises: TransformResponse's output is what
// extractUsage actually parses.
func TestGeminiTransformResponse_CachedTokens(t *testing.T) {
	t.Parallel()

	input := `{
		"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":50,"cachedContentTokenCount":20,"candidatesTokenCount":8,"totalTokenCount":58}
	}`

	a := &GeminiAdapter{}
	out, err := a.TransformResponse([]byte(input))
	if err != nil {
		t.Fatalf("TransformResponse() error = %v", err)
	}

	var resp openAIResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("output is not valid openAIResponse JSON: %v", err)
	}

	if resp.Usage.PromptTokens != 50 {
		t.Errorf("usage.prompt_tokens = %d, want 50 (cache is a subset, unchanged)", resp.Usage.PromptTokens)
	}
	if resp.Usage.PromptTokensDetails == nil {
		t.Fatal("usage.prompt_tokens_details is nil, want non-nil")
	}
	if resp.Usage.PromptTokensDetails.CachedTokens != 20 {
		t.Errorf("usage.prompt_tokens_details.cached_tokens = %d, want 20", resp.Usage.PromptTokensDetails.CachedTokens)
	}

	// Round trip: this is what extractUsage actually receives on the buffered path.
	ui := extractUsage(out)
	if ui.PromptTokens != 50 {
		t.Errorf("extractUsage: PromptTokens = %d, want 50", ui.PromptTokens)
	}
	if ui.CachedReadTokens != 20 {
		t.Errorf("extractUsage: CachedReadTokens = %d, want 20", ui.CachedReadTokens)
	}
	if ui.CacheWriteTokens != 0 {
		t.Errorf("extractUsage: CacheWriteTokens = %d, want 0 (Gemini has no cache-write concept)", ui.CacheWriteTokens)
	}
}

// TestGeminiTransformResponse_NoCachedTokens verifies that a response with no
// cachedContentTokenCount omits prompt_tokens_details entirely, matching the
// real OpenAI/Gemini wire shape for unaffected requests.
func TestGeminiTransformResponse_NoCachedTokens(t *testing.T) {
	t.Parallel()

	input := `{
		"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":50,"candidatesTokenCount":8,"totalTokenCount":58}
	}`

	a := &GeminiAdapter{}
	out, err := a.TransformResponse([]byte(input))
	if err != nil {
		t.Fatalf("TransformResponse() error = %v", err)
	}

	var resp openAIResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("output is not valid openAIResponse JSON: %v", err)
	}
	if resp.Usage.PromptTokensDetails != nil {
		t.Errorf("usage.prompt_tokens_details = %+v, want nil when no cached tokens reported", resp.Usage.PromptTokensDetails)
	}
}

// ---- Gemini: streaming accumulation ----------------------------------------

// TestGeminiStreamUsage_CachedTokens verifies that cachedContentTokenCount is
// accumulated across stream chunks the same way promptTokens/completionTokens
// are, and that it remains a SUBSET of PromptTokens (not added into it).
func TestGeminiStreamUsage_CachedTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		lines          []string
		wantPrompt     int
		wantCompletion int
		wantCachedRead int
	}{
		{
			name:           "zero cache usage before any stream lines",
			lines:          nil,
			wantPrompt:     0,
			wantCompletion: 0,
			wantCachedRead: 0,
		},
		{
			name: "cached tokens accumulated from final chunk",
			lines: []string{
				`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"a"}]},"finishReason":""}],"usageMetadata":{}}`,
				`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"b"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":40,"cachedContentTokenCount":25,"candidatesTokenCount":6,"totalTokenCount":46}}`,
			},
			wantPrompt:     40,
			wantCompletion: 6,
			wantCachedRead: 25,
		},
		{
			name: "mid-stream chunk with cache info updates before the terminal chunk",
			lines: []string{
				`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"a"}]},"finishReason":""}],"usageMetadata":{"promptTokenCount":40,"cachedContentTokenCount":25,"candidatesTokenCount":0,"totalTokenCount":40}}`,
				`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"b"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":40,"cachedContentTokenCount":25,"candidatesTokenCount":6,"totalTokenCount":46}}`,
			},
			wantPrompt:     40,
			wantCompletion: 6,
			wantCachedRead: 25,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &GeminiAdapter{}
			for _, line := range tc.lines {
				transformLineIgnore(a, []byte(line))
			}

			got := a.StreamUsage()
			if got.PromptTokens != tc.wantPrompt {
				t.Errorf("PromptTokens = %d, want %d", got.PromptTokens, tc.wantPrompt)
			}
			if got.CompletionTokens != tc.wantCompletion {
				t.Errorf("CompletionTokens = %d, want %d", got.CompletionTokens, tc.wantCompletion)
			}
			if got.CachedReadTokens != tc.wantCachedRead {
				t.Errorf("CachedReadTokens = %d, want %d", got.CachedReadTokens, tc.wantCachedRead)
			}
			if got.CacheWriteTokens != 0 {
				t.Errorf("CacheWriteTokens = %d, want 0 (Gemini has no cache-write concept)", got.CacheWriteTokens)
			}
		})
	}
}

// ---- Anthropic: buffered TransformResponse ---------------------------------

// TestAnthropicTransformResponse_CachedTokens verifies that
// cache_read_input_tokens and cache_creation_input_tokens are ADDED into
// promptTokens/totalTokens (Anthropic reports them in addition to
// input_tokens, not as a subset), that the individual buckets still surface
// via prompt_tokens_details, and that the round trip through extractUsage
// (the buffered path's actual input) recovers all four figures correctly.
// This is the under-reporting fix for #179: before it, an Anthropic request
// with heavy cache reads had its prompt/total tokens under-counted by exactly
// the cached amount.
func TestAnthropicTransformResponse_CachedTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		inputJSON         string
		wantPrompt        int
		wantCompletion    int
		wantTotal         int
		wantCachedRead    int
		wantCacheWrite    int
		wantDetailsAbsent bool
	}{
		{
			name: "cache_read_input_tokens added into prompt/total tokens",
			inputJSON: `{"id":"msg_1","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
				`"usage":{"input_tokens":10,"cache_read_input_tokens":40,"output_tokens":5}}`,
			wantPrompt:     50, // 10 + 40, NOT the raw 10 Anthropic reported
			wantCompletion: 5,
			wantTotal:      55, // 50 + 5
			wantCachedRead: 40,
			wantCacheWrite: 0,
		},
		{
			name: "cache_creation_input_tokens (cache write) also added",
			inputJSON: `{"id":"msg_2","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
				`"usage":{"input_tokens":10,"cache_creation_input_tokens":25,"output_tokens":5}}`,
			wantPrompt:     35, // 10 + 25
			wantCompletion: 5,
			wantTotal:      40,
			wantCachedRead: 0,
			wantCacheWrite: 25,
		},
		{
			name: "both cache_read and cache_creation added simultaneously",
			inputJSON: `{"id":"msg_3","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
				`"usage":{"input_tokens":10,"cache_read_input_tokens":40,"cache_creation_input_tokens":25,"output_tokens":5}}`,
			wantPrompt:     75, // 10 + 40 + 25
			wantCompletion: 5,
			wantTotal:      80,
			wantCachedRead: 40,
			wantCacheWrite: 25,
		},
		{
			name: "no cache fields reported: prompt_tokens_details omitted, prompt unchanged",
			inputJSON: `{"id":"msg_4","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
				`"usage":{"input_tokens":10,"output_tokens":5}}`,
			wantPrompt:        10,
			wantCompletion:    5,
			wantTotal:         15,
			wantCachedRead:    0,
			wantCacheWrite:    0,
			wantDetailsAbsent: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AnthropicAdapter{}
			out, err := a.TransformResponse([]byte(tc.inputJSON))
			if err != nil {
				t.Fatalf("TransformResponse() error = %v", err)
			}

			var resp openAIResponse
			if err := json.Unmarshal(out, &resp); err != nil {
				t.Fatalf("output is not valid openAIResponse JSON: %v", err)
			}

			if resp.Usage.PromptTokens != tc.wantPrompt {
				t.Errorf("usage.prompt_tokens = %d, want %d", resp.Usage.PromptTokens, tc.wantPrompt)
			}
			if resp.Usage.CompletionTokens != tc.wantCompletion {
				t.Errorf("usage.completion_tokens = %d, want %d", resp.Usage.CompletionTokens, tc.wantCompletion)
			}
			if resp.Usage.TotalTokens != tc.wantTotal {
				t.Errorf("usage.total_tokens = %d, want %d", resp.Usage.TotalTokens, tc.wantTotal)
			}

			if tc.wantDetailsAbsent {
				if resp.Usage.PromptTokensDetails != nil {
					t.Errorf("usage.prompt_tokens_details = %+v, want nil", resp.Usage.PromptTokensDetails)
				}
			} else {
				if resp.Usage.PromptTokensDetails == nil {
					t.Fatal("usage.prompt_tokens_details is nil, want non-nil")
				}
				if resp.Usage.PromptTokensDetails.CachedTokens != tc.wantCachedRead {
					t.Errorf("prompt_tokens_details.cached_tokens = %d, want %d", resp.Usage.PromptTokensDetails.CachedTokens, tc.wantCachedRead)
				}
				if resp.Usage.PromptTokensDetails.CacheCreationTokens != tc.wantCacheWrite {
					t.Errorf("prompt_tokens_details.cache_creation_tokens = %d, want %d", resp.Usage.PromptTokensDetails.CacheCreationTokens, tc.wantCacheWrite)
				}
			}

			// Round trip: this is what extractUsage actually receives on the
			// buffered path (handleBufferedResponse feeds it the adapter's
			// TransformResponse output, not the raw Anthropic body).
			ui := extractUsage(out)
			if ui.PromptTokens != tc.wantPrompt {
				t.Errorf("extractUsage: PromptTokens = %d, want %d", ui.PromptTokens, tc.wantPrompt)
			}
			if ui.TotalTokens != tc.wantTotal {
				t.Errorf("extractUsage: TotalTokens = %d, want %d", ui.TotalTokens, tc.wantTotal)
			}
			if ui.CachedReadTokens != tc.wantCachedRead {
				t.Errorf("extractUsage: CachedReadTokens = %d, want %d", ui.CachedReadTokens, tc.wantCachedRead)
			}
			if ui.CacheWriteTokens != tc.wantCacheWrite {
				t.Errorf("extractUsage: CacheWriteTokens = %d, want %d", ui.CacheWriteTokens, tc.wantCacheWrite)
			}
		})
	}
}

// ---- Anthropic: StreamUsage (never previously tested) ----------------------

// TestAnthropicStreamUsage verifies AnthropicAdapter.StreamUsage(), which had
// no test coverage anywhere in the repo before #179. It exercises the full
// accumulation path: message_start carries input_tokens plus both cache
// counters, message_delta carries output_tokens, and StreamUsage() must fold
// the cache counters additively into PromptTokens/TotalTokens exactly as
// TransformResponse does for the buffered path.
func TestAnthropicStreamUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		events         []string
		wantPrompt     int
		wantCompletion int
		wantTotal      int
		wantCachedRead int
		wantCacheWrite int
	}{
		{
			name:           "zero usage before any stream events",
			events:         nil,
			wantPrompt:     0,
			wantCompletion: 0,
			wantTotal:      0,
			wantCachedRead: 0,
			wantCacheWrite: 0,
		},
		{
			name: "input_tokens only, no cache activity",
			events: []string{
				`data: {"type":"message_start","message":{"id":"msg_a","usage":{"input_tokens":12}}}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":8}}`,
			},
			wantPrompt:     12,
			wantCompletion: 8,
			wantTotal:      20,
			wantCachedRead: 0,
			wantCacheWrite: 0,
		},
		{
			name: "cache_read_input_tokens added into PromptTokens/TotalTokens",
			events: []string{
				`data: {"type":"message_start","message":{"id":"msg_b","usage":{"input_tokens":10,"cache_read_input_tokens":40}}}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
			},
			wantPrompt:     50, // 10 + 40, not the raw 10
			wantCompletion: 5,
			wantTotal:      55,
			wantCachedRead: 40,
			wantCacheWrite: 0,
		},
		{
			name: "cache_creation_input_tokens (cache write) added into PromptTokens/TotalTokens",
			events: []string{
				`data: {"type":"message_start","message":{"id":"msg_c","usage":{"input_tokens":10,"cache_creation_input_tokens":25}}}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
			},
			wantPrompt:     35,
			wantCompletion: 5,
			wantTotal:      40,
			wantCachedRead: 0,
			wantCacheWrite: 25,
		},
		{
			name: "both cache_read and cache_creation added simultaneously",
			events: []string{
				`data: {"type":"message_start","message":{"id":"msg_d","usage":{"input_tokens":10,"cache_read_input_tokens":40,"cache_creation_input_tokens":25}}}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
			},
			wantPrompt:     75,
			wantCompletion: 5,
			wantTotal:      80,
			wantCachedRead: 40,
			wantCacheWrite: 25,
		},
		{
			name: "only message_start seen (no message_delta yet): output remains zero",
			events: []string{
				`data: {"type":"message_start","message":{"id":"msg_e","usage":{"input_tokens":10,"cache_read_input_tokens":40}}}`,
			},
			wantPrompt:     50,
			wantCompletion: 0,
			wantTotal:      50,
			wantCachedRead: 40,
			wantCacheWrite: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AnthropicAdapter{}
			for _, ev := range tc.events {
				transformLineIgnore(a, []byte(ev))
			}

			got := a.StreamUsage()
			if got.PromptTokens != tc.wantPrompt {
				t.Errorf("PromptTokens = %d, want %d", got.PromptTokens, tc.wantPrompt)
			}
			if got.CompletionTokens != tc.wantCompletion {
				t.Errorf("CompletionTokens = %d, want %d", got.CompletionTokens, tc.wantCompletion)
			}
			if got.TotalTokens != tc.wantTotal {
				t.Errorf("TotalTokens = %d, want %d", got.TotalTokens, tc.wantTotal)
			}
			if got.CachedReadTokens != tc.wantCachedRead {
				t.Errorf("CachedReadTokens = %d, want %d", got.CachedReadTokens, tc.wantCachedRead)
			}
			if got.CacheWriteTokens != tc.wantCacheWrite {
				t.Errorf("CacheWriteTokens = %d, want %d", got.CacheWriteTokens, tc.wantCacheWrite)
			}
		})
	}
}

// ---- The provider comparison: subset vs. additive, side by side -----------

// TestCachedTokenAccounting_SubsetVsAdditive is the single most important test
// in this file. All three scenarios below represent the identical logical
// event: an upstream served 15 total prompt tokens, of which 5 came from its
// prompt cache. OpenAI and Gemini report that on the wire as prompt/prompt
// tokens already totalling 15 with a "5 of those were cached" annotation
// (cached_tokens/cachedContentTokenCount is a SUBSET). Anthropic instead
// reports a 10-token input_tokens count and a *separate* 5-token
// cache_read_input_tokens count that is NOT included in the 10 (ADDITIVE).
//
// If the proxy naively used each provider's raw prompt-token field, Anthropic
// would silently under-report by exactly the cached amount (10 instead of 15)
// while OpenAI and Gemini would be correct — this test proves the proxy
// normalizes all three to the same PromptTokens=15, CachedReadTokens=5,
// regardless of how the upstream chose to represent it on the wire.
func TestCachedTokenAccounting_SubsetVsAdditive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		wireJSON       string
		adapter        Adapter // nil for OpenAI passthrough (no adapter transform)
		wantPrompt     int
		wantCachedRead int
	}{
		{
			name: "OpenAI: cached_tokens is a SUBSET of prompt_tokens",
			wireJSON: `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":15,"completion_tokens":1,"total_tokens":16,"prompt_tokens_details":{"cached_tokens":5}}}`,
			adapter:        nil,
			wantPrompt:     15,
			wantCachedRead: 5,
		},
		{
			name: "Gemini: cachedContentTokenCount is a SUBSET of promptTokenCount",
			wireJSON: `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],` +
				`"usageMetadata":{"promptTokenCount":15,"cachedContentTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":16}}`,
			adapter:        &GeminiAdapter{},
			wantPrompt:     15,
			wantCachedRead: 5,
		},
		{
			name: "Anthropic: cache_read_input_tokens is ADDITIVE to input_tokens",
			wireJSON: `{"id":"msg_1","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
				`"usage":{"input_tokens":10,"cache_read_input_tokens":5,"output_tokens":1}}`,
			adapter:        &AnthropicAdapter{},
			wantPrompt:     15, // 10 + 5 — NOT the raw 10 Anthropic reported on the wire
			wantCachedRead: 5,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			body := []byte(tc.wireJSON)
			if tc.adapter != nil {
				out, err := tc.adapter.TransformResponse(body)
				if err != nil {
					t.Fatalf("TransformResponse() error = %v", err)
				}
				body = out
			}

			ui := extractUsage(body)
			if ui.PromptTokens != tc.wantPrompt {
				t.Errorf("PromptTokens = %d, want %d (normalized across providers)", ui.PromptTokens, tc.wantPrompt)
			}
			if ui.CachedReadTokens != tc.wantCachedRead {
				t.Errorf("CachedReadTokens = %d, want %d", ui.CachedReadTokens, tc.wantCachedRead)
			}
		})
	}
}
