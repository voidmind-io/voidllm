package auth

import (
	"context"
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
// A Reloader is safe for concurrent use.
type Reloader struct {
	load func(ctx context.Context) error
	log  *slog.Logger

	mu      sync.Mutex
	current *reloadCall // load in flight, nil when idle
	next    *reloadCall // queued trailing load, nil when none
}

// NewReloader returns a Reloader that loads all active keys from database into
// keyCache.
func NewReloader(database *db.DB, keyCache *cache.Cache[string, KeyInfo], log *slog.Logger) *Reloader {
	return &Reloader{
		load: func(ctx context.Context) error {
			return LoadKeysIntoCache(ctx, database, keyCache, log)
		},
		log: log,
	}
}

// Trigger schedules a reload and returns immediately. Failures are logged.
func (r *Reloader) Trigger() {
	r.schedule()
}

// Reload schedules a reload and blocks until a load that started after this
// call has finished, or ctx is done. The caller therefore observes every
// database write committed before Reload was called. The returned error is the
// result of that load.
func (r *Reloader) Reload(ctx context.Context) error {
	call := r.schedule()
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
func (r *Reloader) schedule() *reloadCall {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.current == nil {
		r.current = &reloadCall{done: make(chan struct{})}
		go r.run(r.current)
		return r.current
	}
	if r.next == nil {
		r.next = &reloadCall{done: make(chan struct{})}
	}
	return r.next
}

// run executes call and then any queued trailing loads until idle.
func (r *Reloader) run(call *reloadCall) {
	for call != nil {
		ctx, cancel := context.WithTimeout(context.Background(), reloadTimeout)
		call.err = r.load(ctx)
		if call.err != nil {
			r.log.LogAttrs(ctx, slog.LevelError, "key cache reload failed",
				slog.String("error", call.err.Error()),
			)
		}
		cancel()

		r.mu.Lock()
		close(call.done)
		call = r.next
		r.current = call
		r.next = nil
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
