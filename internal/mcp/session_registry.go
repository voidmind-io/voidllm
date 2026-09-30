package mcp

import (
	"container/list"
	"strconv"
	"strings"
	"sync"
)

// maxSessionsPerScope bounds how many legacy Mcp-Session-Id values
// SessionRegistry remembers per (server, SessionScope) as having actually
// been minted or refreshed by the upstream for that scope. 64 comfortably
// covers a single organization/key pair juggling several concurrent MCP
// connections to the same server at once — the ordinary shape for an agent
// framework or IDE integration that opens more than one session in parallel
// — while staying a small, fixed amount of memory (a session ID is at most
// MaxSessionIDLength bytes) even for a global server shared by many
// organizations, since it bounds each scope's set independently. Eviction is
// oldest-first (see scopeSessions.record): if a legitimate, still-valid
// session ever falls out under this bound, the next lookup carrying it
// simply treats it as unrecognized — HandleMCPProxy discards that one header
// rather than failing outright, and the caller reinitializes.
const maxSessionsPerScope = 64

// MaxSessionIDLength is the hard ceiling on how long a single session ID may
// be before SessionRegistry will remember or recognize it at all. A real
// Mcp-Session-Id is, in practice, at most a few dozen bytes (an opaque
// server-generated token), but nothing in the spec enforces that — an
// unbounded value here would let a malicious or misbehaving upstream, or a
// caller fabricating one, turn every recorded or checked session ID into an
// arbitrarily large allocation. 512 bytes is generous enough for any
// legitimate token shape while keeping a single scope's worst case
// (maxSessionsPerScope entries) a small, fixed amount of memory. A session ID
// longer than this is never recorded by Record and is always reported
// unrecognized by Known — so HandleMCPProxy discards it exactly as it would
// an ordinary unrecognized session, never relaying it upstream.
//
// Exported so a caller that mints session IDs on the upstream's behalf, or
// mirrors one back downstream — HandleMCPProxy, on the response side of the
// transparent proxy path — can apply this exact same ceiling before ever
// handing an over-long ID to a caller who could never present it back
// successfully: an ID Record would silently refuse to remember is not a
// session either side of this proxy can usefully agree on.
const MaxSessionIDLength = 512

// maxOrgsPerServer bounds how many distinct organizations SessionRegistry
// retains scope state for, per server ID, evicting the least-recently-used
// organization first once a new one would exceed it. maxKeysPerOrg, its
// sibling, applies the same bound one level down: how many distinct API keys
// are remembered within a single organization.
//
// This two-level split replaces a single flat per-server bound this registry
// previously used. Under that flat bound, every (organization, API key)
// scope on a server shared the same LRU regardless of which organization it
// belonged to — and an org_admin may create as many API keys as they like
// (see NewClientSessionScope's doc for why a key, not just an organization,
// gets its own scope). Enough of one organization's own keys, each opening
// its own sessionless legacy "initialize" request against a shared global
// server, could therefore evict a completely unrelated organization's scope
// purely by volume, even though that other organization's scope was the more
// recently used of the two — a targeted disruption of session continuity
// across a tenant boundary this project's org/team/key isolation otherwise
// takes as its core premise. Bounding organizations first means a key can
// only ever evict a scope belonging to its OWN organization; bounding again
// within that organization (maxKeysPerOrg) means an organization can only
// ever evict its OWN prior keys, never a sibling organization's.
//
// 512 organizations times 16 keys per organization is 8192 scopes in the
// worst case — roughly twice the previous flat bound's 4096, deliberately:
// splitting one shared budget across two independent axes needs a little
// headroom so that ordinary, legitimate traffic spread across many
// organizations, each with a handful of keys, does not make either axis'
// cap a near-certainty on its own. It remains the same order of magnitude —
// a small, fixed amount of memory per server regardless of request volume —
// and every reason an eviction here is graceful degradation rather than a
// correctness problem still applies exactly as it did under the flat bound
// (see SessionRegistry's own doc): an evicted organization or key simply has
// its remembered sessions forgotten, and the next request carrying one is
// treated as unrecognized and dropped, exactly as an entirely fresh guess
// would be.
const maxOrgsPerServer = 512

// maxKeysPerOrg bounds how many distinct API keys SessionRegistry retains
// scope state for within a single organization on a single server — see
// maxOrgsPerServer's doc for the full reasoning; this is that bound's other
// axis.
const maxKeysPerOrg = 16

// SessionRegistry tracks which legacy Mcp-Session-Id values an upstream has
// issued to a given tenant scope, so the transparent proxy path
// (HandleMCPProxy) can refuse to relay a caller-supplied session identifier
// it was never issued, instead of relaying whatever the caller happens to
// send on faith. It is deliberately independent of any single
// *HTTPTransport's lifetime: an ad-hoc transport built for a single
// cache-miss request (buildAdHocTransport) is closed and discarded once that
// request completes, and a fresh HTTPTransport is built again from scratch on
// process restart — in both cases, a session store that lived on the
// transport or its resolved eraBinding, as it previously did, would vanish
// out from under a legitimate follow-up request before that request ever
// arrived. A *SessionRegistry is constructed once and held on admin.Handler,
// so it survives every ad-hoc transport and every cache miss. It does not
// survive a process restart — like every other in-memory cache this repo
// relies on (see CLAUDE.md's single-instance-without-Redis deployment
// constraint), it is rebuilt empty — but that is the same graceful
// degradation an eviction under maxOrgsPerServer, maxKeysPerOrg, or
// maxSessionsPerScope already provides: a rebuilt-empty registry simply
// treats every caller's session as unrecognized until it is reissued.
//
// Entries are also removed proactively, not only via eviction: Reconcile
// drops every server ID that has left the active set (deleted, or merely
// deactivated) so its scopes and sessions do not outlive the server itself
// for the rest of the process' life. When Reconcile removes an entry this
// way it also tombstones the server ID (see the removed/removedLRU fields'
// own doc): Record refuses to recreate an entry for a tombstoned server ID,
// so a Record call already in flight when Reconcile ran cannot resurrect the
// very entry Reconcile just removed (docs/mcp-v2.md review round, Fund 5).
// Record still creates an entry on demand, exactly as before, for any server
// ID Reconcile has never removed — including a server this registry has
// never heard of at all, e.g. the very first request against a freshly
// started process, before Reconcile has run even once — so the ordinary
// "grows lazily on first use" shape this type has always had is unaffected
// outside the narrow removed-then-late-Record race this closes. Reconcile
// itself clears a server ID's tombstone the moment that ID reappears in the
// active set it is given, so a reactivated server's very next Record call
// creates a genuinely fresh, empty entry rather than being permanently
// locked out.
//
// Entries are looked up by serverID (the MCP server's stable database ID,
// not its alias, which can be renamed) and a SessionScope. Record and Known
// are both safe for concurrent use from many goroutines at once — the
// ordinary shape of MCP traffic against a shared upstream (docs/mcp-v2.md
// §1a) — via one mutex per server that a lookup only ever holds for the
// duration of a small map/list operation, never across an actual upstream
// round trip.
type SessionRegistry struct {
	mu      sync.Mutex
	servers map[string]*serverSessionRegistry
	// removed and removedLRU remember, bounded and oldest-evicted-first, the
	// server IDs Reconcile has most recently removed from the active set —
	// see maxRemovedServers' own doc for the bound and Record's own doc for
	// why this exists: a tombstoned server ID refuses a Record call instead
	// of silently recreating the very entry Reconcile just removed.
	removed    map[string]*list.Element
	removedLRU *list.List // string values (server IDs); back = oldest tombstone
}

// NewSessionRegistry returns an empty *SessionRegistry, ready for concurrent
// use.
func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{
		servers:    make(map[string]*serverSessionRegistry),
		removed:    make(map[string]*list.Element),
		removedLRU: list.New(),
	}
}

// maxRemovedServers bounds how many recently deactivated-or-deleted server
// IDs SessionRegistry remembers as tombstoned, so a late Record call cannot
// resurrect one (see Record's own doc; docs/mcp-v2.md review round, Fund 5).
// Bounded and oldest-evicted-first for the same reason every other bound in
// this file is: an org_admin can create and deactivate/delete MCP servers at
// will, and each one mints a fresh UUIDv7 ID that is never reused (see
// CLAUDE.md's UUID convention), so an unbounded tombstone set would grow for
// the lifetime of the process — and, unlike maxOrgsPerServer/maxKeysPerOrg,
// this is driven entirely by admin mutations, not by request volume, so it
// needs its own independent cap rather than inheriting one of theirs. Once
// full, the oldest tombstone is evicted to make room for a new one — the
// evicted server ID reverts to being treated as "never removed", reopening
// the narrow, race-only resurrection window this mechanism closes for that
// one server, but only after maxRemovedServers MORE servers have since been
// deactivated or deleted, by which point a request racing the original
// deactivation has long since completed or timed out — the same graceful
// degradation every other bound in this file already provides.
const maxRemovedServers = 4096

// serverSessionRegistry is one SessionRegistry's state for a single server
// ID: a two-level LRU — organizations at the top (bounded by
// maxOrgsPerServer), and within each organization, the API keys that have
// built a scope against this server (bounded by maxKeysPerOrg) — instead of
// one flat LRU of scopes (see maxOrgsPerServer's doc for why). The
// most-recently-used organization is kept at the front of orgLRU, and,
// within each *orgSessions, its most-recently-used key at the front of its
// own keyLRU.
type serverSessionRegistry struct {
	mu     sync.Mutex
	orgLRU *list.List // *orgSessions values; front = most recently used org
	orgs   map[string]*list.Element
}

// orgSessions is one serverSessionRegistry's state for a single organization:
// the bounded, least-recently-used-evicted set of API keys (maxKeysPerOrg)
// that have built a SessionScope against this server for this organization,
// each holding its own *keyScopes bucket (see keyScopes' doc for why that,
// and not *scopeSessions directly, is what this LRU now holds).
type orgSessions struct {
	orgID  string
	keyLRU *list.List // *keyScopes values; front = most recently used key
	keys   map[string]*list.Element
}

// keyScopes is one API key's bucket within one organization on one server:
// every distinct, exact SessionScope string that scopeBucket has ever mapped
// to this (orgID, apiKeyID) pair, each holding its own independent
// *scopeSessions. In production this holds exactly one scope — the single
// string NewClientSessionScope produces for this exact pair, since
// HandleMCPProxy, SessionRegistry's only production caller, never builds a
// scope any other way (see scopeBucket's doc). It can hold more than one
// only when a caller passes Record or Known a scope that was never built via
// NewClientSessionScope and whose fallback bucket (see scopeBucket) happens
// to coincide with some genuinely encoded scope's decoded bucket — a shape
// this package's own unit tests construct on purpose to prove the two stay
// isolated. Keying scopeSessions by the exact scope string here, rather than
// by the bucket alone, is what keeps Record and Known's session bookkeeping
// partitioned per exact scope even though the (orgID, apiKeyID) bucket two
// different scopes land in can be the same (see touch's doc): the bucket
// this type lives in decides only which LRU entry rises and falls together
// for maxOrgsPerServer/maxKeysPerOrg purposes, never which sessions a lookup
// for one particular scope can see.
//
// apiKeyID is the key this bucket belongs to, recorded so touchKey can find
// and remove the right entry from its parent orgSessions.keys on eviction.
type keyScopes struct {
	apiKeyID string
	scopes   map[string]*scopeSessions
}

// scopeSessions is one exact SessionScope's bookkeeping, within one API key
// bucket on one server, for the bounded, oldest-first set of session IDs the
// upstream has actually issued to it.
//
// sessions holds the values also present as keys of sessionSet, oldest
// first, so the oldest can be evicted in O(1) once maxSessionsPerScope is
// exceeded. sessionSet exists alongside it purely for O(1) membership lookup
// on Known's hot path — recomputing this with a linear scan of sessions on
// every single lookup would make this check slower than the upstream request
// it exists to protect.
type scopeSessions struct {
	sessions   []string
	sessionSet map[string]struct{}
}

// record remembers sessionID as issued for e, evicting the oldest recorded
// session first once more than maxSessionsPerScope would otherwise be
// tracked. The freed slot is cleared to the empty string before the backing
// slice is re-sliced: re-slicing alone (sessions = sessions[1:]) advances the
// visible window but leaves the evicted string reachable through the
// underlying array's now-hidden first element, keeping it alive until that
// array itself is garbage collected. Clearing the slot first drops the last
// reference immediately instead.
func (e *scopeSessions) record(sessionID string) {
	if _, ok := e.sessionSet[sessionID]; ok {
		return
	}
	if len(e.sessions) >= maxSessionsPerScope {
		oldest := e.sessions[0]
		e.sessions[0] = ""
		e.sessions = e.sessions[1:]
		delete(e.sessionSet, oldest)
	}
	e.sessions = append(e.sessions, sessionID)
	e.sessionSet[sessionID] = struct{}{}
}

// known reports whether sessionID was previously recorded via record.
func (e *scopeSessions) known(sessionID string) bool {
	_, ok := e.sessionSet[sessionID]
	return ok
}

// Record remembers that the upstream identified by serverID minted or
// refreshed sessionID for scope, so a later Known call for the same
// (serverID, scope, sessionID) reports it as recognized. It is a no-op for an
// empty sessionID or one longer than MaxSessionIDLength (see that constant's
// doc), creating an entry for serverID on first use exactly as it always
// has — UNLESS serverID is currently tombstoned (see r.removed's own doc),
// in which case it is also a no-op.
//
// The tombstone check is what closes the race docs/mcp-v2.md review round
// Fund 5 identified: without it, a Record call already in flight when an
// admin deactivated or deleted serverID — Reconcile having just removed its
// entry — would recreate that entry after the fact, indistinguishable from a
// legitimately active server, and nothing would ever remove it again once
// the admin reactivated serverID before the next Reconcile call (which would
// otherwise have pruned it a second time). Because Reconcile tombstones
// serverID in that same removal (and clears the tombstone the moment
// serverID reappears in an active set Reconcile is given — see that
// method's own doc), a late Record for a server still tombstoned finds
// nothing to record into and does nothing, while a server ID Reconcile has
// never touched at all — including the ordinary "very first request against
// a freshly started process, before Reconcile has run even once" shape this
// type has always supported — creates an entry exactly as before. Safe for
// concurrent use.
func (r *SessionRegistry) Record(serverID string, scope SessionScope, sessionID string) {
	if sessionID == "" || len(sessionID) > MaxSessionIDLength {
		return
	}
	srv, ok := r.serverForIfNotRemoved(serverID)
	if !ok {
		return
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.touch(scope).record(sessionID)
}

// Known reports whether sessionID was previously recorded via Record for the
// same (serverID, scope) pair. An empty sessionID, or one longer than
// MaxSessionIDLength, is always reported unknown — the latter can never have
// been recorded in the first place (see that constant's doc), so treating it
// as unknown here rather than panicking or truncating keeps the two methods'
// notion of "too long" consistent. Safe for concurrent use. A lookup counts
// as use of scope's organization and key for maxOrgsPerServer's and
// maxKeysPerOrg's LRU eviction, exactly like Record, so an actively-checked
// scope is never the one evicted while it is still in active use.
func (r *SessionRegistry) Known(serverID string, scope SessionScope, sessionID string) bool {
	if sessionID == "" || len(sessionID) > MaxSessionIDLength {
		return false
	}
	r.mu.Lock()
	srv, ok := r.servers[serverID]
	r.mu.Unlock()
	if !ok {
		return false
	}

	orgID, apiKeyID := scopeBucket(scope)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	orgEl, ok := srv.orgs[orgID]
	if !ok {
		return false
	}
	srv.orgLRU.MoveToFront(orgEl)
	org := orgEl.Value.(*orgSessions)

	keyEl, ok := org.keys[apiKeyID]
	if !ok {
		return false
	}
	org.keyLRU.MoveToFront(keyEl)

	entry, ok := keyEl.Value.(*keyScopes).scopes[string(scope)]
	if !ok {
		return false
	}
	return entry.known(sessionID)
}

// Reconcile removes every server-ID entry this registry holds whose ID is
// not present in activeServerIDs, so a server that has been deleted, or has
// merely left the active set (deactivated), does not keep every scope and
// session it ever saw alive for the rest of the process' life — nothing else
// ever shrinks r.servers itself, only the bounded LRUs one level down. Every
// entry removed this way is also tombstoned (see r.removed's own doc), and
// every ID in activeServerIDs that WAS tombstoned has that tombstone
// cleared, so a server that was deactivated and is now active again is
// exactly as eligible for Record to create a fresh entry as a server this
// registry has never seen at all (docs/mcp-v2.md review round, Fund 5).
//
// It is called from admin.Handler.refreshMCPCaches with the exact set
// LoadAllActiveMCPServers just returned, after every MCP server mutation
// (create, update, delete, activate/deactivate): reusing that query, rather
// than adding a dedicated per-mutation Delete call, also prunes a server
// that left the active set through any of those paths, not only the one a
// dedicated call happened to be wired into. Pruning a merely-deactivated
// server is deliberate, not just a side effect of reusing the query: a
// deactivated server should not still be relaying sessions, and reactivating
// it later really does start it fresh — a late Record for the removed entry
// can no longer resurrect it while the tombstone stands (Record is a no-op
// against a tombstoned server ID), and the tombstone itself is cleared the
// moment Reconcile sees the server active again — the same graceful
// degradation an ordinary maxOrgsPerServer/maxSessionsPerScope eviction
// already provides (see SessionRegistry's own doc). Safe for concurrent use
// with Record and Known.
func (r *SessionRegistry) Reconcile(activeServerIDs []string) {
	keep := make(map[string]struct{}, len(activeServerIDs))
	for _, id := range activeServerIDs {
		keep[id] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.servers {
		if _, ok := keep[id]; !ok {
			delete(r.servers, id)
			r.tombstone(id)
		}
	}
	for id := range keep {
		r.untombstone(id)
	}
}

// tombstone marks serverID as just removed from the active set, evicting the
// oldest tombstone first once more than maxRemovedServers would otherwise be
// remembered (see that constant's own doc). A no-op if serverID is already
// tombstoned — Reconcile calls this once per removed ID per call, but
// repeated Reconcile calls across ticks would otherwise re-tombstone (and
// therefore keep bumping to the front of removedLRU) a server that has
// stayed inactive the whole time, which is not a "use" worth refreshing the
// eviction order for. Callers must hold r.mu.
func (r *SessionRegistry) tombstone(serverID string) {
	if _, ok := r.removed[serverID]; ok {
		return
	}
	if len(r.removed) >= maxRemovedServers {
		if oldest := r.removedLRU.Back(); oldest != nil {
			r.removedLRU.Remove(oldest)
			delete(r.removed, oldest.Value.(string))
		}
	}
	r.removed[serverID] = r.removedLRU.PushFront(serverID)
}

// untombstone clears serverID's tombstone, if it has one, so Record can
// create a fresh entry for it again. A no-op if serverID was never
// tombstoned — the ordinary case for the overwhelming majority of IDs
// Reconcile is called with, since most active servers were never removed in
// the first place. Callers must hold r.mu.
func (r *SessionRegistry) untombstone(serverID string) {
	if el, ok := r.removed[serverID]; ok {
		r.removedLRU.Remove(el)
		delete(r.removed, serverID)
	}
}

// serverForIfNotRemoved returns r's *serverSessionRegistry for serverID,
// creating an empty one on first use — UNLESS serverID is currently
// tombstoned (see r.removed's own doc), in which case it returns (nil,
// false) and creates nothing. This is Record's sole entry point into
// r.servers; every other reader (Known) looks r.servers up directly without
// ever creating an entry. This is the only place r.mu itself is held; once a
// *serverSessionRegistry exists, all further access to it goes through its
// own mu instead, so lookups against different servers never contend with
// one another.
func (r *SessionRegistry) serverForIfNotRemoved(serverID string) (*serverSessionRegistry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, tombstoned := r.removed[serverID]; tombstoned {
		return nil, false
	}
	srv, ok := r.servers[serverID]
	if !ok {
		srv = &serverSessionRegistry{orgLRU: list.New(), orgs: make(map[string]*list.Element)}
		r.servers[serverID] = srv
	}
	return srv, true
}

// scopeBucket splits scope into the (orgID, apiKeyID) pair
// serverSessionRegistry's two-level LRU is partitioned by, reversing
// NewClientSessionScope's length-prefixed encoding when scope was built that
// way (see decodeClientSessionScope). For any scope not shaped like
// NewClientSessionScope's own output — every literal SessionScope value this
// package's own unit tests construct by hand, for instance — scope becomes
// its own singleton organization holding a single key, both named after
// scope's raw text: fully isolated from every other scope, exactly as under
// the flat structure this replaced, just bounded by maxOrgsPerServer instead
// of the old, single maxScopesPerServer. This fallback is never exercised on
// real MCP traffic: HandleMCPProxy, SessionRegistry's only production
// caller, always builds scope via NewClientSessionScope.
func scopeBucket(scope SessionScope) (orgID, apiKeyID string) {
	if orgID, apiKeyID, ok := decodeClientSessionScope(scope); ok {
		return orgID, apiKeyID
	}
	return string(scope), ""
}

// decodeClientSessionScope reverses NewClientSessionScope's encoding,
// recovering the (orgID, apiKeyID) pair scope was built from. Record and
// Known still key their actual session bookkeeping by the full, opaque scope
// value passed to touch — via keyScopes.scopes, one level below the
// (orgID, apiKeyID) bucket this function's result only ever selects (see
// keyScopes' doc) — so a decoding mistake here could only ever misplace
// which LRU bucket a scope's eviction is grouped under, and, in turn, which
// OTHER scopes it shares eviction fate with — never which sessions a lookup
// for that exact scope returns.
func decodeClientSessionScope(scope SessionScope) (orgID, apiKeyID string, ok bool) {
	s := string(scope)
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return "", "", false
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil || n < 0 || i+1+n > len(s) {
		return "", "", false
	}
	rest := s[i+1:]
	return rest[:n], rest[n:], true
}

// touch returns srv's *scopeSessions for the exact value of scope, creating
// the organization and key buckets scope decodes to (see scopeBucket) on
// first use — evicting the least-recently-used organization first if a new
// one would exceed maxOrgsPerServer, and, within that organization, the
// least-recently-used key first if a new one would exceed maxKeysPerOrg —
// and moving both to the front of their respective LRUs either way, exactly
// as before. Within the resulting key bucket, the returned *scopeSessions is
// additionally keyed by scope's own full string (see keyScopes' doc): the
// (orgID, apiKeyID) bucket decides only LRU grouping and the two-level
// bound, never which scope's sessions a lookup for scope can see. Callers
// must hold srv.mu.
func (srv *serverSessionRegistry) touch(scope SessionScope) *scopeSessions {
	orgID, apiKeyID := scopeBucket(scope)
	return srv.touchOrg(orgID).touchKey(apiKeyID).touchScope(string(scope))
}

// touchOrg returns srv's *orgSessions for orgID, creating one — evicting the
// least-recently-used organization first if this creation would exceed
// maxOrgsPerServer — and moving it to the front of srv.orgLRU either way.
// Callers must hold srv.mu.
func (srv *serverSessionRegistry) touchOrg(orgID string) *orgSessions {
	if el, ok := srv.orgs[orgID]; ok {
		srv.orgLRU.MoveToFront(el)
		return el.Value.(*orgSessions)
	}
	if len(srv.orgs) >= maxOrgsPerServer {
		if oldest := srv.orgLRU.Back(); oldest != nil {
			srv.orgLRU.Remove(oldest)
			delete(srv.orgs, oldest.Value.(*orgSessions).orgID)
		}
	}
	org := &orgSessions{orgID: orgID, keyLRU: list.New(), keys: make(map[string]*list.Element)}
	srv.orgs[orgID] = srv.orgLRU.PushFront(org)
	return org
}

// touchKey returns org's *keyScopes bucket for apiKeyID, creating one —
// evicting the least-recently-used key first if this creation would exceed
// maxKeysPerOrg — and moving it to the front of org.keyLRU either way.
// Callers must hold the owning serverSessionRegistry's mu.
func (org *orgSessions) touchKey(apiKeyID string) *keyScopes {
	if el, ok := org.keys[apiKeyID]; ok {
		org.keyLRU.MoveToFront(el)
		return el.Value.(*keyScopes)
	}
	if len(org.keys) >= maxKeysPerOrg {
		if oldest := org.keyLRU.Back(); oldest != nil {
			org.keyLRU.Remove(oldest)
			delete(org.keys, oldest.Value.(*keyScopes).apiKeyID)
		}
	}
	bucket := &keyScopes{apiKeyID: apiKeyID, scopes: make(map[string]*scopeSessions)}
	org.keys[apiKeyID] = org.keyLRU.PushFront(bucket)
	return bucket
}

// touchScope returns ks's *scopeSessions for the exact full scope string,
// creating an empty one on first use. Unlike touchOrg and touchKey, this
// performs no LRU eviction of its own: in production ks holds exactly one
// scope (see keyScopes' doc), so this map never grows beyond a single entry
// on the only path real MCP traffic takes. The case where a second, distinct
// scope string lands in the same bucket — and therefore adds a second entry
// here — is exercised only by this package's own tests (see keyScopes' doc);
// it does not defeat maxOrgsPerServer/maxKeysPerOrg's memory bound, since
// scopeBucket's fallback for a non-encoded scope always names the bucket
// after that scope's own full text, so distinct non-encoded scopes still
// land in distinct buckets. Callers must hold the owning
// serverSessionRegistry's mu.
func (ks *keyScopes) touchScope(scope string) *scopeSessions {
	if entry, ok := ks.scopes[scope]; ok {
		return entry
	}
	entry := &scopeSessions{sessionSet: make(map[string]struct{})}
	ks.scopes[scope] = entry
	return entry
}

// NewClientSessionScope combines an organization ID and an API key ID into a
// single SessionScope for use with SessionRegistry, encoded so that no pair
// of inputs can ever collide with a different pair, regardless of whether
// either value happens to contain whatever byte this encoding uses as a
// delimiter (docs/mcp-v2.md review finding C): the encoding is
// "<decimal length of orgID>:<orgID><apiKeyID>" — since a decimal digit
// sequence can never itself contain the ':' that terminates it, the boundary
// between the length prefix and orgID is always unambiguous, which in turn
// makes the boundary between orgID and apiKeyID unambiguous, with no
// assumption whatsoever about what characters either input may contain.
// decodeClientSessionScope reverses this exact encoding.
//
// This is unrelated to the plain org-only SessionScope Call uses for its own
// legacy handshake (see SessionScope's doc) — HandleMCPProxy's transparent
// proxy path scopes SessionRegistry lookups one level finer, per
// (organization, API key) rather than per organization alone, so that one
// key issuing many sessionless legacy "initialize" requests can only evict
// its OWN prior sessions from SessionRegistry's bounded per-key set
// (maxSessionsPerScope), never a sibling key's — and, one level up, so that
// one organization's own keys can only ever evict that SAME organization's
// prior keys (maxKeysPerOrg), never a sibling organization's (see
// maxOrgsPerServer's doc).
func NewClientSessionScope(orgID, apiKeyID string) SessionScope {
	return SessionScope(strconv.Itoa(len(orgID)) + ":" + orgID + apiKeyID)
}
