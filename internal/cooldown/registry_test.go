package cooldown

import (
	"sync"
	"testing"
	"time"
)

// newRegistryWithClock returns a Registry whose clock is controlled by the
// supplied pointer, following the pattern established by
// internal/auth/login_throttle_test.go's newThrottleWithClock. Tests advance
// *cur to simulate the passage of time without real sleeps.
func newRegistryWithClock(cur *time.Time) *Registry {
	return &Registry{
		until: make(map[string]time.Time),
		now:   func() time.Time { return *cur },
	}
}

// TestMark_Cooling_ReflectsDeadline verifies that Cooling reports true
// immediately after Mark, remains true right up to the deadline, and becomes
// false once the clock advances past it.
func TestMark_Cooling_ReflectsDeadline(t *testing.T) {
	t.Parallel()

	cur := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	r := newRegistryWithClock(&cur)

	r.Mark("dep-a", 10*time.Second)

	if !r.Cooling("dep-a") {
		t.Fatal("Cooling immediately after Mark = false, want true")
	}

	// Just before the deadline: still cooling.
	cur = cur.Add(9999 * time.Millisecond)
	if !r.Cooling("dep-a") {
		t.Error("Cooling 1ms before deadline = false, want true")
	}

	// Exactly at (and past) the deadline: no longer cooling.
	cur = cur.Add(2 * time.Millisecond)
	if r.Cooling("dep-a") {
		t.Error("Cooling after deadline = true, want false")
	}
}

// TestMark_NonPositiveDuration_IsNoOp verifies that Mark with d <= 0 never
// creates a cooldown, and — critically — never shortens or clears an
// existing one.
func TestMark_NonPositiveDuration_IsNoOp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		d    time.Duration
	}{
		{name: "zero duration", d: 0},
		{name: "negative duration", d: -5 * time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cur := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
			r := newRegistryWithClock(&cur)

			// A non-positive Mark on a never-seen key must not create a
			// cooldown.
			r.Mark("fresh-key", tc.d)
			if r.Cooling("fresh-key") {
				t.Error("Cooling after non-positive Mark on fresh key = true, want false")
			}

			// A non-positive Mark must not shorten or clear an existing
			// cooldown established by a prior valid Mark.
			r.Mark("existing-key", time.Minute)
			if !r.Cooling("existing-key") {
				t.Fatal("setup: existing cooldown not active")
			}
			r.Mark("existing-key", tc.d)
			if !r.Cooling("existing-key") {
				t.Error("non-positive Mark cleared an existing cooldown; want it preserved")
			}
		})
	}
}

// TestCooling_UnknownKey_ReturnsFalse verifies that a key which was never
// marked reports as not cooling.
func TestCooling_UnknownKey_ReturnsFalse(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	if r.Cooling("never-marked") {
		t.Error("Cooling for an unknown key = true, want false")
	}
}

// TestNilRegistry_MethodsAreSafe verifies that both Mark and Cooling are safe
// to call on a nil *Registry — the nil-receiver-safe contract documented on
// both methods — and that Cooling always reports false in that case.
func TestNilRegistry_MethodsAreSafe(t *testing.T) {
	t.Parallel()

	var r *Registry

	// Must not panic.
	r.Mark("any-key", time.Minute)

	if r.Cooling("any-key") {
		t.Error("Cooling on nil Registry = true, want false")
	}
}

// TestRegistry_ConcurrentAccess fires many concurrent Mark and Cooling calls
// against both shared and per-goroutine keys and must be free of data races
// (run with -race).
func TestRegistry_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	r := NewRegistry()

	const goroutines = 50
	const opsEach = 200
	const sharedKey = "shared-key"

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			ownKey := "own-key-" + string(rune('A'+g%26))
			for i := 0; i < opsEach; i++ {
				r.Mark(sharedKey, time.Minute)
				r.Mark(ownKey, time.Minute)
				_ = r.Cooling(sharedKey)
				_ = r.Cooling(ownKey)
				_ = r.Cooling("never-marked-key")
			}
		}()
	}
	wg.Wait()

	if !r.Cooling(sharedKey) {
		t.Error("shared key should be cooling after concurrent Mark calls")
	}
}
