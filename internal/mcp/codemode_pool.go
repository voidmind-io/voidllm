package mcp

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/fastschema/qjs"
)

// RuntimePool manages a fixed-size pool of pre-warmed QuickJS WASM runtimes
// for Code Mode script execution. Each runtime is acquired for a single
// execution and returned afterward. The pool is safe for concurrent use.
//
// The pool channel acts as a concurrency semaphore: at most size executions
// run in parallel. The sandboxDir is an empty directory mounted as the WASM
// filesystem root, preventing scripts from accessing the real host filesystem.
type RuntimePool struct {
	pool       chan *qjs.Runtime
	size       int
	memLimit   int    // bytes, passed as qjs.Option.MemoryLimit
	timeout    int    // milliseconds, passed as qjs.Option.MaxExecutionTime
	sandboxDir string // empty directory mounted as WASM filesystem root
}

// NewRuntimePool creates a pool of pre-warmed QJS runtimes. Each runtime is
// configured with the given memory limit (in megabytes) and eval timeout.
// An empty sandbox directory is created once and shared across all runtimes as
// the WASM filesystem root to prevent host filesystem access.
// Returns an error if the sandbox directory or any runtime fails to initialize.
func NewRuntimePool(size int, memLimitMB int, timeout time.Duration) (*RuntimePool, error) {
	sandboxDir, err := os.MkdirTemp("", "voidllm-sandbox-*")
	if err != nil {
		return nil, fmt.Errorf("create sandbox directory: %w", err)
	}

	p := &RuntimePool{
		pool:       make(chan *qjs.Runtime, size),
		size:       size,
		memLimit:   memLimitMB * 1024 * 1024,
		timeout:    int(timeout.Milliseconds()),
		sandboxDir: sandboxDir,
	}
	for i := 0; i < size; i++ {
		rt, err := p.newRuntime(context.Background())
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("init runtime %d/%d: %w", i+1, size, err)
		}
		p.pool <- rt
	}
	return p, nil
}

// newRuntime creates and configures a single QJS runtime with the pool's
// memory and execution time limits applied via qjs.Option. The provided
// context is embedded into the runtime; when CloseOnContextDone is true and
// the context is cancelled, wazero terminates the WASM execution automatically.
// Stdout and Stderr are discarded to prevent host process output leakage.
// CWD is set to the pool's empty sandbox directory to prevent filesystem access.
func (p *RuntimePool) newRuntime(ctx context.Context) (*qjs.Runtime, error) {
	rt, err := qjs.New(qjs.Option{
		CWD:                p.sandboxDir,
		Context:            ctx,
		CloseOnContextDone: true,
		MemoryLimit:        p.memLimit,
		MaxExecutionTime:   p.timeout,
		Stdout:             io.Discard,
		Stderr:             io.Discard,
	})
	if err != nil {
		return nil, err
	}
	return rt, nil
}

// Acquire retrieves a slot from the pool, blocking until one is available or
// the context is cancelled. It then creates a fresh runtime bound to the
// provided context so that context cancellation terminates WASM execution via
// wazero's CloseOnContextDone mechanism.
//
// The caller must pass the same context to Release so the replacement runtime
// is created with a fresh background context.
func (p *RuntimePool) Acquire(ctx context.Context) (*qjs.Runtime, error) {
	// Block until a pool slot is free or the caller gives up.
	select {
	case slot := <-p.pool:
		// Discard the slot runtime — it may carry stale JS global state from
		// a previous execution. Create a fresh runtime bound to the caller's
		// context so that cancellation terminates the WASM instance.
		if slot != nil {
			slot.Close()
		}
		rt, err := p.newRuntime(ctx)
		if err != nil {
			// Failed to create the execution runtime. Try to restore the
			// slot with a background runtime so the pool doesn't shrink.
			replacement, replErr := p.newRuntime(context.Background())
			if replErr == nil {
				p.pool <- replacement
			}
			// If replacement also fails, the pool permanently loses one
			// slot — acceptable degradation until next successful Release.
			return nil, fmt.Errorf("create execution runtime: %w", err)
		}
		return rt, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Release returns a pool slot after execution completes. The used runtime is
// always closed (regardless of the healthy flag) because Code Mode always
// discards runtimes after each execution to prevent cross-user JS state
// leakage. A fresh replacement runtime is placed back into the pool to
// maintain the concurrency limit.
func (p *RuntimePool) Release(rt *qjs.Runtime, _ bool) {
	// Always close the used runtime to prevent global state leakage between
	// users. The healthy flag is intentionally ignored here — replacement
	// is unconditional.
	if rt != nil {
		rt.Close()
	}
	fresh, err := p.newRuntime(context.Background())
	if err != nil {
		// Pool shrinks by one — acceptable degradation until next release.
		return
	}
	select {
	case p.pool <- fresh:
	default:
		// Pool is full (should not occur with correct Acquire/Release pairing).
		fresh.Close()
	}
}

// Close drains all idle runtimes from the pool and closes them, releasing
// their associated WASM resources. It also removes the sandbox directory
// created at pool construction. Close must not be called concurrently
// with Acquire or Release.
func (p *RuntimePool) Close() {
	close(p.pool)
	for rt := range p.pool {
		if rt != nil {
			rt.Close()
		}
	}
	if p.sandboxDir != "" {
		os.RemoveAll(p.sandboxDir)
	}
}

// Available returns the number of runtimes currently idle in the pool.
// This value is a snapshot and may change immediately after being read.
func (p *RuntimePool) Available() int {
	return len(p.pool)
}
