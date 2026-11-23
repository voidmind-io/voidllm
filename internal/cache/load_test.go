package cache

import (
	"fmt"
	"sync"
	"testing"
)

// pending returns the number of point writes currently recorded for loads in
// flight.
func pending[K comparable, V any](c *Cache[K, V]) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.mutations)
}

// TestCommitLoad_PointWritesDuringLoadWin is the stale-snapshot scenario: the
// snapshot is read, then a key is deleted and another one set, then the
// snapshot is installed.
func TestCommitLoad_PointWritesDuringLoadWin(t *testing.T) {
	t.Parallel()

	c := New[string, string]()
	c.Set("revoked", "old")
	c.Set("kept", "old")
	c.Set("updated", "old")
	c.Set("stale", "old") // absent from the snapshot: gone in the source

	token := c.BeginLoad()
	// The snapshot is read here: it still contains "revoked", has an older
	// value for "updated" and does not know "created".
	snapshot := map[string]string{"revoked": "snap", "kept": "snap", "updated": "snap"}

	c.Delete("revoked")
	c.Set("created", "new")
	c.Set("updated", "new")

	if !c.CommitLoad(token, snapshot) {
		t.Fatal("CommitLoad = false, want true")
	}

	if v, ok := c.Get("revoked"); ok {
		t.Errorf("key deleted during the load is back with value %q", v)
	}
	if v, ok := c.Get("created"); !ok || v != "new" {
		t.Errorf("key set during the load = %q, %v; want \"new\", true", v, ok)
	}
	if v, ok := c.Get("updated"); !ok || v != "new" {
		t.Errorf("key updated during the load = %q, %v; want \"new\", true", v, ok)
	}
	if v, ok := c.Get("kept"); !ok || v != "snap" {
		t.Errorf("untouched key = %q, %v; want snapshot value", v, ok)
	}
	if _, ok := c.Get("stale"); ok {
		t.Error("key set before the load and absent from the snapshot survived")
	}
	if got := c.Len(); got != 3 {
		t.Errorf("Len = %d, want 3", got)
	}
}

func TestCommitLoad_LastPointWriteWins(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		write   func(c *Cache[string, string])
		wantVal string
		wantOK  bool
	}{
		{"set then delete", func(c *Cache[string, string]) { c.Set("k", "new"); c.Delete("k") }, "", false},
		{"delete then set", func(c *Cache[string, string]) { c.Delete("k"); c.Set("k", "new") }, "new", true},
		{"no write", func(*Cache[string, string]) {}, "snap", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := New[string, string]()
			c.Set("k", "old")
			token := c.BeginLoad()
			tc.write(c)
			c.CommitLoad(token, map[string]string{"k": "snap"})
			if v, ok := c.Get("k"); ok != tc.wantOK || v != tc.wantVal {
				t.Errorf("Get = %q, %v; want %q, %v", v, ok, tc.wantVal, tc.wantOK)
			}
		})
	}
}

// TestCommitLoad_WritesBeforeLoadDoNotApply verifies that only writes made
// after BeginLoad override the snapshot: the source was read after the others.
func TestCommitLoad_WritesBeforeLoadDoNotApply(t *testing.T) {
	t.Parallel()

	c := New[string, string]()
	first := c.BeginLoad()
	c.Delete("a") // recorded, because a load is in flight
	c.Set("b", "old")

	second := c.BeginLoad() // begins after both writes
	if !c.CommitLoad(second, map[string]string{"a": "snap", "b": "snap"}) {
		t.Fatal("CommitLoad(second) = false")
	}
	if v, ok := c.Get("a"); !ok || v != "snap" {
		t.Errorf("a = %q, %v; want the snapshot value (delete predates the load)", v, ok)
	}
	if v, ok := c.Get("b"); !ok || v != "snap" {
		t.Errorf("b = %q, %v; want the snapshot value (set predates the load)", v, ok)
	}
	c.AbortLoad(first)
}

func TestCommitLoad_SupersededLoadIsDiscarded(t *testing.T) {
	t.Parallel()

	c := New[string, string]()
	older := c.BeginLoad()
	newer := c.BeginLoad()

	if !c.CommitLoad(newer, map[string]string{"k": "newer"}) {
		t.Fatal("CommitLoad(newer) = false")
	}
	if c.CommitLoad(older, map[string]string{"k": "older", "gone": "older"}) {
		t.Error("CommitLoad(older) = true after a newer load was installed")
	}
	if v, _ := c.Get("k"); v != "newer" {
		t.Errorf("k = %q, want \"newer\"", v)
	}
	if _, ok := c.Get("gone"); ok {
		t.Error("entry of the superseded snapshot was installed")
	}
	if got := pending(c); got != 0 {
		t.Errorf("recorded writes = %d after all loads ended, want 0", got)
	}
}

func TestLoadToken_UsedOnce(t *testing.T) {
	t.Parallel()

	c := New[string, string]()
	c.Set("k", "v")

	token := c.BeginLoad()
	c.AbortLoad(token)
	if v, ok := c.Get("k"); !ok || v != "v" {
		t.Errorf("AbortLoad changed the cache: %q, %v", v, ok)
	}
	if c.CommitLoad(token, map[string]string{}) {
		t.Error("CommitLoad after AbortLoad = true")
	}
	if _, ok := c.Get("k"); !ok {
		t.Error("CommitLoad after AbortLoad replaced the cache")
	}

	token = c.BeginLoad()
	if !c.CommitLoad(token, map[string]string{"n": "1"}) {
		t.Fatal("CommitLoad = false")
	}
	if c.CommitLoad(token, map[string]string{}) {
		t.Error("second CommitLoad with the same token = true")
	}
	c.AbortLoad(token) // no-op
	if _, ok := c.Get("n"); !ok {
		t.Error("repeated use of a finished token changed the cache")
	}
}

func TestCommitLoad_NilSnapshot(t *testing.T) {
	t.Parallel()

	c := New[string, string]()
	c.Set("old", "v")
	token := c.BeginLoad()
	c.Set("new", "v")
	if !c.CommitLoad(token, nil) {
		t.Fatal("CommitLoad(nil) = false")
	}
	if _, ok := c.Get("old"); ok {
		t.Error("old entry survived an empty snapshot")
	}
	if _, ok := c.Get("new"); !ok {
		t.Error("entry set during the load was lost")
	}
	c.Set("after", "v") // must not panic on a nil map
}

func TestLoad_RecordsArePruned(t *testing.T) {
	t.Parallel()

	c := New[string, string]()

	// No load in flight: nothing is recorded.
	c.Set("a", "1")
	c.Delete("a")
	if got := pending(c); got != 0 {
		t.Fatalf("recorded writes without a load = %d, want 0", got)
	}

	// One load: records accumulate, one per key, and go away with the load.
	first := c.BeginLoad()
	c.Set("a", "1")
	c.Set("a", "2")
	c.Delete("b")
	if got := pending(c); got != 2 {
		t.Fatalf("recorded writes during a load = %d, want 2", got)
	}

	// A second load begins; only writes after its start are still needed
	// once the first load ends.
	second := c.BeginLoad()
	c.Delete("c")
	if got := pending(c); got != 3 {
		t.Fatalf("recorded writes with two loads = %d, want 3", got)
	}
	c.AbortLoad(first)
	if got := pending(c); got != 1 {
		t.Fatalf("recorded writes after the older load ended = %d, want 1 (only the write newer than the remaining load)", got)
	}

	// The remaining record still applies to the remaining load.
	if !c.CommitLoad(second, map[string]string{"c": "snap"}) {
		t.Fatal("CommitLoad(second) = false")
	}
	if _, ok := c.Get("c"); ok {
		t.Error("key deleted during the second load was installed")
	}
	if got := pending(c); got != 0 {
		t.Errorf("recorded writes after the last load ended = %d, want 0", got)
	}

	// Writes after the last load are again not recorded.
	c.Delete("d")
	if got := pending(c); got != 0 {
		t.Errorf("recorded writes after all loads = %d, want 0", got)
	}
}

// TestLoad_EndingNewerLoadKeepsRecordsForOlder verifies that records are kept
// while an older load can still need them.
func TestLoad_EndingNewerLoadKeepsRecordsForOlder(t *testing.T) {
	t.Parallel()

	c := New[string, string]()
	older := c.BeginLoad()
	c.Delete("x")
	newer := c.BeginLoad()
	c.AbortLoad(newer)
	if got := pending(c); got != 1 {
		t.Fatalf("recorded writes = %d, want 1 (older load still in flight)", got)
	}
	if !c.CommitLoad(older, map[string]string{"x": "snap"}) {
		t.Fatal("CommitLoad(older) = false")
	}
	if _, ok := c.Get("x"); ok {
		t.Error("key deleted during the older load was installed")
	}
}

// TestLoad_Concurrent exercises loads against point writes and reads under the
// race detector, and checks the invariant that a key deleted while a load is
// in flight is absent once everything has settled.
func TestLoad_Concurrent(t *testing.T) {
	t.Parallel()

	c := New[string, int]()
	const keys = 64
	var wg sync.WaitGroup

	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				token := c.BeginLoad()
				snap := make(map[string]int, keys)
				for k := 0; k < keys; k++ {
					snap[fmt.Sprintf("k%d", k)] = i
				}
				if i%7 == 0 {
					c.AbortLoad(token)
					continue
				}
				c.CommitLoad(token, snap)
			}
		}()
	}
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				k := fmt.Sprintf("k%d", (i+w)%keys)
				switch i % 3 {
				case 0:
					c.Set(k, i)
				case 1:
					c.Delete(k)
				default:
					c.Get(k)
					c.Len()
				}
			}
		}(w)
	}
	wg.Wait()

	if got := pending(c); got != 0 {
		t.Errorf("recorded writes after all loads ended = %d, want 0", got)
	}

	// Deterministic tail: a delete during a final load must stick.
	token := c.BeginLoad()
	c.Delete("k0")
	c.CommitLoad(token, map[string]int{"k0": 1, "k1": 1})
	if _, ok := c.Get("k0"); ok {
		t.Error("k0 present after being deleted during the final load")
	}
}
