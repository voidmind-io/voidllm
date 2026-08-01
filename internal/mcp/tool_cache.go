package mcp

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
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
}

// NewToolCache creates a ToolCache that uses fetcher to retrieve tool schemas
// and considers entries stale after maxAge. A maxAge of zero means entries
// never expire automatically.
func NewToolCache(fetcher ToolFetcher, maxAge time.Duration) *ToolCache {
	return &ToolCache{
		entries:     make(map[string]*cacheEntry),
		generations: make(map[string]uint64),
		fetcher:     fetcher,
		maxAge:      maxAge,
	}
}

// NewPersistentToolCache creates a ToolCache backed by a persistent store.
// Tools are written through to the store on every fetch whose CacheableResult
// hint (MCP 2026-07-28 §5) does not mark the listing "private" — see
// ToolCache.persistListing — and can be loaded from the store at startup via
// LoadFromStore.
func NewPersistentToolCache(fetcher ToolFetcher, maxAge time.Duration, store ToolStore) *ToolCache {
	return &ToolCache{
		entries:     make(map[string]*cacheEntry),
		generations: make(map[string]uint64),
		fetcher:     fetcher,
		maxAge:      maxAge,
		store:       store,
	}
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
		// stale on first access. This ensures tools are refreshed from upstream
		// within maxAge of startup, while still providing immediate availability
		// for TypeScript type generation and list_servers tool counts.
		//
		// ttl and neverExpires are resolved exactly as they were before ttlMs
		// hints existed: a DB-loaded entry carries no fetch-time CacheHint (the
		// store persists only tools, see ToolStore.LoadAll), so it always falls
		// back to tc.maxAge here, same as resolveTTL's no-hint branch would. The
		// entry is stale on first access when maxAge > 0 (the production case)
		// and fresh when maxAge == 0, unchanged from before this cacheEntry had
		// ttl/neverExpires fields at all.
		tc.entries[serverID] = &cacheEntry{
			tools:        tools,
			fetchedAt:    time.Time{},
			ttl:          tc.maxAge,
			neverExpires: tc.maxAge == 0,
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
func (tc *ToolCache) persistListing(ctx context.Context, serverID string, listing *ToolListing) {
	if listing.Cache.Scope == CacheScopePrivate {
		if err := tc.store.Delete(ctx, serverID); err != nil {
			slog.Default().LogAttrs(ctx, slog.LevelWarn, "mcp: failed to delete private tool listing from store",
				slog.String("server_id", serverID),
				slog.String("error", err.Error()))
		}
		return
	}
	if listing.Cache.TTLMsSet && listing.Cache.Scope != CacheScopePublic {
		// A hint was offered but did not grant public-scope sharing — see the
		// doc above. Neither saved nor deleted: this is not the "the upstream
		// said private" case (handled above), just an absence of proof this
		// listing is safe to persist and hand to any caller.
		return
	}
	_ = tc.store.Save(ctx, serverID, listing.Tools) //nolint:errcheck
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

// entryFor returns the cache entry for serverID, fetching it from upstream
// if missing or stale — the shared fresh/stale, fetch-on-miss,
// single-flight-via-double-check logic both GetTools and HeaderParams need.
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
	tc.mu.RLock()
	e, ok := tc.entries[serverID]
	if ok && e.isFresh() {
		tc.mu.RUnlock()
		return e, nil
	}
	tc.mu.RUnlock()

	// Entry is missing or stale — upgrade to write lock.
	tc.mu.Lock()
	defer tc.mu.Unlock()

	// Double-check: another goroutine may have fetched while we waited.
	e, ok = tc.entries[serverID]
	if ok && e.isFresh() {
		return e, nil
	}

	listing, err := tc.fetcher(ctx, serverID)
	if err != nil {
		return nil, err
	}
	ttl, neverExpires := tc.resolveTTL(listing.Cache)
	e = &cacheEntry{
		tools:        listing.Tools,
		headerParams: listing.HeaderParams,
		fetchedAt:    time.Now(),
		ttl:          ttl,
		neverExpires: neverExpires,
		scope:        listing.Cache.Scope,
	}
	tc.entries[serverID] = e
	if tc.store != nil {
		tc.persistListing(ctx, serverID, listing)
	}
	return e, nil
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

// RefreshServer forces a re-fetch of the tool list for serverID regardless of
// whether the cached entry is still fresh. On fetch failure the existing cache
// entry is preserved and the error is returned.
//
// The fetch itself (tc.fetcher) runs outside tc.mu, same as before this
// method's own generation counter existed — an upstream round-trip has no
// business holding the cache lock for its whole duration (unlike entryFor's
// fetch-on-miss path, which single-flights under a full Lock by design; see
// that method's own doc for why the two are not the same tradeoff). That gap
// is exactly what lets a concurrent Invalidate or InvalidateWithStore run
// while this fetch is still in flight — including one triggered because an
// admin just changed this very server's credential. Without a check, this
// method would publish a result it fetched under the OLD credential straight
// back into tc.entries once the fetch returns, silently undoing the
// invalidation. tc.generations[serverID] is this method's guard against
// exactly that: it is read once, before the fetch starts, and compared
// again, under tc.mu, once the fetch returns — the same compare-and-swap
// shape http_transport.go's eraBinding/invalidateBinding use to solve the
// identical "a late-returning call must not clobber a newer generation"
// problem for a resolved upstream era binding. A mismatch means Invalidate
// or InvalidateWithStore ran in between: this fetch's result is discarded
// entirely — neither published to tc.entries nor persisted to tc.store, so a
// stale, pre-invalidation listing can never reach either — and RefreshServer
// returns nil, since the fetch itself succeeded; it is simply no longer the
// freshest information available about serverID; entryFor will re-fetch
// under the new state the next time serverID is accessed, exactly as it
// would after any other invalidation.
func (tc *ToolCache) RefreshServer(ctx context.Context, serverID string) error {
	tc.mu.RLock()
	startGen := tc.generations[serverID]
	tc.mu.RUnlock()

	listing, err := tc.fetcher(ctx, serverID)
	if err != nil {
		return err
	}

	ttl, neverExpires := tc.resolveTTL(listing.Cache)
	tc.mu.Lock()
	if tc.generations[serverID] != startGen {
		tc.mu.Unlock()
		slog.Default().LogAttrs(ctx, slog.LevelDebug, "mcp: discarding refresh superseded by a concurrent invalidation",
			slog.String("server_id", serverID))
		return nil
	}
	tc.entries[serverID] = &cacheEntry{
		tools:        listing.Tools,
		headerParams: listing.HeaderParams,
		fetchedAt:    time.Now(),
		ttl:          ttl,
		neverExpires: neverExpires,
		scope:        listing.Cache.Scope,
	}
	tc.mu.Unlock()

	if tc.store != nil {
		tc.persistListing(ctx, serverID, listing)
	}
	return nil
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
func (tc *ToolCache) InvalidateWithStore(ctx context.Context, serverID string) {
	tc.mu.Lock()
	delete(tc.entries, serverID)
	tc.generations[serverID]++
	tc.mu.Unlock()
	if tc.store != nil {
		_ = tc.store.Delete(ctx, serverID) //nolint:errcheck
	}
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
