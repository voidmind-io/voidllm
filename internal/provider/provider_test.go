package provider_test

import (
	"testing"

	"github.com/voidmind-io/voidllm/internal/provider"
)

func TestNames_ReturnsSortedSlice(t *testing.T) {
	t.Parallel()

	names := provider.Names()
	if len(names) == 0 {
		t.Fatal("Names() returned empty slice")
	}

	// Must be sorted.
	for i := 1; i < len(names); i++ {
		if names[i] < names[i-1] {
			t.Errorf("Names() not sorted at index %d: %q < %q", i, names[i], names[i-1])
		}
	}

	// Must contain all providers from ValidProviders.
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}
	for p := range provider.ValidProviders {
		if !nameSet[p] {
			t.Errorf("Names() missing provider %q from ValidProviders", p)
		}
	}
}
