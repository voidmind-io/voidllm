package license_test

import (
	"sync"
	"testing"

	"github.com/voidmind-io/voidllm/internal/license"
)

func TestNewHolder(t *testing.T) {
	t.Parallel()

	lic := license.Verify("", false) // community license
	h := license.NewHolder(lic)

	got := h.Load()
	if got == nil {
		t.Fatal("Load() = nil, want non-nil License")
	}
	if got.Edition() != license.EditionCommunity {
		t.Errorf("Load().Edition() = %q, want %q", got.Edition(), license.EditionCommunity)
	}
}

func TestHolderLoadEmpty(t *testing.T) {
	t.Parallel()

	// A zero-value Holder — nothing has been stored yet.
	var h license.Holder

	got := h.Load()
	if got == nil {
		t.Fatal("Load() = nil, want non-nil communityLicense fallback")
	}
	if got.Edition() != license.EditionCommunity {
		t.Errorf("Load().Edition() = %q, want %q (zero-value Holder must return communityLicense)",
			got.Edition(), license.EditionCommunity)
	}
	if !got.Valid() {
		t.Error("Load().Valid() = false, want true for communityLicense")
	}
	if !got.ExpiresAt().IsZero() {
		t.Errorf("Load().ExpiresAt() = %v, want zero time", got.ExpiresAt())
	}
}

func TestHolderStore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		first       license.License
		replacement license.License
		wantEdition license.Edition
	}{
		{
			name:        "community replaced by dev",
			first:       license.Verify("", false),
			replacement: license.Verify("", true),
			wantEdition: license.EditionDev,
		},
		{
			name:        "dev replaced by community",
			first:       license.Verify("", true),
			replacement: license.Verify("", false),
			wantEdition: license.EditionCommunity,
		},
		{
			name:        "community replaced by community",
			first:       license.Verify("", false),
			replacement: license.Verify("", false),
			wantEdition: license.EditionCommunity,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := license.NewHolder(tc.first)

			h.Store(tc.replacement)

			got := h.Load()
			if got == nil {
				t.Fatal("Load() after Store() = nil, want non-nil")
			}
			if got.Edition() != tc.wantEdition {
				t.Errorf("Load().Edition() = %q, want %q", got.Edition(), tc.wantEdition)
			}
		})
	}
}

func TestHolderConcurrency(t *testing.T) {
	t.Parallel()

	h := license.NewHolder(license.Verify("", false))

	community := license.Verify("", false)
	dev := license.Verify("", true)

	const goroutines = 50
	const opsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				if (idx+j)%3 == 0 {
					h.Store(community)
				} else if (idx+j)%3 == 1 {
					h.Store(dev)
				} else {
					got := h.Load()
					if got == nil {
						// Panic here is caught as a test failure by the race detector.
						panic("Load() returned nil during concurrent access")
					}
					ed := got.Edition()
					if ed != license.EditionCommunity && ed != license.EditionDev {
						panic("Load() returned unexpected edition: " + string(ed))
					}
				}
			}
		}(i)
	}

	wg.Wait()
	// If we get here without the race detector firing, the test passes.
}
