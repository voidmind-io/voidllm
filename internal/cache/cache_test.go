package cache_test

import (
	"sync"
	"testing"

	"github.com/voidmind-io/voidllm/internal/cache"
)

// newIntCache is a convenience constructor for Cache[string, int].
func newIntCache() *cache.Cache[string, int] {
	return cache.New[string, int]()
}

// newStrCache is a convenience constructor for Cache[string, string].
func newStrCache() *cache.Cache[string, string] {
	return cache.New[string, string]()
}

// ---- Get ---------------------------------------------------------------

func TestGet_EmptyCache(t *testing.T) {
	t.Parallel()

	c := newIntCache()
	got, ok := c.Get("missing")
	if ok {
		t.Errorf("Get on empty cache: ok = true, want false")
	}
	if got != 0 {
		t.Errorf("Get on empty cache: value = %d, want 0 (zero value)", got)
	}
}

// ---- Set + Get ---------------------------------------------------------

func TestSetGet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		key   string
		value int
	}{
		{name: "integer value", key: "a", value: 42},
		{name: "zero value stored explicitly", key: "b", value: 0},
		{name: "negative value", key: "c", value: -99},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := newIntCache()
			c.Set(tc.key, tc.value)

			got, ok := c.Get(tc.key)
			if !ok {
				t.Fatalf("Get after Set: ok = false, want true")
			}
			if got != tc.value {
				t.Errorf("Get after Set: value = %d, want %d", got, tc.value)
			}
		})
	}
}

func TestSet_Overwrites(t *testing.T) {
	t.Parallel()

	c := newIntCache()
	c.Set("key", 1)
	c.Set("key", 2)

	got, ok := c.Get("key")
	if !ok {
		t.Fatalf("Get after overwrite: ok = false, want true")
	}
	if got != 2 {
		t.Errorf("Get after overwrite: value = %d, want 2", got)
	}
}

// ---- Delete ------------------------------------------------------------

func TestDelete_RemovesEntry(t *testing.T) {
	t.Parallel()

	c := newIntCache()
	c.Set("x", 10)
	c.Delete("x")

	_, ok := c.Get("x")
	if ok {
		t.Errorf("Get after Delete: ok = true, want false")
	}
}

func TestDelete_NonExistentKey(t *testing.T) {
	t.Parallel()

	c := newIntCache()
	// Must not panic or return an error.
	c.Delete("does-not-exist")
}

// ---- LoadAll -----------------------------------------------------------

func TestLoadAll_ReplacesContents(t *testing.T) {
	t.Parallel()

	c := newStrCache()
	c.Set("old-key", "old-value")

	incoming := map[string]string{
		"new-a": "alpha",
		"new-b": "beta",
	}
	c.LoadAll(incoming)

	// old key must be gone
	if _, ok := c.Get("old-key"); ok {
		t.Errorf("LoadAll: old-key still present after LoadAll, want removed")
	}

	// new keys must be present with correct values
	for k, want := range incoming {
		got, ok := c.Get(k)
		if !ok {
			t.Errorf("LoadAll: key %q not found after LoadAll", k)
			continue
		}
		if got != want {
			t.Errorf("LoadAll: key %q = %q, want %q", k, got, want)
		}
	}
}

func TestLoadAll_EmptyMap_ClearsCache(t *testing.T) {
	t.Parallel()

	c := newStrCache()
	c.Set("a", "1")
	c.Set("b", "2")

	c.LoadAll(map[string]string{})

	if n := c.Len(); n != 0 {
		t.Errorf("LoadAll(empty): Len = %d, want 0", n)
	}
}

func TestLoadAll_ThenLen(t *testing.T) {
	t.Parallel()

	c := newStrCache()
	incoming := map[string]string{"x": "1", "y": "2", "z": "3"}
	c.LoadAll(incoming)

	if got := c.Len(); got != len(incoming) {
		t.Errorf("Len after LoadAll: got %d, want %d", got, len(incoming))
	}
}

// ---- Len ---------------------------------------------------------------

func TestLen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(c *cache.Cache[string, int])
		wantLen int
	}{
		{
			name:    "empty cache",
			setup:   func(c *cache.Cache[string, int]) {},
			wantLen: 0,
		},
		{
			name: "after three sets",
			setup: func(c *cache.Cache[string, int]) {
				c.Set("a", 1)
				c.Set("b", 2)
				c.Set("c", 3)
			},
			wantLen: 3,
		},
		{
			name: "after set then delete",
			setup: func(c *cache.Cache[string, int]) {
				c.Set("a", 1)
				c.Set("b", 2)
				c.Delete("a")
			},
			wantLen: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := newIntCache()
			tc.setup(c)

			if got := c.Len(); got != tc.wantLen {
				t.Errorf("Len = %d, want %d", got, tc.wantLen)
			}
		})
	}
}

// ---- Clear -------------------------------------------------------------

func TestClear(t *testing.T) {
	t.Parallel()

	c := newStrCache()
	keys := []string{"a", "b", "c"}
	for _, k := range keys {
		c.Set(k, "val")
	}

	c.Clear()

	if n := c.Len(); n != 0 {
		t.Errorf("Len after Clear = %d, want 0", n)
	}
	for _, k := range keys {
		if _, ok := c.Get(k); ok {
			t.Errorf("Get(%q) after Clear: ok = true, want false", k)
		}
	}
}

func TestClear_AlreadyEmpty(t *testing.T) {
	t.Parallel()

	c := newIntCache()
	c.Clear() // must not panic

	if n := c.Len(); n != 0 {
		t.Errorf("Len after Clear on empty cache = %d, want 0", n)
	}
}

// ---- Range -------------------------------------------------------------

func TestRange_VisitsAllEntries(t *testing.T) {
	t.Parallel()

	c := newIntCache()
	want := map[string]int{"a": 1, "b": 2, "c": 3}
	for k, v := range want {
		c.Set(k, v)
	}

	got := make(map[string]int)
	c.Range(func(k string, v int) bool {
		got[k] = v
		return true
	})

	if len(got) != len(want) {
		t.Fatalf("Range visited %d entries, want %d", len(got), len(want))
	}
	for k, wantV := range want {
		if gotV, ok := got[k]; !ok || gotV != wantV {
			t.Errorf("Range: key %q = %d (ok=%v), want %d", k, gotV, ok, wantV)
		}
	}
}

func TestRange_StopsOnFalse(t *testing.T) {
	t.Parallel()

	c := newIntCache()
	for i := range 10 {
		c.Set(string(rune('a'+i)), i)
	}

	visited := 0
	c.Range(func(_ string, _ int) bool {
		visited++
		return visited < 3 // stop after 3
	})

	// Map iteration order is arbitrary, but we must have stopped early.
	if visited > 3 {
		t.Errorf("Range did not stop: visited %d entries, want <= 3", visited)
	}
}

func TestRange_EmptyCache_FnNeverCalled(t *testing.T) {
	t.Parallel()

	c := newIntCache()
	called := false
	c.Range(func(_ string, _ int) bool {
		called = true
		return true
	})

	if called {
		t.Error("Range on empty cache: fn was called, want never called")
	}
}

// ---- Concurrency -------------------------------------------------------

// TestConcurrency spawns 100 goroutines that interleave Get, Set, and Delete
// operations on the same cache. The test itself makes no value assertions —
// its purpose is to expose data races when run with -race.
func TestConcurrency(t *testing.T) {
	t.Parallel()

	const goroutines = 100
	c := newIntCache()

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(id int) {
			defer wg.Done()

			key := string(rune('a' + id%26))

			c.Set(key, id)
			c.Get(key)
			c.Set(key, id*2)
			c.Get(key)
			c.Delete(key)
			c.Get(key)
		}(i)
	}

	wg.Wait()
}

// TestConcurrencyLoadAll verifies that concurrent LoadAll calls interleaved
// with Get/Set do not race.
func TestConcurrencyLoadAll(t *testing.T) {
	t.Parallel()

	c := newStrCache()

	var wg sync.WaitGroup
	const writers = 20
	const readers = 20

	wg.Add(writers + readers)

	for i := range writers {
		go func(id int) {
			defer wg.Done()
			m := map[string]string{
				"k1":                      "v1",
				"k2":                      "v2",
				string(rune('a' + id%26)): "x",
			}
			c.LoadAll(m)
		}(i)
	}

	for range readers {
		go func() {
			defer wg.Done()
			c.Get("k1")
			c.Len()
			c.Range(func(_ string, _ string) bool { return true })
		}()
	}

	wg.Wait()
}
