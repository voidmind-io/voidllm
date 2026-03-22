package shutdown_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/shutdown"
)

func TestNew(t *testing.T) {
	t.Parallel()

	s := shutdown.New()

	if s == nil {
		t.Fatal("New() returned nil")
	}
	if s.Draining() {
		t.Error("Draining() = true immediately after New(), want false")
	}
	if got := s.InFlight(); got != 0 {
		t.Errorf("InFlight() = %d, want 0", got)
	}
	if err := s.ParentCtx().Err(); err != nil {
		t.Errorf("ParentCtx().Err() = %v, want nil", err)
	}
}

func TestTrackStartDone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		starts     int
		dones      int
		wantInFlight int64
	}{
		{
			name:         "single start increments to one",
			starts:       1,
			dones:        0,
			wantInFlight: 1,
		},
		{
			name:         "start then done returns to zero",
			starts:       1,
			dones:        1,
			wantInFlight: 0,
		},
		{
			name:         "three starts increments to three",
			starts:       3,
			dones:        0,
			wantInFlight: 3,
		},
		{
			name:         "three starts two dones leaves one",
			starts:       3,
			dones:        2,
			wantInFlight: 1,
		},
		{
			name:         "equal starts and dones returns to zero",
			starts:       5,
			dones:        5,
			wantInFlight: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := shutdown.New()

			for range tc.starts {
				s.TrackStart()
			}
			for range tc.dones {
				s.TrackDone()
			}

			if got := s.InFlight(); got != tc.wantInFlight {
				t.Errorf("InFlight() = %d, want %d", got, tc.wantInFlight)
			}
		})
	}
}

func TestBeginDrain(t *testing.T) {
	t.Parallel()

	s := shutdown.New()

	if s.Draining() {
		t.Error("Draining() = true before BeginDrain(), want false")
	}

	s.BeginDrain()

	if !s.Draining() {
		t.Error("Draining() = false after BeginDrain(), want true")
	}

	// Calling BeginDrain a second time must be safe and keep the flag set.
	s.BeginDrain()

	if !s.Draining() {
		t.Error("Draining() = false after second BeginDrain(), want true")
	}
}

func TestWaitForDrain_AllDrained(t *testing.T) {
	t.Parallel()

	s := shutdown.New()
	const numRequests = 3

	for range numRequests {
		s.TrackStart()
	}

	// Complete all requests before waiting.
	for range numRequests {
		s.TrackDone()
	}

	drained := s.WaitForDrain(100 * time.Millisecond)
	if !drained {
		t.Error("WaitForDrain() = false, want true when all requests completed")
	}
}

func TestWaitForDrain_ConcurrentDrain(t *testing.T) {
	t.Parallel()

	s := shutdown.New()
	const numRequests = 3

	var started sync.WaitGroup
	started.Add(numRequests)

	for range numRequests {
		s.TrackStart()
		go func() {
			started.Done()
			time.Sleep(10 * time.Millisecond)
			s.TrackDone()
		}()
	}

	// Wait until all goroutines have called TrackStart before draining.
	started.Wait()

	drained := s.WaitForDrain(500 * time.Millisecond)
	if !drained {
		t.Error("WaitForDrain() = false, want true when all requests completed before timeout")
	}
}

func TestWaitForDrain_Timeout(t *testing.T) {
	t.Parallel()

	s := shutdown.New()
	s.TrackStart() // never paired with TrackDone

	drained := s.WaitForDrain(20 * time.Millisecond)
	if drained {
		t.Error("WaitForDrain() = true, want false when a request never completes")
	}

	// Clean up: complete the outstanding request so the internal WaitGroup is
	// consistent. The goroutine spawned by WaitForDrain will also exit cleanly.
	s.TrackDone()
}

func TestCancelInflight(t *testing.T) {
	t.Parallel()

	s := shutdown.New()

	if err := s.ParentCtx().Err(); err != nil {
		t.Fatalf("ParentCtx().Err() = %v before CancelInflight(), want nil", err)
	}

	s.CancelInflight()

	if err := s.ParentCtx().Err(); err != context.Canceled {
		t.Errorf("ParentCtx().Err() = %v after CancelInflight(), want context.Canceled", err)
	}
}

func TestParentCtxPropagation(t *testing.T) {
	t.Parallel()

	s := shutdown.New()

	child, cancel := context.WithCancel(s.ParentCtx())
	defer cancel()

	if err := child.Err(); err != nil {
		t.Fatalf("child.Err() = %v before CancelInflight(), want nil", err)
	}

	s.CancelInflight()

	// The child must be cancelled promptly because the parent was cancelled.
	select {
	case <-child.Done():
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Fatal("child context was not cancelled within 100ms after CancelInflight()")
	}

	if err := child.Err(); err != context.Canceled {
		t.Errorf("child.Err() = %v, want context.Canceled", err)
	}
}
