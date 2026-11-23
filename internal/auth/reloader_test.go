package auth

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/cache"
)

// newCountingReloader returns a Reloader whose load blocks on gate (when
// non-nil) and counts how often it ran and how many ran concurrently at most.
func newCountingReloader(gate <-chan struct{}) (*Reloader, *atomic.Int32, *atomic.Int32) {
	var runs, maxConcurrent, concurrent atomic.Int32
	r := newReloader(func(ctx context.Context) error {
		n := concurrent.Add(1)
		for {
			m := maxConcurrent.Load()
			if n <= m || maxConcurrent.CompareAndSwap(m, n) {
				break
			}
		}
		runs.Add(1)
		if gate != nil {
			<-gate
		}
		concurrent.Add(-1)
		return nil
	}, discardLogger())
	return r, &runs, &maxConcurrent
}

func TestReloader_CoalescesBurst(t *testing.T) {
	t.Parallel()

	for _, triggers := range []int{1, 2, 10, 200} {
		gate := make(chan struct{})
		r, runs, maxConcurrent := newCountingReloader(gate)

		var wg sync.WaitGroup
		for i := 0; i < triggers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.Trigger()
			}()
		}
		wg.Wait()
		close(gate)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := r.Wait(ctx); err != nil {
			t.Fatalf("triggers=%d: Wait: %v", triggers, err)
		}
		cancel()

		want := int32(2)
		if triggers == 1 {
			want = 1
		}
		if got := runs.Load(); got > want || got < 1 {
			t.Errorf("triggers=%d: loads = %d, want 1..%d", triggers, got, want)
		}
		if got := maxConcurrent.Load(); got != 1 {
			t.Errorf("triggers=%d: max concurrent loads = %d, want 1", triggers, got)
		}
	}
}

// TestReloader_NoLostTrigger verifies that a trigger arriving while a load is
// in flight still produces a load that starts after the trigger.
func TestReloader_NoLostTrigger(t *testing.T) {
	t.Parallel()

	var runs atomic.Int32
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	r := newReloader(func(ctx context.Context) error {
		runs.Add(1)
		started <- struct{}{}
		<-release
		return nil
	}, discardLogger())

	r.Trigger()
	<-started // first load is in flight
	r.Trigger()
	r.Trigger()
	r.Trigger()
	close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := runs.Load(); got != 2 {
		t.Errorf("loads = %d, want 2 (one in flight plus one trailing)", got)
	}
}

// TestReloader_ReloadSeesWriteBeforeCall verifies that Reload only returns after
// a load that started after the call, using a value written before the call.
func TestReloader_ReloadSeesWriteBeforeCall(t *testing.T) {
	t.Parallel()

	var dbValue, cached atomic.Int32
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	var first atomic.Bool
	r := newReloader(func(ctx context.Context) error {
		v := dbValue.Load() // snapshot at load start
		if first.CompareAndSwap(false, true) {
			close(slowStarted)
			<-releaseSlow
		}
		cached.Store(v)
		return nil
	}, discardLogger())

	r.Trigger()
	<-slowStarted // in-flight load already read dbValue == 0
	dbValue.Store(7)

	done := make(chan error, 1)
	go func() { done <- r.Reload(context.Background()) }()
	close(releaseSlow)
	if err := <-done; err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := cached.Load(); got != 7 {
		t.Errorf("cached = %d after Reload, want 7", got)
	}
}

func TestReloader_ReloadContextCanceled(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	r, _, _ := newCountingReloader(gate)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Reload(ctx); err == nil {
		t.Error("Reload with canceled context returned nil, want error")
	}
	close(gate)
	if err := r.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestHandleKeyInvalidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		payload     string
		wantReloads int32
		wantEvicted bool
	}{
		{name: "reload", payload: "reload", wantReloads: 1},
		{name: "org", payload: "org:0190aaaa-0000-7000-8000-000000000001", wantReloads: 1},
		{name: "team", payload: "team:0190aaaa-0000-7000-8000-000000000002", wantReloads: 1},
		{name: "key hash", payload: "abc123hash", wantEvicted: true},
		{name: "empty", payload: "", wantEvicted: true},
		{name: "unknown prefix", payload: "user:42", wantEvicted: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			kc := cache.New[string, KeyInfo]()
			kc.Set(tc.payload, KeyInfo{ID: "k"})
			kc.Set("other", KeyInfo{ID: "o"})
			r, runs, _ := newCountingReloader(nil)

			HandleKeyInvalidation(tc.payload, r, kc)
			if err := r.Wait(context.Background()); err != nil {
				t.Fatalf("Wait: %v", err)
			}

			if got := runs.Load(); got != tc.wantReloads {
				t.Errorf("reloads = %d, want %d", got, tc.wantReloads)
			}
			if _, ok := kc.Get(tc.payload); ok == tc.wantEvicted {
				t.Errorf("payload entry present = %v, want evicted = %v", ok, tc.wantEvicted)
			}
			if _, ok := kc.Get("other"); !ok {
				t.Error("unrelated entry was evicted")
			}
		})
	}
}
