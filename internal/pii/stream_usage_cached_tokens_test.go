package pii

// stream_usage_cached_tokens_test.go covers the whitelistedUsage
// prompt_tokens_details.cached_tokens extension added alongside the
// Anthropic adapter's include_usage streaming support: numeric cached_tokens
// must survive StreamRestorer's usage whitelist, and the object as a whole
// must still be dropped (skip, not fail-closed) when any usage field is
// non-numeric. It reuses the realPseudonym and NewStreamRestorer helpers
// declared in stream_restorer_test.go.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStreamRestorer_UsageChunk_CachedTokensPreserved verifies that a
// usage-only chunk (choices=[]) carrying usage.prompt_tokens_details.cached_tokens
// re-emits that field with its numeric value intact, while any other field
// inside prompt_tokens_details (e.g. a proxy-internal cache_creation_tokens
// extension) is stripped since it is not on the whitelist.
func TestStreamRestorer_UsageChunk_CachedTokensPreserved(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "cachedtokens@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	usageLine := `data: {"id":"cid","object":"chat.completion.chunk","choices":[],` +
		`"usage":{"prompt_tokens":75,"completion_tokens":30,"total_tokens":105,` +
		`"prompt_tokens_details":{"cached_tokens":20,"cache_creation_tokens":5}}}`

	out, _, err := r.Push([]byte(usageLine))
	if err != nil {
		t.Fatalf("usage-only chunk: %v", err)
	}

	var emitted string
	for _, b := range out {
		if b != nil {
			emitted = string(b)
		}
	}
	if !strings.HasPrefix(emitted, "data: ") {
		t.Fatalf("no data: line emitted for usage chunk; out=%v", out)
	}

	payload := emitted[len("data: "):]
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		t.Fatalf("emitted usage chunk is not valid JSON: %v\npayload: %s", err, payload)
	}

	rawUsage, ok := doc["usage"]
	if !ok {
		t.Fatal("usage field missing from emitted usage chunk")
	}

	var u struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails *struct {
			CachedTokens        int              `json:"cached_tokens"`
			CacheCreationTokens *json.RawMessage `json:"cache_creation_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if err := json.Unmarshal(rawUsage, &u); err != nil {
		t.Fatalf("cannot unmarshal usage: %v", err)
	}
	if u.PromptTokens != 75 {
		t.Errorf("prompt_tokens = %d, want 75", u.PromptTokens)
	}
	if u.CompletionTokens != 30 {
		t.Errorf("completion_tokens = %d, want 30", u.CompletionTokens)
	}
	if u.TotalTokens != 105 {
		t.Errorf("total_tokens = %d, want 105", u.TotalTokens)
	}
	if u.PromptTokensDetails == nil {
		t.Fatal("prompt_tokens_details missing from emitted usage chunk, want preserved")
	}
	if u.PromptTokensDetails.CachedTokens != 20 {
		t.Errorf("prompt_tokens_details.cached_tokens = %d, want 20", u.PromptTokensDetails.CachedTokens)
	}
	if u.PromptTokensDetails.CacheCreationTokens != nil {
		t.Errorf("prompt_tokens_details.cache_creation_tokens = %s, want stripped (not whitelisted)",
			*u.PromptTokensDetails.CacheCreationTokens)
	}
}

// TestStreamRestorer_UsageChunk_AbsentPromptTokensDetails verifies that a
// usage-only chunk with no prompt_tokens_details object at all round-trips
// without emitting an empty object for it (the pointer field stays nil/omitted).
func TestStreamRestorer_UsageChunk_AbsentPromptTokensDetails(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "noprompttokens@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	usageLine := `data: {"id":"cid","object":"chat.completion.chunk","choices":[],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`

	out, _, err := r.Push([]byte(usageLine))
	if err != nil {
		t.Fatalf("usage-only chunk: %v", err)
	}
	var emitted string
	for _, b := range out {
		if b != nil {
			emitted = string(b)
		}
	}
	if strings.Contains(emitted, "prompt_tokens_details") {
		t.Errorf("prompt_tokens_details emitted for a chunk that never carried it: %s", emitted)
	}
}

// TestStreamRestorer_UsageChunk_NonNumericFieldSkipsChunk verifies that a
// usage-only chunk with a non-numeric value in any whitelisted usage field
// (including inside prompt_tokens_details) fails jsonx.Unmarshal for the
// whole usage object; buildUsageChunk's caller treats this the same as any
// other malformed usage chunk — skip the chunk, do not abort the stream.
func TestStreamRestorer_UsageChunk_NonNumericFieldSkipsChunk(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		usageLine string
	}{
		{
			name: "non-numeric top-level prompt_tokens",
			usageLine: `data: {"id":"cid","object":"chat.completion.chunk","choices":[],` +
				`"usage":{"prompt_tokens":"not-a-number","completion_tokens":5,"total_tokens":15}}`,
		},
		{
			name: "non-numeric prompt_tokens_details.cached_tokens",
			usageLine: `data: {"id":"cid","object":"chat.completion.chunk","choices":[],` +
				`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,` +
				`"prompt_tokens_details":{"cached_tokens":"not-a-number"}}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, f := realPseudonym(t, "nonnumeric@example.com")
			r := NewStreamRestorer(f, "gpt-4o")

			out, terminal, err := r.Push([]byte(tc.usageLine))
			if err != nil {
				t.Fatalf("malformed usage chunk must not abort the stream: %v", err)
			}
			if terminal {
				t.Error("malformed usage chunk must not mark the stream terminal")
			}
			for _, b := range out {
				if b != nil {
					t.Errorf("malformed usage chunk must be skipped (no output line), got: %s", b)
				}
			}
		})
	}
}
