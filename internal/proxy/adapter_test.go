package proxy

import (
	"testing"
)

// TestGetAdapter verifies that GetAdapter returns the correct concrete type for
// known providers and nil for providers that use the OpenAI wire format natively.
func TestGetAdapter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		provider      string
		wantNil       bool
		wantAnthropic bool
		wantAzure     bool
	}{
		{
			name:          "anthropic returns AnthropicAdapter",
			provider:      "anthropic",
			wantNil:       false,
			wantAnthropic: true,
		},
		{
			name:      "azure returns AzureAdapter",
			provider:  "azure",
			wantNil:   false,
			wantAzure: true,
		},
		{
			name:     "vllm returns nil",
			provider: "vllm",
			wantNil:  true,
		},
		{
			name:     "openai returns nil",
			provider: "openai",
			wantNil:  true,
		},
		{
			name:     "custom returns nil",
			provider: "custom",
			wantNil:  true,
		},
		{
			name:     "empty string returns nil",
			provider: "",
			wantNil:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := GetAdapter(tc.provider)

			if tc.wantNil {
				if got != nil {
					t.Errorf("GetAdapter(%q) = %T, want nil", tc.provider, got)
				}
				return
			}

			if got == nil {
				t.Fatalf("GetAdapter(%q) = nil, want non-nil", tc.provider)
			}

			if tc.wantAnthropic {
				if _, ok := got.(*AnthropicAdapter); !ok {
					t.Errorf("GetAdapter(%q) = %T, want *AnthropicAdapter", tc.provider, got)
				}
			}

			if tc.wantAzure {
				if _, ok := got.(*AzureAdapter); !ok {
					t.Errorf("GetAdapter(%q) = %T, want *AzureAdapter", tc.provider, got)
				}
			}
		})
	}
}

// TestGetAdapter_FreshInstance verifies that each call to GetAdapter returns a
// new, independent instance so that stateful adapters (e.g. AnthropicAdapter
// tracking a stream message ID) cannot share state across concurrent requests.
func TestGetAdapter_FreshInstance(t *testing.T) {
	t.Parallel()

	a1 := GetAdapter("anthropic")
	a2 := GetAdapter("anthropic")

	if a1 == nil || a2 == nil {
		t.Fatal("expected non-nil adapters")
	}
	if a1 == a2 {
		t.Error("GetAdapter returned the same pointer on two calls; each call must return a fresh instance")
	}
}
