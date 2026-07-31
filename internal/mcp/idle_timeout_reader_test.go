package mcp_test

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file is the direct unit-level counterpart of the idle-timeout
// properties forward_streaming_test.go already proves end to end through
// Forward (TestForward_IdleTimeout_NoDataAtAll_StreamEndsAfterIdleWindow,
// TestForward_IdleTimeout_KeepAliveComments_StreamStaysOpenPastIdleWindow):
// idle_timeout_reader.go's own Read must reset its timer on any read that
// returns n > 0, and must NOT reset it for a zero-byte, no-error read —
// permitted by io.Reader's contract, and exactly the shape an upstream
// endlessly returning (0, nil) without actual progress would produce.

// zeroThenSignalReader is an io.ReadCloser whose Read always returns (0,
// nil) — never any bytes, never an error — until closed, at which point
// every blocked and future Read returns io.EOF. It exists to drive
// idleTimeoutReader.Read repeatedly with zero-byte, no-error reads, the one
// case Read's own doc says must NOT reset the timer.
type zeroThenSignalReader struct {
	closed chan struct{}
}

func newZeroThenSignalReader() *zeroThenSignalReader {
	return &zeroThenSignalReader{closed: make(chan struct{})}
}

func (z *zeroThenSignalReader) Read(_ []byte) (int, error) {
	select {
	case <-z.closed:
		return 0, io.EOF
	default:
		return 0, nil
	}
}

func (z *zeroThenSignalReader) Close() error {
	select {
	case <-z.closed:
	default:
		close(z.closed)
	}
	return nil
}

// TestIdleTimeoutReader_ZeroByteReads_DoNotResetTimer verifies that
// continuous (0, nil) reads — a caller that keeps calling Read but never
// makes any actual progress — do NOT push the idle timeout out indefinitely.
// A reader that incorrectly reset on every Read call, including zero-byte
// ones, would let an upstream defeat the idle timeout completely simply by
// returning (0, nil) forever; this test drives the wrapper with exactly that
// pattern and asserts the timeout still fires close to idle, not later.
func TestIdleTimeoutReader_ZeroByteReads_DoNotResetTimer(t *testing.T) {
	t.Parallel()

	const idle = 150 * time.Millisecond

	fired := make(chan struct{})
	var once sync.Once
	var cancel context.CancelFunc = func() {
		once.Do(func() { close(fired) })
	}

	underlying := newZeroThenSignalReader()
	r := mcp.NewIdleTimeoutReader(underlying, idle, cancel)
	t.Cleanup(func() { _ = r.Close() })

	start := time.Now()

	// Drive Read in a tight-ish loop, simulating a caller that keeps getting
	// (0, nil) back — the wrapper itself must be the one deciding not to
	// reset, not merely "no caller happened to trigger a reset".
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 16)
		for {
			select {
			case <-fired:
				return
			default:
			}
			if _, err := r.Read(buf); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	select {
	case <-fired:
		elapsed := time.Since(start)
		if elapsed > 2*time.Second {
			t.Errorf("idle timer fired after %v, want well under 2s for a %v idle window", elapsed, idle)
		}
		if elapsed < idle/2 {
			t.Errorf("idle timer fired after only %v, want at least ~%v", elapsed, idle)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle timeout never fired despite continuous zero-byte, no-error reads — " +
			"a zero-byte read must not reset the timer")
	}

	<-readDone
}

// nByteThenBlockReader is an io.ReadCloser that returns a fixed payload,
// split across calls of at most one byte each (n=1, err=nil), and then
// blocks on unblock. It exists to drive idleTimeoutReader.Read with
// n>0 reads and verify each one DOES reset the timer — the Gegenprobe for
// TestIdleTimeoutReader_ZeroByteReads_DoNotResetTimer above.
type nByteThenBlockReader struct {
	remaining []byte
	unblock   chan struct{}
}

func (n *nByteThenBlockReader) Read(p []byte) (int, error) {
	if len(n.remaining) > 0 {
		p[0] = n.remaining[0]
		n.remaining = n.remaining[1:]
		return 1, nil
	}
	<-n.unblock
	return 0, io.EOF
}

func (n *nByteThenBlockReader) Close() error {
	select {
	case <-n.unblock:
	default:
		close(n.unblock)
	}
	return nil
}

// TestIdleTimeoutReader_NonZeroReads_ResetTimer verifies the Gegenprobe: a
// sequence of small (n=1) but genuinely progressing reads, each spaced well
// under idle, keeps resetting the timer, so the idle timeout does not fire
// merely because no single read was large.
func TestIdleTimeoutReader_NonZeroReads_ResetTimer(t *testing.T) {
	t.Parallel()

	const idle = 200 * time.Millisecond
	const gap = 40 * time.Millisecond // comfortably under idle
	const payloadLen = 6              // 6 * gap > idle: only survives if each read resets the timer

	fired := make(chan struct{})
	var once sync.Once
	var cancel context.CancelFunc = func() {
		once.Do(func() { close(fired) })
	}

	underlying := &nByteThenBlockReader{
		remaining: []byte("abcdef")[:payloadLen],
		unblock:   make(chan struct{}),
	}
	r := mcp.NewIdleTimeoutReader(underlying, idle, cancel)
	t.Cleanup(func() { _ = r.Close() })

	buf := make([]byte, 1)
	var got []byte
	for i := 0; i < payloadLen; i++ {
		n, err := r.Read(buf)
		if err != nil {
			t.Fatalf("Read() #%d error = %v, want nil (idle timer must not have fired yet)", i, err)
		}
		got = append(got, buf[:n]...)
		select {
		case <-fired:
			t.Fatalf("idle timer fired after read #%d, want it reset by every non-zero-byte read", i)
		default:
		}
		time.Sleep(gap)
	}

	if string(got) != "abcdef" {
		t.Errorf("read bytes = %q, want %q", got, "abcdef")
	}
}
