package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
)

// ToolStore persists and retrieves tool schemas from a backing store (typically
// a database). When non-nil, the ToolCache writes through on every fetch whose
// CacheableResult hint (MCP 2026-07-28 §5) is not scoped "private" — a private
// fetch is actively deleted from the store instead, never written — see
// ToolCache.persistListing. The store is also loaded from at startup for
// zero-HTTP-call warm starts.
type ToolStore interface {
	// LoadAll returns all cached tool schemas grouped by server ID.
	LoadAll(ctx context.Context) (map[string][]Tool, error)
	// Save persists the tool schemas for a server, replacing any previous entry.
	// serverID is the database ID of the MCP server.
	Save(ctx context.Context, serverID string, tools []Tool) error
	// Delete removes all cached tool schemas for a server. serverID is the
	// database ID, used because alias-based lookups fail after soft-delete.
	Delete(ctx context.Context, serverID string) error
}

// ToolFetcher retrieves the tool listing from an MCP server identified by
// serverID. Implementations typically create an HTTPTransport, send
// tools/list (or, for legacy upstreams, initialize + tools/list), and return
// the parsed result. HTTPTransport.ListTools already returns the *ToolListing
// shape this type expects, including the x-mcp-header filtering
// FilterHeaderParamTools performs — a ToolFetcher built on top of it (e.g.
// Handler.MakeToolFetcher) can typically return that value unchanged.
type ToolFetcher func(ctx context.Context, serverID string) (*ToolListing, error)

// ToolListing is the result of one upstream tool discovery: the tools
// themselves, the validated x-mcp-header bindings for whichever of those
// tools carry them (keyed by tool name; see FilterHeaderParamTools), and the
// CacheableResult freshness hint the response carried, if any. It is a
// single type, rather than three separate ToolFetcher return values, so that
// adding HeaderParams and Cache in the same change only touched the
// ToolFetcher signature once.
//
// entryFor and RefreshServer turn Cache into a cacheEntry's ttl/neverExpires
// fields via ToolCache.resolveTTL, and into the decision of whether a fetch
// is written through to the store at all (see ToolCache.persistListing).
type ToolListing struct {
	// Tools is the upstream's tool array, already filtered by
	// FilterHeaderParamTools when produced by HTTPTransport.ListTools: every
	// tool here has either no x-mcp-header annotation or one that satisfies
	// every MCP 2026-07-28 §4.3 constraint.
	Tools []Tool
	// HeaderParams holds each kept tool's validated x-mcp-header bindings,
	// indexed by tool name. A tool absent from this map declared no
	// annotation at all.
	HeaderParams map[string][]HeaderParam
	// Cache is the CacheableResult hint (MCP 2026-07-28 §5) the tools/list
	// response carried, if any. See the type doc for why ToolCache does not
	// yet read this field.
	Cache CacheHint
}

// cacheEntry holds the cached tools and validated x-mcp-header bindings for
// a single MCP server.
//
// A *cacheEntry is immutable once published to ToolCache.entries: every
// mutation (entryFor's fetch-on-miss path, RefreshServer, SetTools,
// LoadFromStore) constructs a brand-new *cacheEntry and replaces the pointer
// stored under tc.mu — none of them ever mutates a field of an
// already-published *cacheEntry in place. This was already true before this
// doc existed; it is written down here because entryFor now relies on it
// explicitly, returning the live pointer itself (not a copy) to callers that
// read its fields after tc.mu has already been released.
type cacheEntry struct {
	tools        []Tool
	headerParams map[string][]HeaderParam
	fetchedAt    time.Time
	// ttl is this entry's resolved freshness window: the upstream's
	// CacheableResult ttlMs hint (MCP 2026-07-28 §5), reinterpreted by
	// ToolCache.resolveTTL, when the fetch that produced this entry carried
	// one, or ToolCache.maxAge otherwise (the fallback for any upstream that
	// gave no hint at all — in practice every legacy MCP server today). Only
	// meaningful when neverExpires is false.
	ttl time.Duration
	// neverExpires is true only when no upstream hint applied AND the
	// cache's own maxAge is zero — the pre-existing "entries never expire"
	// semantics of NewToolCache, preserved unchanged. An upstream ttlMs of 0
	// is a different thing entirely: it means "immediately stale" per §5, and
	// deliberately never sets this field. This ambiguity between "no hint,
	// zero maxAge" and "an explicit hint of zero" is exactly why
	// CacheHint.TTLMsSet exists — without it, resolveTTL could not tell the
	// two apart from TTLMs alone.
	neverExpires bool
	// scope is the CacheableResult cacheScope the fetch that produced this
	// entry carried: CacheScopePublic, CacheScopePrivate, or "" when the
	// upstream gave no hint at all (treated the same as CacheScopePublic for
	// persistence purposes — see ToolCache.persistListing).
	scope string
	// generation is the value of ToolCache.generations[serverID] at the
	// moment fetchAndPublish published this entry — always equal to the
	// startGen fetchAndPublish captured before fetching, since it only ever
	// publishes when tc.generations[serverID] still equals startGen (see
	// that method's own doc). entryFor compares this against the CURRENT
	// tc.generations[serverID] before handing a freshly fetched entry back
	// to its own caller, closing a window fetchAndPublish's own generation
	// check does not: an Invalidate or InvalidateWithStore that lands AFTER
	// fetchAndPublish's publish step but before (or during) its own,
	// separately-locked store write bumps tc.generations[serverID] without
	// touching this already-published entry's generation field, so the
	// mismatch is exactly what lets entryFor detect and discard it — instead
	// of handing a superseded entry to a caller that only just joined the
	// singleflight fetch — the same way it already discards a fetch
	// fetchAndPublish itself never got to publish at all.
	generation uint64
}

// isFresh reports whether e is still within its resolved freshness window.
// neverExpires entries are always fresh; otherwise e is fresh when less than
// e.ttl has elapsed since e.fetchedAt. This is the same fresh/stale decision
// ToolCache.isFresh used to make by consulting ToolCache.maxAge directly; it
// now lives on the entry itself because ttl is resolved per fetch (see
// ToolCache.resolveTTL), not fixed for the whole cache.
func (e *cacheEntry) isFresh() bool {
	if e.neverExpires {
		return true
	}
	return time.Since(e.fetchedAt) < e.ttl
}

// ToolCache maintains a thread-safe cache of tool schemas from upstream MCP
// servers. Entries are populated lazily on first access and automatically
// refreshed when older than maxAge.
//
// ToolCache caches tools/list results exclusively. No entry is ever
// constructed from an MRTR retry (a request carrying inputResponses or
// requestState) — MCP 2026-07-28 §5 forbids caching those. This holds
// vacuously today: entryFor and RefreshServer only ever call fetcher, which
// only ever issues tools/list, and ClientDialect.Parse already rejects a
// resultType of "input_required" before a Result reaches this package.
// Anyone adding a second method type to this cache must re-check this
// assumption.
type ToolCache struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry // keyed by server ID
	// generations counts, per server ID, how many times Invalidate or
	// InvalidateWithStore has discarded that server's entry. It is read and
	// written under mu, the same lock that guards entries — see
	// RefreshServer's own doc for why this is the compare-and-swap
	// RefreshServer needs to avoid publishing a fetch that started before an
	// invalidation superseded it. A server ID absent from this map reads as
	// generation 0, Go's ordinary zero-value-on-miss map semantics; no entry
	// is ever pre-populated for a server that has not yet been invalidated.
	generations map[string]uint64
	fetcher     ToolFetcher
	maxAge      time.Duration
	store       ToolStore // optional, nil for pure in-memory
	// storeMu serializes every write RefreshServer and InvalidateWithStore
	// make to tc.store for the same serverID, without ever being held during
	// an upstream fetch (unlike tc.mu, which entryFor's fetch-on-miss path
	// does hold across the fetch, by design — see entryFor's own doc). It
	// exists solely to close the race RefreshServer's own doc describes:
	// tc.mu is only ever held long enough to check tc.generations and publish
	// a *cacheEntry, then released BEFORE the store write, because the store
	// write is I/O and I/O has no business running under the lock that guards
	// the hot GetTools/HeaderParams path. Without storeMu, an
	// InvalidateWithStore landing in that gap could run its own store.Delete
	// concurrently with — and in either order relative to — RefreshServer's
	// store.Save, so a Save that happens to commit after the Delete would
	// resurrect a listing InvalidateWithStore just removed. storeMu forces
	// RefreshServer's persistListing call and InvalidateWithStore's
	// store.Delete call into one total order per serverID; combined with
	// RefreshServer re-checking tc.generations while holding storeMu (see
	// RefreshServer's own doc), whichever of the two entered storeMu first
	// determines the final state, and it is always the more recent one: if
	// InvalidateWithStore's generation bump (which always happens before it
	// even attempts to acquire storeMu) is visible by the time RefreshServer
	// gets storeMu, RefreshServer skips its write entirely; otherwise
	// RefreshServer's write completes and is unconditionally overwritten by
	// InvalidateWithStore's Delete once it acquires storeMu afterward. Only
	// these two call sites touch storeMu — GetTools, HeaderParams, and every
	// other read never do, so it adds no contention to the hot path.
	storeMu sync.Mutex
	// sf deduplicates concurrent upstream fetches per server ID: entryFor's
	// fetch-on-miss path used to hold tc.mu (a full write Lock) across the
	// whole upstream round-trip specifically to get this deduplication for
	// free from mutual exclusion — see fetchAndPublish's own doc for why that
	// no longer works once a fetch can span multiple tools/list pages.
	// sharedFetch's tc.sf.DoChan call, keyed by serverID, gives the same "at
	// most one fetch in flight per server ID" guarantee without ever holding
	// tc.mu for the fetch's duration: a slow upstream for one server no
	// longer blocks GetTools for every other server. entryFor and
	// RefreshServer both call sharedFetch — the SAME key for the SAME
	// serverID — so a forced RefreshServer and an ordinary cache-miss fetch
	// can never run concurrently for one server; see sharedFetch's and
	// RefreshServer's own docs. The zero Group is ready to use, so no
	// constructor needs to initialize this field.
	sf singleflight.Group
	// fetchSem bounds how many upstream tools/list fetches this cache will
	// run concurrently, across every server ID it manages, to
	// maxConcurrentToolsListFetches — see that constant's own doc for why.
	// It is acquired inside sharedFetch's singleflight callback, with the
	// same bounded fetchCtx the fetch itself runs under, so time spent
	// waiting for a free slot counts against toolsListFetchTimeout exactly
	// like time spent waiting on the upstream itself — a caller blocked on
	// this semaphore for the whole budget fails the same way a caller
	// blocked on a slow upstream would, rather than waiting unboundedly for
	// a slot that never frees up.
	fetchSem *semaphore.Weighted
	// fetchTimeout is the production toolsListFetchTimeout by default;
	// tests may override it (see export_test.go) to exercise the timeout
	// path without waiting out the real 2-minute budget. Stored as
	// nanoseconds in an atomic.Int64, not a plain time.Duration: the
	// test-only setter (SetFetchTimeoutForTest) and sharedFetch's own
	// production read of it run on different goroutines with no other
	// synchronization between the two, which a plain field would make a
	// data race under -race the moment a test overrides it concurrently
	// with a fetch already in flight.
	fetchTimeout atomic.Int64
	// onChange, when installed via SetOnChange, is called whenever a
	// server's cached tool listing changes structurally — see SetOnChange's
	// own doc for exactly which mutations qualify and the guarantees around
	// when and how it is called. atomic.Pointer, not a plain field guarded
	// by tc.mu: every call site fires it AFTER already releasing tc.mu (see
	// Invalidate, InvalidateWithStore, and fetchAndPublish), so reading it
	// under the same lock it is written under is neither necessary nor
	// desirable — SetOnChange must be safe to call concurrently with every
	// other ToolCache method, including while a fetch is calling the
	// previously-installed hook.
	onChange atomic.Pointer[func(serverID string)]
}

// NewToolCache creates a ToolCache that uses fetcher to retrieve tool schemas
// and considers entries stale after maxAge. A maxAge of zero means entries
// never expire automatically.
func NewToolCache(fetcher ToolFetcher, maxAge time.Duration) *ToolCache {
	tc := &ToolCache{
		entries:     make(map[string]*cacheEntry),
		generations: make(map[string]uint64),
		fetcher:     fetcher,
		maxAge:      maxAge,
		fetchSem:    semaphore.NewWeighted(maxConcurrentToolsListFetches),
	}
	tc.fetchTimeout.Store(int64(toolsListFetchTimeout))
	return tc
}

// NewPersistentToolCache creates a ToolCache backed by a persistent store.
// Tools are written through to the store on every fetch whose CacheableResult
// hint (MCP 2026-07-28 §5) does not mark the listing "private" — see
// ToolCache.persistListing — and can be loaded from the store at startup via
// LoadFromStore.
func NewPersistentToolCache(fetcher ToolFetcher, maxAge time.Duration, store ToolStore) *ToolCache {
	tc := &ToolCache{
		entries:     make(map[string]*cacheEntry),
		generations: make(map[string]uint64),
		fetcher:     fetcher,
		maxAge:      maxAge,
		store:       store,
		fetchSem:    semaphore.NewWeighted(maxConcurrentToolsListFetches),
	}
	tc.fetchTimeout.Store(int64(toolsListFetchTimeout))
	return tc
}

// SetOnChange installs fn to be called whenever a server's cached tool
// listing changes structurally:
//
//   - Invalidate or InvalidateWithStore discards it outright (an admin
//     mutation, or ListenManager reacting to an upstream's own
//     notifications/tools/list_changed — see NewListenManager's onToolsChanged
//     parameter, which app.go wires straight to Invalidate).
//   - fetchAndPublish republishes a listing for serverID whose tool names or
//     InputSchema bytes differ from the immediately-preceding entry — see
//     toolsListingChanged. An ordinary TTL-driven refetch that returns an
//     unchanged listing does NOT fire fn: most upstreams' tools rarely
//     change, and firing on every routine refresh would turn this into
//     noise no different from polling.
//
// fn is called synchronously, on whichever goroutine triggered the change —
// an admin mutation's own request goroutine for Invalidate/
// InvalidateWithStore, or whichever goroutine's GetTools/RefreshServer call
// happened to be singleflight's leader for a republish — and always AFTER
// this cache has released every lock of its own (tc.mu and, where
// applicable, tc.storeMu): fn must not block for long, but it is free to
// call back into this same ToolCache without deadlocking. A nil fn (the
// default) disables the callback entirely. Safe to call concurrently with
// every other ToolCache method.
//
// The only production caller wires this to a Code Mode *mcp.Server's own
// NotifyToolsListChanged, scoped to the changed serverID (internal/app
// wiring, code_mode.go) — the trigger side of this package's
// subscriptions/listen support (subscriptions.go).
func (tc *ToolCache) SetOnChange(fn func(serverID string)) {
	if fn == nil {
		tc.onChange.Store(nil)
		return
	}
	tc.onChange.Store(&fn)
}

// fireOnChange invokes the installed onChange hook for serverID, if any.
// Callers must never hold tc.mu or tc.storeMu when calling this.
func (tc *ToolCache) fireOnChange(serverID string) {
	if hook := tc.onChange.Load(); hook != nil {
		(*hook)(serverID)
	}
}

// toolsListingChanged reports whether newEntry's tool listing differs from
// oldEntry's — by tool name set and, for each name present in both, its
// InputSchema bytes — the comparison fetchAndPublish uses to decide whether
// a republished listing is "materially different" enough to fire the
// onChange hook (see SetOnChange's own doc): an identical refetch (the
// common case — most upstreams' tools rarely change) must not trigger a
// subscriptions/listen notification for every ordinary TTL-driven refresh.
//
// oldEntry == nil — this server's very first published entry, with no
// predecessor to compare against — is always reported as changed: there is
// no meaningful "unchanged" for a listing that did not previously exist.
func toolsListingChanged(oldEntry, newEntry *cacheEntry) bool {
	if oldEntry == nil {
		return true
	}
	if len(oldEntry.tools) != len(newEntry.tools) {
		return true
	}
	oldByName := make(map[string][]byte, len(oldEntry.tools))
	for _, t := range oldEntry.tools {
		oldByName[t.Name] = t.InputSchema
	}
	for _, t := range newEntry.tools {
		oldSchema, ok := oldByName[t.Name]
		if !ok || !bytes.Equal(oldSchema, t.InputSchema) {
			return true
		}
	}
	return false
}

// LoadFromStore populates the in-memory cache from the backing store.
// Call once at startup before serving requests. Returns nil if the store
// is nil or empty.
func (tc *ToolCache) LoadFromStore(ctx context.Context) error {
	if tc.store == nil {
		return nil
	}
	all, err := tc.store.LoadAll(ctx)
	if err != nil {
		return err
	}
	tc.mu.Lock()
	defer tc.mu.Unlock()
	for serverID, tools := range all {
		// Set fetchedAt to zero so entries loaded from the DB are considered
		// stale on first access, regardless of maxAge. This ensures tools are
		// refreshed from upstream on first access after startup, while still
		// providing immediate availability for TypeScript type generation and
		// list_servers tool counts in the meantime.
		//
		// neverExpires is always false here, even when tc.maxAge == 0 — this is
		// deliberately NOT the same fallback resolveTTL's no-hint branch uses
		// for a fetch that actually reached the upstream. ToolStore persists
		// only []Tool (see dbToolStore.Save / ToolStore.LoadAll): a DB-loaded
		// entry carries neither the x-mcp-header bindings a fetch would have
		// populated in headerParams nor the CacheableResult hint that would
		// have justified caching it at all. Marking it neverExpires would let
		// it stand in as authoritative forever whenever maxAge == 0 — a tool
		// annotated with x-mcp-header (MCP 2026-07-28 §4.3) would then be
		// called without its required Mcp-Param-* mirror header, silently and
		// permanently, since nothing would ever trigger a refetch to repopulate
		// headerParams. Setting ttl to tc.maxAge with neverExpires forced false
		// means isFresh() compares time.Since(zero-time) against ttl, which is
		// always stale (zero-time is decades in the past) regardless of
		// maxAge's value, so the very next access always refetches and
		// replaces this placeholder with a real, fully-populated entry.
		tc.entries[serverID] = &cacheEntry{
			tools:        tools,
			fetchedAt:    time.Time{},
			ttl:          tc.maxAge,
			neverExpires: false,
		}
	}
	return nil
}

// maxUpstreamToolTTL caps the freshness window resolveTTL derives from an
// upstream's ttlMs hint. MCP 2026-07-28 §5 explicitly calls ttlMs a
// freshness hint, not a guarantee — without a ceiling, an upstream sending
// an absurdly large ttlMs could effectively freeze an entry forever, so a
// tool the upstream has since removed would never disappear from our cache.
// As a side effect, this cap also keeps resolveTTL's
// milliseconds-to-time.Duration conversion from overflowing for a ttlMs
// close to math.MaxInt64.
const maxUpstreamToolTTL = 24 * time.Hour

// minToolFetchInterval is a floor under any upstream-supplied ttlMs, and the
// single deliberate deviation from MCP 2026-07-28 §5 in this whole cache.
// Code Mode calls GetTools once per incoming request; without a floor, an
// upstream that DOES implement CacheableResult (see
// dialect2026Client.parseCacheHint — at least one of ttlMs/cacheScope
// present on the wire) reporting ttlMs: 0 — explicitly, or implicitly by
// omitting ttlMs while still setting cacheScope (§5 treats a missing ttlMs
// WITHIN a present hint as 0, not as "no hint") — or any ttlMs entryFor
// rounds well below a second, would turn every incoming request into an
// upstream tools/list call — an amplifier whose trigger is entirely under
// that same upstream's control. §5 already requires jitter and backoff from
// any client that polls on a TTL; this floor is the non-polling equivalent
// of that safeguard for a cache that instead re-fetches lazily, on access.
//
// An upstream that implements CacheableResult not at all — omitting BOTH
// ttlMs and cacheScope, true of nearly every MCP server as of this
// revision's release — never reaches this floor at all: parseCacheHint
// reports no hint (CacheHint.TTLMsSet false) for that response, and
// resolveTTL's no-hint branch falls back to this cache's own configured
// maxAge instead, exactly as it did before CacheableResult existed.
const minToolFetchInterval = time.Second

// resolveTTL turns hint — the CacheableResult ttlMs/cacheScope pair a
// tools/list response carried, per MCP 2026-07-28 §5 — into the ttl and
// neverExpires a cacheEntry should be constructed with. This is the single
// place that interprets a CacheHint into cache freshness.
//
// When hint carries no usable ttlMs (hint.TTLMsSet is false — see that
// field's doc for why this is not the same as TTLMs == 0), the cache falls
// back to its own configured maxAge, with neverExpires set exactly when
// maxAge is zero — the pre-existing behavior of every entry before ttlMs
// hints existed. Otherwise ttl is hint.TTLMs milliseconds, clamped to
// [minToolFetchInterval, maxUpstreamToolTTL], and neverExpires is always
// false: an upstream that opts into CacheableResult never gets the "cache
// forever" treatment, even at a very large or very small ttlMs.
func (tc *ToolCache) resolveTTL(hint CacheHint) (ttl time.Duration, neverExpires bool) {
	if !hint.TTLMsSet {
		return tc.maxAge, tc.maxAge == 0
	}
	minMs := int64(minToolFetchInterval / time.Millisecond)
	maxMs := int64(maxUpstreamToolTTL / time.Millisecond)
	ms := hint.TTLMs
	if ms < minMs {
		ms = minMs
	}
	if ms > maxMs {
		ms = maxMs
	}
	return time.Duration(ms) * time.Millisecond, false
}

// persistListing writes listing's tools through to tc.store on behalf of
// serverID, honoring listing.Cache (MCP 2026-07-28 §5):
//
//   - CacheScopePrivate: the tools are NOT saved. Any previously persisted
//     copy for serverID is actively deleted instead. A left-behind copy
//     would otherwise outlive the current process and be lifted straight
//     back into memory by the next LoadFromStore at startup — indistinguishable
//     from a fresh, legitimate public entry. This is also what makes an
//     upstream that newly starts reporting "private" take effect on disk: any
//     previously persisted (from before the upstream reported a scope at all)
//     copy is removed on the very next fetch. The in-memory cacheEntry is
//     still populated as usual by the caller — only persistence is skipped;
//     ListTools always authenticates to the upstream with one server-wide
//     credential, so the authorization context behind an in-memory entry is
//     the same for every caller regardless of scope.
//   - A hint WAS offered (listing.Cache.TTLMsSet) but its Scope is anything
//     other than CacheScopePublic — meaning either the upstream explicitly
//     said "private" (handled above) or it set ttlMs/cacheScope at all
//     without a recognizable "public"/"private" value, which parseCacheHint
//     leaves as Scope == "": NOT saved, and — unlike the private case —
//     nothing is deleted either. §5 grants permission to store a result and
//     serve it to ANY caller only for cacheScope: "public" ("Jeder Client
//     […] DARF sie speichern und an beliebige Nutzer ausliefern"); an
//     upstream that opted into CacheableResult at all but then sent no valid
//     public grant has given VoidLLM no basis to treat this listing as safe
//     to share across every organization and key that can reach this shared,
//     server-wide tool cache. This is deliberately distinct from the next
//     case: an upstream that implements CacheableResult has said SOMETHING,
//     even if unusable, so persistListing takes it as "unproven", not as the
//     "said nothing, so persist as always" default the next case is.
//   - No hint at all (TTLMsSet is false — every legacy upstream today, and a
//     modern one that has simply not implemented CacheableResult yet):
//     saved exactly as before this fix. Treating "no hint" as permission to
//     persist, unlike an unusable hint, is deliberate and unchanged: refusing
//     it instead would evict every legacy upstream's cache from the store on
//     every single fetch, for a distinction §5 never asked this package to
//     draw in the first place — CacheableResult is itself a MCP 2026-07-28
//     concept a legacy response has no way to express an opinion on. This
//     preserves this function's era-neutrality (see resolveTTL's own doc for
//     why that is load-bearing): the branch above reads only
//     listing.Cache.TTLMsSet and .Scope, both already era-neutral fields on
//     CacheHint, never the dialect or protocol version that produced them.
//
// Called only when tc.store is non-nil. A Save failure is swallowed, as it
// always was before this method existed — it is not new behavior introduced
// here. A Delete failure is different: unlike an upstream error, it
// originates entirely within VoidLLM's own storage layer, so it is safe to
// log, and is logged at Warn with the server ID and error text.
//
// It reports whether it actually attempted AND SUCCEEDED at a store.Save
// call, as opposed to a Delete, a failed Save, or neither. This is not merely
// "did this method take the Save branch": fetchAndPublish's own post-Save
// generation recheck (see its doc) exists to delete a value Save just wrote
// to disk if a concurrent invalidation superseded it in the meantime — and
// there is nothing on disk for that recheck to worry about undoing when Save
// itself never actually got a value there in the first place. Reporting
// saved true for a failed Save (the previous behavior, which discarded
// store.Save's error entirely) would make fetchAndPublish run that recheck,
// and potentially a store.Delete, against a serverID whose store entry Save
// never touched — harmless in isolation (Delete of a value that also never
// changed is a no-op on the correct row), but still work performed, and a
// Warn-level Delete-failure log line potentially issued, on the strength of
// a Save this method already knows failed. A Save failure is itself only
// logged, not escalated to the caller — it was silently swallowed before
// this method's saved-reporting existed at all, and staying silent here is
// not new behavior this change introduces.
func (tc *ToolCache) persistListing(ctx context.Context, serverID string, listing *ToolListing) (saved bool) {
	if listing.Cache.Scope == CacheScopePrivate {
		if err := tc.store.Delete(ctx, serverID); err != nil {
			slog.Default().LogAttrs(ctx, slog.LevelWarn, "mcp: failed to delete private tool listing from store",
				slog.String("server_id", serverID),
				slog.String("error", err.Error()))
		}
		return false
	}
	if listing.Cache.TTLMsSet && listing.Cache.Scope != CacheScopePublic {
		// A hint was offered but did not grant public-scope sharing — see the
		// doc above. Neither saved nor deleted: this is not the "the upstream
		// said private" case (handled above), just an absence of proof this
		// listing is safe to persist and hand to any caller.
		return false
	}
	if err := tc.store.Save(ctx, serverID, listing.Tools); err != nil {
		slog.Default().LogAttrs(ctx, slog.LevelWarn, "mcp: failed to save tool listing to store",
			slog.String("server_id", serverID),
			slog.String("error", err.Error()))
		return false
	}
	return true
}

// copyTools returns a deep copy of the given slice so callers cannot mutate
// the cache's internal state. A plain slice `copy` (the previous
// implementation) only copies each Tool's header fields — InputSchema is a
// []byte-backed JSONSchema, so the copy and the cached original would still
// alias the same backing array, letting a caller that mutates the returned
// InputSchema in place corrupt the cache without ever taking tc.mu, and race
// with any concurrent reader (docs/mcp-v2.md, FIX 3). Tool.clone (already
// used by Server.Tools for exactly this reason) copies the InputSchema bytes
// too, so it is reused here instead of a second copying implementation.
func copyTools(src []Tool) []Tool {
	if src == nil {
		return nil
	}
	dst := make([]Tool, len(src))
	for i, t := range src {
		dst[i] = t.clone()
	}
	return dst
}

// copyHeaderParams returns a deep copy of src — including each HeaderParam's
// Path slice, not merely the outer slice — so a caller can never mutate a
// cacheEntry's bindings through the returned value. This mirrors copyTools'
// reasoning for InputSchema (see that function's doc): Path is a
// []string-backed field, so a shallow copy of []HeaderParam would still
// alias the cached entry's own backing array.
func copyHeaderParams(src []HeaderParam) []HeaderParam {
	if src == nil {
		return nil
	}
	dst := make([]HeaderParam, len(src))
	for i, p := range src {
		dst[i] = p
		if p.Path != nil {
			dst[i].Path = append([]string(nil), p.Path...)
		}
	}
	return dst
}

// fetchAndPublish fetches serverID's tool listing from tc.fetcher — which
// may itself issue several upstream tools/list requests to follow
// pagination (HTTPTransport.ListTools) — and, unless a concurrent
// invalidation has superseded it, publishes the result to tc.entries and
// writes it through to tc.store. It is the single place that generation-
// guarded publish+persist logic lives; entryFor (via tc.sf.Do) and
// RefreshServer are its only two callers, and both need the identical
// guarantee: an Invalidate or InvalidateWithStore that runs while this fetch
// is still in flight (e.g. triggered by an admin rotating this server's
// credential) must never be silently undone by this fetch publishing, or
// persisting, a result it fetched under the state that invalidation already
// superseded. See tc.generations' own doc for the counter this compares.
//
// The fetch (tc.fetcher) itself deliberately runs outside tc.mu — an
// upstream round-trip, now possibly several of them across nextCursor pages,
// has no business holding the cache lock, or blocking GetTools/HeaderParams
// for every other server, for its whole duration.
//
// A (nil, nil) return means the fetch itself succeeded but was discarded:
// the generation check found a concurrent invalidation had already
// superseded the state this fetch started under. RefreshServer, which does
// not need the fetched data back — only whether an error occurred — takes
// that literally and returns nil. entryFor cannot: its own callers (GetTools,
// HeaderParams) need a *cacheEntry to hand back right now, and handing back
// the very entry this method just decided not to publish would defeat the
// invalidation that discarded it — see entryFor's own doc for how it instead
// retries from the top when it sees a nil entry here.
//
// A plain Invalidate call — unlike InvalidateWithStore — never touches
// tc.storeMu or tc.store at all: it only bumps tc.generations and clears
// tc.entries under tc.mu (see its own doc). That makes it, deliberately, the
// one operation that can run fully concurrently with persistListing's own
// store.Save call below: Save is I/O and can take a non-trivial amount of
// time, and a plain Invalidate landing anywhere during that window bumps the
// generation without ever being blocked by, or blocking, the write in
// progress. The "stillCurrent" check taken BEFORE calling persistListing
// only catches an invalidation that landed before Save was even attempted;
// one that lands WHILE Save is running is caught by the second,
// post-persistListing recheck below instead — without it, a Save that
// started under a since-superseded generation could still land on disk
// after Invalidate believed it had already cleared this server's state,
// leaving a stale listing on disk that a later LoadFromStore would resurrect
// as if it were still current.
//
// This is safe from ever discarding a NEWER, legitimately-saved listing for
// two independent reasons that both have to hold, and do: first, tc.sf (see
// its own doc) guarantees at most one fetchAndPublish call is ever running
// for a given serverID at a time, so there is no OTHER fetchAndPublish call
// for this same serverID whose Save could race this one's recheck-and-delete
// in real time; second, the entire persistListing call and the recheck that
// follows it below run inside ONE continuous tc.storeMu critical section —
// never released and reacquired in between — and InvalidateWithStore's own
// store.Delete also takes tc.storeMu, so neither it nor any other store
// write for this serverID can interleave between this call's own Save and
// its recheck. Whatever tc.generations[serverID] reads as by the time the
// recheck runs was therefore already true at the moment Save returned, not
// something that could still change before the conditional Delete below
// runs.
func (tc *ToolCache) fetchAndPublish(ctx context.Context, serverID string) (*cacheEntry, error) {
	tc.mu.RLock()
	startGen := tc.generations[serverID]
	tc.mu.RUnlock()

	listing, err := tc.fetcher(ctx, serverID)
	if err != nil {
		return nil, err
	}

	ttl, neverExpires := tc.resolveTTL(listing.Cache)
	entry := &cacheEntry{
		tools:        listing.Tools,
		headerParams: listing.HeaderParams,
		fetchedAt:    time.Now(),
		ttl:          ttl,
		neverExpires: neverExpires,
		scope:        listing.Cache.Scope,
		generation:   startGen,
	}

	tc.mu.Lock()
	if tc.generations[serverID] != startGen {
		tc.mu.Unlock()
		slog.Default().LogAttrs(ctx, slog.LevelDebug, "mcp: discarding fetch superseded by a concurrent invalidation",
			slog.String("server_id", serverID))
		return nil, nil
	}
	oldEntry := tc.entries[serverID]
	tc.entries[serverID] = entry
	tc.mu.Unlock()

	if toolsListingChanged(oldEntry, entry) {
		// Fired outside tc.mu (already released above) and before the store
		// write below — see SetOnChange's own doc for why this must never run
		// under any lock this cache holds.
		tc.fireOnChange(serverID)
	}

	if tc.store != nil {
		// See tc.storeMu's own doc for why the store write happens under a
		// second, dedicated lock rather than tc.mu, and why it re-checks the
		// generation again here rather than trusting the check above alone.
		// This whole block — the pre-check, persistListing's own Save, and
		// the post-Save recheck-and-delete below — runs inside ONE continuous
		// storeMu.Lock/Unlock pair: see this method's own doc for why never
		// releasing storeMu in between is what makes the post-Save recheck
		// safe from ever discarding a newer, legitimately-saved listing.
		tc.storeMu.Lock()
		tc.mu.RLock()
		stillCurrent := tc.generations[serverID] == startGen
		tc.mu.RUnlock()
		if stillCurrent {
			if tc.persistListing(ctx, serverID, listing) {
				// persistListing actually saved — Save returned nil, not
				// merely "this method took the Save branch" (see its own
				// doc for why a failed Save must not reach this branch at
				// all) — so there is now a value on disk that a plain
				// Invalidate landing DURING that Save call (see this
				// method's own doc for why that race is possible at all)
				// could have already superseded by the time Save returned.
				// Re-check the generation one more time, still under the
				// same storeMu critical section, and delete whatever Save
				// just wrote if it has: leaving it would let a later
				// LoadFromStore resurrect a listing this generation no
				// longer represents.
				tc.mu.RLock()
				superseded := tc.generations[serverID] != startGen
				tc.mu.RUnlock()
				if superseded {
					// A dedicated, short-lived context — never ctx (fetchCtx,
					// bounded by tc.fetchTimeout and already possibly close to
					// its own deadline by the time Save returned, or even past
					// it under an adverse enough schedule) — because this
					// cleanup's job is to undo a write this method itself just
					// made; it must run to completion regardless of how much
					// of the fetch's own budget is left, exactly as it must
					// run even if the fetch context has already expired.
					cleanupCtx, cancel := context.WithTimeout(context.Background(), toolCacheCleanupDeleteTimeout)
					err := tc.store.Delete(cleanupCtx, serverID)
					cancel()
					if err != nil {
						slog.Default().LogAttrs(ctx, slog.LevelWarn,
							"mcp: failed to delete tool listing superseded by a concurrent invalidation during store save",
							slog.String("server_id", serverID),
							slog.String("error", err.Error()))
					}
				}
			}
		} else {
			slog.Default().LogAttrs(ctx, slog.LevelDebug, "mcp: skipping tool store write superseded by a concurrent invalidation",
				slog.String("server_id", serverID))
		}
		tc.storeMu.Unlock()
	}
	return entry, nil
}

// toolsListFetchTimeout bounds the shared upstream fetch sharedFetch runs on
// behalf of every caller currently waiting on the same server ID. See
// sharedFetch's own doc for why the fetch is bounded by this fixed duration
// rather than by whichever caller happened to start it. This is the default
// every ToolCache constructor sets ToolCache.fetchTimeout to; production
// code never overrides it, but tests may (see export_test.go) to exercise
// the timeout path without waiting out the real 2 minutes.
const toolsListFetchTimeout = 2 * time.Minute

// toolCacheCleanupDeleteTimeout bounds the dedicated context fetchAndPublish's
// post-Save cleanup uses for its store.Delete call — deliberately its own
// context.WithTimeout(context.Background(), toolCacheCleanupDeleteTimeout),
// never the fetch's own ctx (fetchCtx, bounded by toolsListFetchTimeout and
// possibly already near, or past, its own deadline by the time Save
// returned). That cleanup exists to undo a write fetchAndPublish itself just
// made once a concurrent invalidation is found to have superseded it — see
// fetchAndPublish's own doc — and it must run to completion regardless of how
// much of the fetch's own budget remains, or whether it has already expired
// entirely, since an expired fetch context is exactly the adverse scheduling
// window in which this cleanup is most needed: skipping it would leave a
// stale listing on disk for a later LoadFromStore to resurrect. Five seconds
// is ample for a single-row delete against either supported backend (SQLite,
// PostgreSQL) without risking this cleanup itself hanging indefinitely
// against a genuinely wedged store.
const toolCacheCleanupDeleteTimeout = 5 * time.Second

// maxConcurrentToolsListFetches bounds how many upstream tools/list fetches
// this cache will ever run at once, across every server ID it manages —
// acquired via ToolCache.fetchSem inside sharedFetch's singleflight callback,
// so it applies regardless of how many DISTINCT server IDs happen to have a
// stale or missing entry at the same moment. Before this existed, a cache
// miss for every server registered against one VoidLLM deployment at once
// (e.g. right after process startup, before LoadFromStore's placeholders —
// see its own doc — have been refreshed) could open one upstream connection
// per server simultaneously, with no ceiling at all: tc.sf only deduplicates
// concurrent callers for the SAME server ID, never bounds how many DIFFERENT
// server IDs fetch concurrently. Four is small enough to keep that burst
// bounded and gentle on both VoidLLM's own outbound connection pool and
// whatever upstream MCP servers happen to be slow or rate-limited at that
// moment, while still large enough that an ordinary handful of concurrent
// cache misses is not artificially serialized down to one at a time.
//
// This cap lives on ToolCache itself (fetchSem), not as a single
// package-level semaphore shared by every ToolCache ever constructed in the
// process. In production this is the same thing: cmd/voidllm's wiring
// constructs exactly one ToolCache for the whole running process, so a
// per-instance cap already achieves a process-wide bound in the only
// topology VoidLLM ever runs. Keeping it per-instance rather than truly
// global additionally means the many independent ToolCache instances this
// package's own test suite constructs (one or more per test, frequently
// running with t.Parallel()) never silently share and deplete the same four
// slots with each other — a shared global here would make unrelated tests'
// blocked fetchers contend for the same bounded pool purely as a test-suite
// artifact, not a property this cap is meant to enforce at all.
const maxConcurrentToolsListFetches = 4

// sharedFetch runs (or joins) a single upstream fetch for serverID via
// tc.sf.DoChan, deduplicating concurrent callers exactly as tc.sf always
// has, and returns once either the shared fetch completes or ctx — THIS
// caller's own context, not necessarily the one that started the fetch —
// ends first. A caller whose own ctx ends first gets ctx.Err() back,
// wrapped, immediately; the shared fetch itself is entirely unaffected and
// continues running for every other caller still waiting on it.
//
// force, when true (RefreshServer only — see its own doc), skips the
// freshness recheck below unconditionally: RefreshServer's whole contract is
// "re-fetch right now, regardless of what is already cached", so it must
// never be satisfied by an entry sharedFetch itself decides is still fresh
// enough. entryFor always passes false: its own contract is "fetch only if
// missing or stale", which is exactly what the recheck below re-verifies.
//
// When force is false, the singleflight callback's FIRST action — before
// ever touching tc.fetchSem or calling tc.fetcher — is to re-check, under a
// brief RLock, whether a fresh entry for serverID already exists, and return
// it directly if so, without fetching at all. This closes a genuine TOCTOU
// window entryFor's own freshness check cannot close by itself: entryFor
// checks freshness, finds the entry missing or stale, and only THEN calls
// sharedFetch — and by the time this callback actually runs (scheduled by
// tc.sf.DoChan, not necessarily synchronously with entryFor's own check),
// some OTHER caller may have already published a fresh entry for the exact
// same serverID in between, whether by joining a different, now-completed
// singleflight round for this key or via RefreshServer. Without this
// recheck, this callback would still redundantly re-fetch upstream even
// though the cache already holds current data — never incorrect (the
// generation-guarded publish in fetchAndPublish still protects against
// publishing something stale), but a real, unnecessary amplifier of
// upstream load that a fixed sleep in a test can paper over without ever
// fixing in the production code path itself. Checking it exactly here, as
// the very first thing the callback that ANY caller might become the
// singleflight leader for does, is what makes the outcome deterministic
// regardless of scheduling — see this package's test suite for the "settle"
// sleeps this replaced.
//
// The shared fetch itself runs against
// context.WithTimeout(context.Background(), tc.fetchTimeout) — fully
// detached from every caller's context, not merely stripped of cancellation
// (the previous context.WithoutCancel(ctx) approach). Before context.
// WithoutCancel existed here, the leader's context was used directly: a
// caller with a short deadline, or one whose own request was simply
// cancelled by its client mid-flight, could sever the upstream fetch every
// OTHER caller currently joined to the same singleflight key was also
// waiting on, even though their own contexts were perfectly healthy.
// context.WithoutCancel fixed the cancellation half of that, but still
// carried the leader's own context VALUES forward — an accident of
// whichever caller happened to win the race to become singleflight's
// leader, not a property of the fetch itself, which serves every caller
// currently joined to it equally and has no legitimate reason to inherit
// values scoped to only one of them. context.Background() removes that
// accident entirely: the fetch is now the same detached, timeout-bounded
// operation regardless of which caller's context happened to start it.
//
// tc.fetchSem is acquired here too, with this same fetchCtx — see
// maxConcurrentToolsListFetches' own doc for why the cap exists and why it
// is scoped per-ToolCache — so time spent waiting for a free concurrency
// slot counts against toolsListFetchTimeout exactly like time spent waiting
// on the upstream itself: a caller blocked on this semaphore for the whole
// budget fails the same way a caller blocked on a genuinely slow upstream
// would, rather than waiting unboundedly for a slot that never frees up.
// The semaphore is only ever acquired once the freshness recheck above has
// already found a fetch necessary — an already-fresh entry costs this
// method nothing beyond the recheck's own RLock.
//
// Both entryFor and RefreshServer call sharedFetch — the SAME tc.sf key for
// a given serverID — so a forced RefreshServer and an ordinary cache-miss
// fetch for that server can never run concurrently: whichever call reaches
// tc.sf.DoChan first becomes singleflight's leader, and the other simply
// joins its result. See RefreshServer's own doc for why joining an
// in-flight fetch it did not itself start is an acceptable substitute for
// the forced refetch it would otherwise begin — and for the one case where it
// is NOT: the fetched return value below reports whether THIS singleflight
// round actually reached tc.fetchAndPublish, as opposed to returning early via
// the force-false freshness recheck above without ever contacting the
// upstream. RefreshServer reads it specifically to detect the case its own
// doc's "documented exception" glosses over — joining a call that turns out
// to have been that recheck's early return, which honors nobody's force=true
// contract at all, since the leader whose call this was had force=false to
// begin with.
//
// sharedFetchOutcome, not a bare *cacheEntry, is what the singleflight
// callback below actually returns (as any), specifically so every caller
// joined to one singleflight round — leader and joiners alike — observes the
// same fetched value for that round, exactly as they already observe the same
// entry and error: fetched is a property of what the round itself did, not
// of which caller happens to be asking.
func (tc *ToolCache) sharedFetch(ctx context.Context, serverID string, force bool) (entry *cacheEntry, fetched bool, err error) {
	ch := tc.sf.DoChan(serverID, func() (any, error) {
		if !force {
			tc.mu.RLock()
			e, ok := tc.entries[serverID]
			fresh := ok && e.isFresh()
			tc.mu.RUnlock()
			if fresh {
				if hook := sharedFetchFreshEntryHookForTest.Load(); hook != nil {
					// Test-only synchronization point — see this hook's own
					// doc. Never set outside a test; nil in every production
					// build's actual execution.
					(*hook)()
				}
				return sharedFetchOutcome{entry: e, fetched: false}, nil
			}
		}

		fetchCtx, cancel := context.WithTimeout(context.Background(), time.Duration(tc.fetchTimeout.Load()))
		defer cancel()

		if err := tc.fetchSem.Acquire(fetchCtx, 1); err != nil {
			return nil, fmt.Errorf("mcp: tool cache fetch: acquire fetch concurrency slot: %w", err)
		}
		defer tc.fetchSem.Release(1)

		e, err := tc.fetchAndPublish(fetchCtx, serverID)
		if err != nil {
			return nil, err
		}
		return sharedFetchOutcome{entry: e, fetched: true}, nil
	})
	if hook := sharedFetchJoinedHookForTest.Load(); hook != nil {
		// Test-only synchronization point — see this hook's own doc. Never
		// set outside a test; nil in every production build's actual
		// execution.
		(*hook)(ctx)
	}
	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, false, res.Err
		}
		outcome, _ := res.Val.(sharedFetchOutcome)
		if hook := sharedFetchResultHookForTest.Load(); hook != nil {
			// Test-only observation point — see this hook's own doc. Never
			// set outside a test; nil in every production build's actual
			// execution.
			(*hook)(ctx, outcome.fetched)
		}
		return outcome.entry, outcome.fetched, nil
	case <-ctx.Done():
		return nil, false, fmt.Errorf("mcp: tool cache fetch: %w", ctx.Err())
	}
}

// sharedFetchOutcome is the value sharedFetch's singleflight callback
// returns: the resulting *cacheEntry (nil when fetchAndPublish itself
// discarded a superseded fetch — see that method's own (nil, nil) case), and
// whether this singleflight round actually performed an upstream fetch via
// fetchAndPublish, as opposed to returning early via the force-false
// freshness recheck. See sharedFetch's own doc for why RefreshServer needs
// this distinction.
type sharedFetchOutcome struct {
	entry   *cacheEntry
	fetched bool
}

// sharedFetchFreshEntryHookForTest, when its Load() is non-nil, is invoked by
// sharedFetch's singleflight callback immediately after a force=false round
// has decided — via its own inner freshness recheck (see sharedFetch's own
// doc) — to return an already-fresh entry without ever reaching
// fetchAndPublish, but before that return actually happens. Its sole purpose
// is letting a test pause a round at exactly that point: the shortcut it
// guards otherwise completes so close to instantly (one RLock, one map read,
// one RUnlock) that no amount of scheduling — goroutine yields, short sleeps
// — can reliably land a concurrent RefreshServer call inside the same
// singleflight round while it is still in flight, the one scenario
// RefreshServer's own retry-when-not-fetched behavior (see its own doc)
// exists to handle.
//
// atomic.Pointer[func()] rather than a plain package-level var func(): this
// is set and cleared by tests (SetSharedFetchFreshEntryHookForTest,
// export_test.go) concurrently with sharedFetch reading it from whichever
// goroutine tc.sf's singleflight callback happens to run on for any
// ToolCache in the same test binary — a bare `var sharedFetchFreshEntryHookForTest
// func()` read and written across goroutines without synchronization is a
// data race the race detector rightly flags, even though every actual
// assignment in practice is serialized by each test's own "set, run, defer
// clear" discipline. The atomic makes that safe structurally instead of by
// convention alone. Production code never assigns this; its zero value
// (Load() returning nil) costs the hot path this guards nothing beyond the
// one atomic load and nil check already visible at its call site.
var sharedFetchFreshEntryHookForTest atomic.Pointer[func()]

// sharedFetchJoinedHookForTest, when its Load() is non-nil, is invoked by
// sharedFetch immediately after its own tc.sf.DoChan(serverID, ...) call
// returns — the moment this specific caller's call has been registered with
// the singleflight group, either as a fresh round's leader or as a joiner of
// an already in-flight round for the same key — but strictly before this
// call blocks on that round's result channel. DoChan itself is synchronous:
// by the time it returns, registration under the group's own lock has
// already happened, so this hook firing is a genuine, ordering-guaranteed
// proof of "this caller has joined (or started) the round for serverID now",
// not a probabilistic one.
//
// Its purpose mirrors sharedFetchFreshEntryHookForTest's, one step earlier in
// sharedFetch: a test that needs to prove a SPECIFIC caller (e.g.
// RefreshServer, as opposed to the round's own leader) has actually reached
// tc.sf.DoChan for a still in-flight round, before releasing that round to
// complete, previously had nothing but a bounded runtime.Gosched() spin plus
// a fixed sleep to make that likely — never structurally guaranteed. A test
// hook installed here, filtering on a marker the test itself embeds in ctx
// (this hook receives the caller's own ctx unmodified), turns that guess into
// a genuine synchronization point: close a channel from inside the hook, and
// the test's main goroutine can wait on it instead of sleeping.
//
// Same atomic.Pointer discipline as sharedFetchFreshEntryHookForTest: set,
// use, and clear via SetSharedFetchJoinedHookForTest (export_test.go); nil,
// and therefore free beyond one atomic load and nil check, in production.
var sharedFetchJoinedHookForTest atomic.Pointer[func(ctx context.Context)]

// sharedFetchResultHookForTest, when its Load() is non-nil, is invoked by
// sharedFetch immediately after THIS caller receives its own result from the
// singleflight round's channel (the res := <-ch branch only — never the
// ctx.Done() branch, which never has an outcome to report), with that
// caller's own ctx and the fetched value it observed for that round. Every
// caller joined to one singleflight round — leader and joiners alike —
// receives the SAME fetched value (see sharedFetchOutcome's own doc), so this
// hook reports, per caller, which round(s) that specific caller took part in
// and whether each one reached fetchAndPublish.
//
// Its purpose is letting a test attribute an observed fetched=false (or
// fetched=true) outcome to a SPECIFIC caller — e.g. counting how many times
// RefreshServer itself observed fetched=false across its own retry loop, as
// opposed to any other caller (GetTools, another RefreshServer instance in a
// concurrent test) that happens to be joined to the same rounds — by
// filtering on a marker the test embeds in the ctx it passes to that specific
// caller.
//
// Same atomic.Pointer discipline as sharedFetchFreshEntryHookForTest: set,
// use, and clear via SetSharedFetchResultHookForTest (export_test.go); nil,
// and therefore free beyond one atomic load and nil check, in production.
var sharedFetchResultHookForTest atomic.Pointer[func(ctx context.Context, fetched bool)]

// maxToolCacheEntryAttempts bounds how many times entryFor will retry after
// a fetch it received is discarded — either because fetchAndPublish itself
// never published it (the existing (nil, nil) case) or because entryFor's
// own generation recheck below found it superseded — before giving up.
// Continuous invalidation churn (an admin, or misbehaving automation,
// invalidating a server on every single fetch) would otherwise spin this
// loop forever; each attempt still performs a real upstream round trip via
// sharedFetch, so an unbounded retry loop here is not merely wasted CPU but
// unbounded upstream load as well.
const maxToolCacheEntryAttempts = 5

// errToolCacheEntryRetriesExhausted is returned by entryFor when
// maxToolCacheEntryAttempts consecutive fetches were all discarded before
// entryFor could return one to its own caller.
var errToolCacheEntryRetriesExhausted = errors.New("mcp: tool cache entry could not be resolved after repeated invalidation")

// errToolCacheRefreshRetriesExhausted is returned by RefreshServer when
// maxToolCacheEntryAttempts consecutive singleflight rounds it took part in
// — as leader or as joiner — all ended without actually reaching
// fetchAndPublish, so RefreshServer could never confirm a genuine re-fetch
// of serverID's tools ran under its own call. See RefreshServer's own doc
// for exactly how a round can end up not reaching fetchAndPublish despite
// force=true.
var errToolCacheRefreshRetriesExhausted = errors.New("mcp: tool cache refresh could not force a genuine re-fetch after repeated retries")

// entryFor returns the cache entry for serverID, fetching it from upstream
// if missing or stale — the shared fresh/stale, fetch-on-miss,
// single-flight-deduplicated logic both GetTools and HeaderParams need.
//
// The freshness check runs under only a brief RLock; the fetch itself
// (sharedFetch, which calls fetchAndPublish) runs outside any lock this
// method holds, deduplicated per serverID by tc.sf instead — see that
// field's own doc for why this replaced the previous "hold a full write
// Lock across the whole fetch" approach. A concurrent caller for the same
// stale-or-missing serverID shares the same in-flight fetch and its result
// via singleflight, exactly as the previous full-Lock double-check pattern
// shared one fetch among however many goroutines were blocked waiting for
// the lock.
//
// Two independent conditions make entryFor discard a fetch it received
// instead of returning it to its own caller, both retried identically from
// the top of the loop:
//
//   - sharedFetch/fetchAndPublish itself never published the fetch at all
//     (the (nil, nil) case — a concurrent Invalidate or InvalidateWithStore
//     superseded it before fetchAndPublish's own generation check).
//   - fetchAndPublish DID publish it, but a concurrent Invalidate or
//     InvalidateWithStore landed afterward, in the window between the
//     publish step and fetchAndPublish's own, separately-locked store
//     write — see cacheEntry.generation's own doc for exactly this window
//     and why comparing it against the CURRENT tc.generations[serverID] is
//     what catches it.
//
// Either way, by the time this happens tc.entries no longer holds the
// discarded entry (or holds a different, newer one) for serverID, so
// re-checking freshness at the top of the loop naturally triggers a fresh
// fetch — or picks up the newer entry directly — under the current,
// post-invalidation state, the same outcome any other caller arriving after
// the invalidation would see. The loop is bounded by
// maxToolCacheEntryAttempts (see that const's own doc) and also checks
// ctx.Err() on every iteration, so a caller whose own context has already
// ended does not spend an attempt on a fetch it can no longer use anyway.
//
// It returns the live *cacheEntry pointer, not a copy. Per cacheEntry's own
// doc, that pointer is immutable once published: any future update replaces
// the map entry with an entirely new *cacheEntry instead of mutating this
// one's fields, so a caller reading tools or headerParams off of the
// returned pointer after entryFor has released tc.mu can never observe a
// partial or torn update. Callers that hand data derived from this pointer
// to code outside this package (GetTools, HeaderParams) still deep-copy on
// the way out via copyTools/copyHeaderParams — that copy protects the
// CACHE from a caller mutating what it receives, which is a separate
// concern from this pointer's own safety to read.
func (tc *ToolCache) entryFor(ctx context.Context, serverID string) (*cacheEntry, error) {
	for attempt := 0; attempt < maxToolCacheEntryAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("mcp: tool cache entry: %w", err)
		}

		tc.mu.RLock()
		e, ok := tc.entries[serverID]
		fresh := ok && e.isFresh()
		tc.mu.RUnlock()
		if fresh {
			return e, nil
		}

		entry, _, err := tc.sharedFetch(ctx, serverID, false)
		if err != nil {
			return nil, err
		}
		if entry == nil {
			continue
		}

		tc.mu.RLock()
		curGen := tc.generations[serverID]
		tc.mu.RUnlock()
		if curGen != entry.generation {
			// Superseded by a concurrent Invalidate/InvalidateWithStore that
			// landed after fetchAndPublish published this entry — see
			// cacheEntry.generation's own doc. Discard exactly like the
			// existing (nil, nil) case above and retry.
			continue
		}
		return entry, nil
	}
	return nil, errToolCacheEntryRetriesExhausted
}

// GetTools returns the cached tools for serverID, fetching them from upstream
// if the entry is missing or stale. A double-check pattern ensures that at most
// one fetch per serverID is in flight when multiple goroutines request the same
// stale entry concurrently.
func (tc *ToolCache) GetTools(ctx context.Context, serverID string) ([]Tool, error) {
	e, err := tc.entryFor(ctx, serverID)
	if err != nil {
		return nil, err
	}
	return copyTools(e.tools), nil
}

// HeaderParams returns a deep copy of toolName's validated x-mcp-header
// bindings (MCP 2026-07-28 §4.3) for serverID, using the same fresh/stale,
// fetch-on-miss logic GetTools uses (entryFor) — a stale or missing entry is
// refreshed from upstream exactly as it would be for a GetTools call made at
// the same moment. Returns (nil, nil) when serverID's tool listing has no
// entry for toolName: either the server has no such tool, or the tool's
// schema violated §4.3 and FilterHeaderParamTools already excluded it from
// the listing entirely (see that function's doc) — CallMCPTool treats both
// the same way, mirroring no headers and returning no error.
//
// On the Code Mode path this call is warm almost every time: the same
// request has typically already called GetTools moments earlier for the
// same serverID, so entryFor's fast path applies and this method costs one
// RLock plus two map lookups (entryFor's own, then this method's lookup into
// headerParams) — no upstream I/O.
func (tc *ToolCache) HeaderParams(ctx context.Context, serverID, toolName string) ([]HeaderParam, error) {
	e, err := tc.entryFor(ctx, serverID)
	if err != nil {
		return nil, err
	}
	return copyHeaderParams(e.headerParams[toolName]), nil
}

// GetAllTools returns a snapshot of all currently cached tool lists keyed by
// server ID. Only entries that are already in the cache are included; no
// upstream fetches are performed.
func (tc *ToolCache) GetAllTools() map[string][]Tool {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	snapshot := make(map[string][]Tool, len(tc.entries))
	for serverID, e := range tc.entries {
		snapshot[serverID] = copyTools(e.tools)
	}
	return snapshot
}

// RefreshServer forces a re-fetch of the tool list for serverID, going
// through the SAME tc.sf singleflight key (via sharedFetch) that entryFor's
// own fetch-on-miss path uses — see sharedFetch's own doc. This means
// RefreshServer's "force a fetch regardless of freshness" contract has one
// documented exception: if a fetch for serverID is ALREADY in flight when
// RefreshServer is called — started by a concurrent GetTools/HeaderParams
// cache miss, or by another concurrent RefreshServer call — RefreshServer
// joins that existing fetch and reports its outcome, rather than starting a
// second, independent one. An in-flight fetch already satisfies "re-fetch
// this server's tools right now" exactly as well as a fetch RefreshServer
// started itself would; starting a second one concurrently would only
// duplicate upstream load against the same server for no benefit, and is
// exactly what routing both callers through the same tc.sf key exists to
// prevent (see tc.sf's own field doc). On fetch failure the existing cache
// entry is preserved and the error is returned.
//
// RefreshServer always passes force=true to sharedFetch: unlike entryFor,
// it must never be satisfied by sharedFetch's own freshness recheck handing
// back an already-cached entry without fetching (see that method's own doc)
// — a fresh entry is exactly the case an ordinary GetTools/HeaderParams call
// already serves without ever reaching sharedFetch at all, and RefreshServer
// exists specifically for callers that need a genuine re-fetch regardless.
// force only changes what happens if THIS call becomes singleflight's
// leader; if it instead joins an already in-flight fetch (the documented
// exception above), it receives that fetch's result exactly as any other
// joiner would, regardless of which force value either caller passed — and
// that in-flight round could have been started by an ordinary,
// force=false GetTools/HeaderParams cache miss whose own freshness recheck
// (sharedFetch's own doc) found an entry that only became fresh AFTER
// RefreshServer's caller decided a refresh was needed, and so returned that
// already-cached entry without ever reaching fetchAndPublish at all. Joining
// a round like that would silently downgrade RefreshServer's "re-fetch right
// now, regardless of what is already cached" contract into "return whatever
// happens to already be cached" — exactly the outcome RefreshServer exists
// to NOT provide. sharedFetch's second return value, fetched, is what lets
// RefreshServer tell the two apart: when the round it ended up part of — as
// leader or as joiner — did not actually reach fetchAndPublish, RefreshServer
// runs sharedFetch again, still with force=true. Because a round already
// completed by the time the next call is made (tc.sf.DoChan does not hand
// back a Result until the singleflight round it belongs to has finished),
// that next call cannot join the SAME non-fetching round again — it either
// becomes leader of a brand new round (which, with force=true, always
// reaches fetchAndPublish) or joins some OTHER round already in flight by
// then.
//
// That "joins some other round" branch is exactly why this is a bounded
// LOOP rather than a single retry: the round it joins instead can, in turn,
// ALSO turn out to be a force=false round that takes the freshness shortcut
// without ever reaching fetchAndPublish, if a concurrent
// GetTools/HeaderParams cache miss happens to win the race for tc.sf's key
// again immediately afterward. Nothing about tc.sf's per-key deduplication
// rules out that happening several times in a row under sustained
// concurrent load — an earlier version of this method treated a single
// retry as always enough, on the assumption that this was a one-off
// scheduling window; that assumption does not actually hold; a continuously
// busy serverID could in principle keep handing every retry a fresh
// non-fetching round to join. RefreshServer therefore loops, re-checking
// ctx.Err() on every iteration so a caller whose own context has already
// ended does not spend an attempt on a round it can no longer use, up to
// maxToolCacheEntryAttempts times — the same bound and constant entryFor's
// own analogous retry loop uses, reused here rather than duplicated so the
// two independent "give up after this many discarded rounds" policies stay
// in sync — before giving up with the static
// errToolCacheRefreshRetriesExhausted sentinel.
//
// The generation-guarded publish, store write, and "a concurrent Invalidate
// or InvalidateWithStore must win over a fetch already in flight" guarantee
// this method has always offered lives in fetchAndPublish, which sharedFetch
// calls on RefreshServer's behalf exactly as it does for entryFor. See
// fetchAndPublish's own doc for the full guarantee, including why a
// discarded fetch (superseded by a concurrent invalidation) is reported here
// as success (nil error): the fetch itself succeeded, it is simply no longer
// the freshest information available about serverID, and entryFor will
// re-fetch under the new state the next time serverID is accessed, exactly
// as it would after any other invalidation.
func (tc *ToolCache) RefreshServer(ctx context.Context, serverID string) error {
	var err error
	for attempt := 0; attempt < maxToolCacheEntryAttempts; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("mcp: tool cache refresh: %w", ctxErr)
		}

		var fetched bool
		_, fetched, err = tc.sharedFetch(ctx, serverID, true)
		if err != nil {
			return err
		}
		if fetched {
			return nil
		}
		// Joined a round that never reached fetchAndPublish — see this
		// method's own doc for exactly how that happens. Retry: a
		// force=true call cannot join THIS SAME round again, since
		// tc.sf.DoChan only hands back a Result once the round it belongs
		// to has already finished.
	}
	return errToolCacheRefreshRetriesExhausted
}

// RefreshAll forces a re-fetch for every server ID currently in the cache.
// All entries are refreshed even when individual fetches fail. The first
// error encountered is returned; subsequent errors are joined with it.
func (tc *ToolCache) RefreshAll(ctx context.Context) error {
	tc.mu.RLock()
	serverIDs := make([]string, 0, len(tc.entries))
	for serverID := range tc.entries {
		serverIDs = append(serverIDs, serverID)
	}
	tc.mu.RUnlock()

	var firstErr error
	for _, serverID := range serverIDs {
		if err := tc.RefreshServer(ctx, serverID); err != nil {
			firstErr = errors.Join(firstErr, err)
		}
	}
	return firstErr
}

// SetTools manually populates the cache for serverID with the given tools.
// Used for the built-in server whose tools come from memory, not HTTP.
// The entry is marked fresh at the current time; it will not be re-fetched
// from upstream until maxAge has elapsed. tools is deep-copied on ingest, so
// the cache never aliases the caller's backing array (symmetric with the
// deep copies GetTools and GetAllTools hand back on read) — the caller
// retains full ownership of tools and may keep using or mutating it after
// this call without affecting the cached entry.
func (tc *ToolCache) SetTools(serverID string, tools []Tool) {
	// The built-in server has no CacheableResult hint to read here — it isn't
	// fetched over HTTP at all — so this resolves to the same fallback
	// resolveTTL uses for any upstream that sent none: ttl = tc.maxAge,
	// neverExpires = tc.maxAge == 0. That is what keeps this method's own
	// "fresh until maxAge elapses" doc true after ttl became per-entry.
	ttl, neverExpires := tc.resolveTTL(CacheHint{})
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.entries[serverID] = &cacheEntry{
		tools:        copyTools(tools),
		fetchedAt:    time.Now(),
		ttl:          ttl,
		neverExpires: neverExpires,
	}
}

// Invalidate removes the cached entry for serverID. Subsequent calls to
// GetTools for that serverID will trigger a fresh upstream fetch.
//
// It also advances serverID's generation counter (tc.generations), which is
// what makes a RefreshServer call already in flight for serverID discard its
// result instead of publishing it after this call returns — see
// RefreshServer's own doc for the full compare-and-swap this enables.
func (tc *ToolCache) Invalidate(serverID string) {
	tc.mu.Lock()
	delete(tc.entries, serverID)
	tc.generations[serverID]++
	tc.mu.Unlock()
	tc.fireOnChange(serverID)
}

// InvalidateWithStore removes a server from the cache and deletes its
// persisted tools from the backing store. serverID is the database ID used
// both as the cache key and to address the store deletion.
//
// Like Invalidate, it advances serverID's generation counter before
// releasing tc.mu — see Invalidate's and RefreshServer's own docs — so a
// RefreshServer already mid-fetch under the credential or configuration this
// call is reacting to (an admin rotating a server's auth token, for example)
// cannot re-publish that stale fetch, or persist it to tc.store, after this
// call has already deleted both copies.
//
// The generation counter is bumped BEFORE this method even attempts to
// acquire tc.storeMu, and the store delete itself runs under tc.storeMu —
// see that field's own doc for why this ordering, combined with
// RefreshServer's nested re-check while holding the same lock, is what
// guarantees this call's store.Delete is never silently undone by a
// RefreshServer that started before it.
func (tc *ToolCache) InvalidateWithStore(ctx context.Context, serverID string) {
	tc.mu.Lock()
	delete(tc.entries, serverID)
	tc.generations[serverID]++
	tc.mu.Unlock()
	if tc.store != nil {
		tc.storeMu.Lock()
		_ = tc.store.Delete(ctx, serverID) //nolint:errcheck
		tc.storeMu.Unlock()
	}
	tc.fireOnChange(serverID)
}

// FreshFor returns how long the cache entry for serverID has been fresh,
// measured from the time the entry was last fetched. It returns -1 if the
// serverID is not present in the cache. The caller can use this to enforce a
// cooldown between forced refreshes.
func (tc *ToolCache) FreshFor(serverID string) time.Duration {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	entry, ok := tc.entries[serverID]
	if !ok {
		return -1
	}
	return time.Since(entry.fetchedAt)
}

// ToolCount returns the number of tools cached for serverID. It returns 0 if
// serverID is not present in the cache; no upstream fetch is performed.
func (tc *ToolCache) ToolCount(serverID string) int {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	e, ok := tc.entries[serverID]
	if !ok {
		return 0
	}
	return len(e.tools)
}
