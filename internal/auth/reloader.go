package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/db"
)

// reloadTimeout bounds a single background key cache load.
const reloadTimeout = 30 * time.Second

// reloadCall is one scheduled load. done is closed once the load has finished
// and err holds its result.
type reloadCall struct {
	done chan struct{}
	err  error
}

// Reloader coalesces key cache reloads. At most one load runs at a time and at
// most one more is queued behind it: every trigger that arrives while a load is
// in flight joins the single queued (trailing) load, so a burst of N triggers
// costs at most two loads and no trigger is lost, because the trailing load
// always starts after the last trigger it absorbed.
//
// The load function is LoadKeysIntoCache. Like every full reload it replaces
// the cache with a snapshot of the database, so a key that is not in the
// database is dropped; the Reloader never evicts individual entries.
//
// Every load runs under a context derived from the Reloader's own lifecycle
// context. Stop cancels it, waits for the worker goroutine to exit and turns
// later triggers into no-ops, so no load can outlive the database it reads.
//
// A Reloader is safe for concurrent use. Construct it with NewReloader.
type Reloader struct {
	load func(ctx context.Context) error
	log  *slog.Logger

	// ctx is the parent of every load context; cancel ends it. Both are set
	// once at construction. ctx is a lifecycle scope rather than a request
	// context, which is why it lives in the struct.
	ctx    context.Context
	cancel context.CancelFunc

	// worker tracks the goroutine started by schedule. Add is only called
	// under mu while stopped is false, so it never races with Stop's Wait.
	worker sync.WaitGroup

	mu      sync.Mutex
	current *reloadCall // load in flight, nil when idle
	next    *reloadCall // queued trailing load, nil when none
	stopped bool        // set by Stop; no load is scheduled afterwards
}

// ErrReloaderStopped is returned by Reload after Stop has been called, and to
// callers whose queued load was dropped by Stop.
var ErrReloaderStopped = errors.New("key cache reloader stopped")

// NewReloader returns a Reloader that loads all active keys from database into
// keyCache. Call Stop before closing database.
func NewReloader(database *db.DB, keyCache *cache.Cache[string, KeyInfo], log *slog.Logger) *Reloader {
	return newReloader(func(ctx context.Context) error {
		return LoadKeysIntoCache(ctx, database, keyCache, log)
	}, log)
}

// newReloader returns a Reloader that runs load for every reload.
func newReloader(load func(ctx context.Context) error, log *slog.Logger) *Reloader {
	ctx, cancel := context.WithCancel(context.Background())
	return &Reloader{load: load, log: log, ctx: ctx, cancel: cancel}
}

// Trigger schedules a reload and returns immediately. Failures are logged.
// After Stop it does nothing.
func (r *Reloader) Trigger() {
	r.schedule()
}

// Stop cancels the load in flight, drops the queued one and blocks until the
// worker goroutine has exited. Callers waiting in Reload are released with an
// error. Afterwards Trigger is a no-op and Reload returns ErrReloaderStopped.
// Stop is idempotent and safe to call concurrently.
func (r *Reloader) Stop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()

	r.cancel()
	r.worker.Wait()
}

// Reload schedules a reload and blocks until a load that started after this
// call has finished, or ctx is done. The caller therefore observes every
// database write committed before Reload was called. The returned error is the
// result of that load, or ErrReloaderStopped once Stop has been called.
func (r *Reloader) Reload(ctx context.Context) error {
	call := r.schedule()
	if call == nil {
		return ErrReloaderStopped
	}
	select {
	case <-call.done:
		return call.err
	case <-ctx.Done():
		return fmt.Errorf("wait for key cache reload: %w", ctx.Err())
	}
}

// Wait blocks until every load scheduled before the call has finished, or ctx
// is done. It returns immediately when the Reloader is idle.
func (r *Reloader) Wait(ctx context.Context) error {
	r.mu.Lock()
	last := r.next
	if last == nil {
		last = r.current
	}
	r.mu.Unlock()
	if last == nil {
		return nil
	}
	select {
	case <-last.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for key cache reload: %w", ctx.Err())
	}
}

// schedule registers a trigger and returns the call that will satisfy it: the
// queued trailing load when a load is already running, otherwise a new load.
// It returns nil once the Reloader has been stopped.
func (r *Reloader) schedule() *reloadCall {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.stopped {
		return nil
	}
	if r.current == nil {
		r.current = &reloadCall{done: make(chan struct{})}
		r.worker.Add(1)
		go r.run(r.current)
		return r.current
	}
	if r.next == nil {
		r.next = &reloadCall{done: make(chan struct{})}
	}
	return r.next
}

// run executes call and then any queued trailing loads until idle or stopped.
func (r *Reloader) run(call *reloadCall) {
	defer r.worker.Done()

	for call != nil {
		ctx, cancel := context.WithTimeout(r.ctx, reloadTimeout)
		call.err = r.load(ctx)
		cancel()
		if call.err != nil {
			// A load cut short by Stop is expected and not worth an error.
			level := slog.LevelError
			if r.ctx.Err() != nil {
				level = slog.LevelDebug
			}
			r.log.LogAttrs(context.Background(), level, "key cache reload failed",
				slog.String("error", call.err.Error()),
			)
		}

		r.mu.Lock()
		close(call.done)
		call = r.next
		r.current = call
		r.next = nil
		if call != nil && r.stopped {
			// Stop dropped the queued load: release its waiters.
			call.err = ErrReloaderStopped
			close(call.done)
			r.current = nil
			call = nil
		}
		r.mu.Unlock()
	}
}

// KeyInvalidationReload is the ChannelKeys payload that asks every instance to
// reload its key cache, used for changes that touch many keys (memberships).
const KeyInvalidationReload = "reload"

// KeyInvalidationOrgPrefix and KeyInvalidationTeamPrefix start the ChannelKeys
// payloads published when an organization or team changes; the remainder is
// the entity ID.
const (
	KeyInvalidationOrgPrefix  = "org:"
	KeyInvalidationTeamPrefix = "team:"
)

// HandleKeyInvalidation applies a ChannelKeys pub/sub payload to the local key
// cache. The payloads "reload", "org:<id>" and "team:<id>" trigger a coalesced
// reload through reloader (the cached KeyInfo embeds org, team and user
// limits, and evicting keys would make them fail authentication until the next
// refresh). Any other payload is a key hash and evicts exactly that entry.
func HandleKeyInvalidation(payload string, reloader *Reloader, keyCache *cache.Cache[string, KeyInfo]) {
	if payload == KeyInvalidationReload ||
		strings.HasPrefix(payload, KeyInvalidationOrgPrefix) ||
		strings.HasPrefix(payload, KeyInvalidationTeamPrefix) {
		reloader.Trigger()
		return
	}
	keyCache.Delete(payload)
}
