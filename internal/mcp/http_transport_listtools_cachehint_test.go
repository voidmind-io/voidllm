package mcp_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers ListTools' CacheHint aggregation across pages (MCP
// 2026-07-28 §5), per ListTools' own doc: the minimum TTL across every page
// that carried a hint; no hint at all when ANY page carried none; and
// CacheScopePrivate for the whole aggregated listing when pages that did
// carry a hint disagree on cacheScope. CacheHint is a modern-era-only
// concept (legacyClientDialect.Parse never sets it), so every test here uses
// the modern transport.

// ---- Minimum TTL across pages ------------------------------------------------

// TestListTools_Modern_CacheHint_MinTTLAcrossPages drives two pages that both
// carry a hint with the SAME scope but different ttlMs, and verifies the
// aggregated TTLMs is the minimum of the two, not the first, the last, or
// their sum.
func TestListTools_Modern_CacheHint_MinTTLAcrossPages(t *testing.T) {
	t.Parallel()

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t1","inputSchema":{"type":"object"}}],"nextCursor":"next","ttlMs":5000,"cacheScope":"public"}}`)
		case 2:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t2","inputSchema":{"type":"object"}}],"ttlMs":2000,"cacheScope":"public"}}`)
		default:
			t.Errorf("unexpected request #%d", n)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}

	if !listing.Cache.TTLMsSet {
		t.Fatal("Cache.TTLMsSet = false, want true (both pages carried a hint)")
	}
	if listing.Cache.TTLMs != 2000 {
		t.Errorf("Cache.TTLMs = %d, want 2000 (the minimum across both pages)", listing.Cache.TTLMs)
	}
	if listing.Cache.Scope != mcp.CacheScopePublic {
		t.Errorf("Cache.Scope = %q, want %q (both pages agreed)", listing.Cache.Scope, mcp.CacheScopePublic)
	}
}

// ---- One page without a hint at all: no aggregate hint ----------------------

// TestListTools_Modern_CacheHint_OnePageWithoutHint_NoAggregateHint verifies
// that a hint on SOME pages is not enough: if even one page carries no
// CacheableResult hint at all, the whole aggregated listing gets no hint
// (the zero CacheHint), regardless of what any other page offered.
func TestListTools_Modern_CacheHint_OnePageWithoutHint_NoAggregateHint(t *testing.T) {
	t.Parallel()

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			// Carries a hint.
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t1","inputSchema":{"type":"object"}}],"nextCursor":"next","ttlMs":5000,"cacheScope":"public"}}`)
		case 2:
			// No ttlMs, no cacheScope at all — no hint offered.
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t2","inputSchema":{"type":"object"}}]}}`)
		default:
			t.Errorf("unexpected request #%d", n)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}

	if listing.Cache.TTLMsSet {
		t.Errorf("Cache.TTLMsSet = true, want false — one page offered no hint, so the aggregate must carry none either, "+
			"got Cache = %+v", listing.Cache)
	}
	if listing.Cache != (mcp.CacheHint{}) {
		t.Errorf("Cache = %+v, want the zero CacheHint", listing.Cache)
	}
}

// ---- Mixed cacheScope: aggregates private, and ToolCache does not persist ---

// TestListTools_Modern_CacheHint_MixedScope_AggregatesPrivate_AndToolCacheDoesNotPersist
// drives two pages that both carry a hint but disagree on cacheScope (one
// "public", one "private"), and verifies two things end to end: ListTools
// itself aggregates the whole listing as CacheScopePrivate (with TTLMsSet
// still true, since both pages DID offer a hint), and feeding that listing
// through a PersistentToolCache never calls the store's Save — only Delete,
// exactly as a genuinely private-scoped single-page fetch would (see
// tool_cache_cachehint_test.go's TestToolCache_PersistListing_ScopeGovernsSaveVsDelete).
func TestListTools_Modern_CacheHint_MixedScope_AggregatesPrivate_AndToolCacheDoesNotPersist(t *testing.T) {
	t.Parallel()

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t1","inputSchema":{"type":"object"}}],"nextCursor":"next","ttlMs":5000,"cacheScope":"public"}}`)
		case 2:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t2","inputSchema":{"type":"object"}}],"ttlMs":2000,"cacheScope":"private"}}`)
		default:
			t.Errorf("unexpected request #%d", n)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}

	if !listing.Cache.TTLMsSet {
		t.Fatal("Cache.TTLMsSet = false, want true (both pages offered a hint, even though they disagreed on scope)")
	}
	if listing.Cache.Scope != mcp.CacheScopePrivate {
		t.Errorf("Cache.Scope = %q, want %q (disagreement among hinted pages must resolve to private)", listing.Cache.Scope, mcp.CacheScopePrivate)
	}

	// Feed this exact listing through a PersistentToolCache and confirm the
	// store is never asked to Save — only Delete (mirroring a genuinely
	// private single-page fetch).
	store := &fakeToolStore{}
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		return listing, nil
	}
	cache := mcp.NewPersistentToolCache(fetcher, time.Hour, store)
	if err := cache.RefreshServer(context.Background(), "srv"); err != nil {
		t.Fatalf("RefreshServer: %v", err)
	}
	saves, deletes := store.counts()
	if saves != 0 {
		t.Errorf("store.Save called %d times, want 0 — a mixed-scope aggregate must never be persisted", saves)
	}
	if deletes != 1 {
		t.Errorf("store.Delete called %d times, want 1 (a private-scoped listing actively evicts any previously persisted copy)", deletes)
	}
}

// ---- A page with no hint at all must not mask another page's "private" -----

// TestListTools_Modern_CacheHint_PrivatePlusNoHint_AggregatesPrivate_AndNotPersisted
// drives one page that offers no CacheableResult hint at all and a second
// page that explicitly reports cacheScope "private". Before the fix this
// guards, ListTools' aggregation set aggNoHint on the hint-less page and
// then returned the ENTIRE zero CacheHint for the aggregate — silently
// dropping the other page's explicit privacy claim along with everything
// else. The aggregate's Scope must be forced to CacheScopePrivate
// regardless, and feeding it through a PersistentToolCache must never call
// the store's Save — only Delete, exactly like a genuinely, fully-hinted
// private fetch.
func TestListTools_Modern_CacheHint_PrivatePlusNoHint_AggregatesPrivate_AndNotPersisted(t *testing.T) {
	t.Parallel()

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			// No ttlMs, no cacheScope at all — no hint offered.
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t1","inputSchema":{"type":"object"}}],"nextCursor":"next"}}`)
		case 2:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t2","inputSchema":{"type":"object"}}],"ttlMs":2000,"cacheScope":"private"}}`)
		default:
			t.Errorf("unexpected request #%d", n)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}

	if listing.Cache.Scope != mcp.CacheScopePrivate {
		t.Errorf("Cache.Scope = %q, want %q — one page's explicit private hint must never be masked by another page offering none at all",
			listing.Cache.Scope, mcp.CacheScopePrivate)
	}

	store := &fakeToolStore{}
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		return listing, nil
	}
	cache := mcp.NewPersistentToolCache(fetcher, time.Hour, store)
	if err := cache.RefreshServer(context.Background(), "srv"); err != nil {
		t.Fatalf("RefreshServer: %v", err)
	}
	saves, deletes := store.counts()
	if saves != 0 {
		t.Errorf("store.Save called %d times, want 0 — a listing any page marked private must never be persisted", saves)
	}
	if deletes != 1 {
		t.Errorf("store.Delete called %d times, want 1", deletes)
	}
}

// ---- Single page: aggregation must not change existing behavior -------------

// TestListTools_Modern_CacheHint_SinglePage_Unchanged is the regression guard
// for the aggregation logic added alongside pagination: a single-page fetch
// (no nextCursor at all) must carry its own CacheHint through completely
// unchanged, exactly as it did before pagination existed.
func TestListTools_Modern_CacheHint_SinglePage_Unchanged(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"only","inputSchema":{"type":"object"}}],"ttlMs":7500,"cacheScope":"public"}}`)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}

	if !listing.Cache.TTLMsSet {
		t.Fatal("Cache.TTLMsSet = false, want true")
	}
	if listing.Cache.TTLMs != 7500 {
		t.Errorf("Cache.TTLMs = %d, want 7500", listing.Cache.TTLMs)
	}
	if listing.Cache.Scope != mcp.CacheScopePublic {
		t.Errorf("Cache.Scope = %q, want %q", listing.Cache.Scope, mcp.CacheScopePublic)
	}
}

// ---- Unknown-scope hint plus a no-hint page: neither saved nor deleted -----

// TestListTools_Modern_CacheHint_UnknownScopePlusNoHint_NeitherSavedNorDeleted
// drives one page that offers no CacheableResult hint at all and a second
// page whose hint sets ttlMs but an unrecognized cacheScope (parseCacheHint
// leaves Scope at "" for anything other than exactly "public" or "private").
// Before the fix this test guards, ListTools' aggregation treated the
// no-hint page as decisive — aggNoHint forced the WHOLE aggregate back to
// the zero CacheHint, which persistListing's own fallback branch always
// persists — silently promoting an unproven scope claim to "safe to
// persist" merely because another page happened to say nothing at all. The
// aggregate here must instead carry TTLMsSet true with a Scope that is
// neither public nor private, and feeding it through a PersistentToolCache
// must call neither Save nor Delete.
func TestListTools_Modern_CacheHint_UnknownScopePlusNoHint_NeitherSavedNorDeleted(t *testing.T) {
	t.Parallel()

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			// No ttlMs, no cacheScope at all — no hint offered.
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t1","inputSchema":{"type":"object"}}],"nextCursor":"next"}}`)
		case 2:
			// A hint IS offered (ttlMs present), but cacheScope is neither
			// "public" nor "private" — parseCacheHint leaves Scope at "".
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t2","inputSchema":{"type":"object"}}],"ttlMs":3000,"cacheScope":"unrecognized-value"}}`)
		default:
			t.Errorf("unexpected request #%d", n)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}

	if !listing.Cache.TTLMsSet {
		t.Fatal("Cache.TTLMsSet = false, want true — the unknown-scope page's own hint must still size the freshness window")
	}
	if listing.Cache.TTLMs != 3000 {
		t.Errorf("Cache.TTLMs = %d, want 3000 (the only page that carried a hint at all)", listing.Cache.TTLMs)
	}
	if listing.Cache.Scope == mcp.CacheScopePublic || listing.Cache.Scope == mcp.CacheScopePrivate {
		t.Errorf("Cache.Scope = %q, want neither %q nor %q — unproven, not masked by the other page's silence",
			listing.Cache.Scope, mcp.CacheScopePublic, mcp.CacheScopePrivate)
	}

	store := &fakeToolStore{}
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		return listing, nil
	}
	cache := mcp.NewPersistentToolCache(fetcher, time.Hour, store)
	if err := cache.RefreshServer(context.Background(), "srv"); err != nil {
		t.Fatalf("RefreshServer: %v", err)
	}
	saves, deletes := store.counts()
	if saves != 0 {
		t.Errorf("store.Save called %d times, want 0 — an unknown-scope aggregate must never be persisted", saves)
	}
	if deletes != 0 {
		t.Errorf("store.Delete called %d times, want 0 — unproven is not the same as private; nothing to evict either", deletes)
	}
}

// ---- Public hint plus a no-hint page: still saved ---------------------------

// TestListTools_Modern_CacheHint_PublicHintPlusNoHint_Saved is the positive
// counterpart of the unknown-scope test above: a page offering no hint at
// all, alongside a page whose hint explicitly grants "public" scope, still
// results in an aggregate persistListing DOES persist — the "no hint at all
// on any page" collapse (rule 3 of ListTools' own CacheHint aggregation doc)
// only ever suppresses the TTL opinion, never the persist decision itself,
// as long as no page was private or unknown-scope.
func TestListTools_Modern_CacheHint_PublicHintPlusNoHint_Saved(t *testing.T) {
	t.Parallel()

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			// No ttlMs, no cacheScope at all — no hint offered.
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t1","inputSchema":{"type":"object"}}],"nextCursor":"next"}}`)
		case 2:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t2","inputSchema":{"type":"object"}}],"ttlMs":3000,"cacheScope":"public"}}`)
		default:
			t.Errorf("unexpected request #%d", n)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}

	if listing.Cache.TTLMsSet {
		t.Errorf("Cache.TTLMsSet = true, want false — one page offered no hint at all, so the aggregate carries no TTL opinion, "+
			"got Cache = %+v", listing.Cache)
	}

	store := &fakeToolStore{}
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		return listing, nil
	}
	cache := mcp.NewPersistentToolCache(fetcher, time.Hour, store)
	if err := cache.RefreshServer(context.Background(), "srv"); err != nil {
		t.Fatalf("RefreshServer: %v", err)
	}
	saves, deletes := store.counts()
	if saves != 1 {
		t.Errorf("store.Save called %d times, want 1 — no page ever claimed private or unknown scope, so the aggregate must still persist", saves)
	}
	if deletes != 0 {
		t.Errorf("store.Delete called %d times, want 0", deletes)
	}
}
