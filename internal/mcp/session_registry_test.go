package mcp_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file is the unit-level test suite for mcp.SessionRegistry itself —
// the mechanism HandleMCPProxy (internal/api/admin/mcp_proxy.go) uses to
// decide whether a caller-supplied Mcp-Session-Id was actually issued to
// that caller's own (organization, API key) scope, independent of any one
// *mcp.HTTPTransport's lifetime (see SessionRegistry's own doc). The
// attack-shaped, full-stack counterparts that drive the real HTTP proxy
// route live in internal/api/admin/mcp_proxy_session_binding_test.go; this
// file exercises the registry's bookkeeping directly and exhaustively,
// including the bounds (maxSessionsPerScope, maxOrgsPerServer,
// maxKeysPerOrg) that would be prohibitively slow to reach by proxying
// thousands of real HTTP requests through a fake upstream.
//
// maxOrgsPerServer/maxKeysPerOrg replace what used to be a single flat
// maxScopesPerServer bound (see maxOrgsPerServer's own doc for the full
// reasoning): under the old flat LRU, every (organization, API key) scope on
// a server shared the same eviction budget regardless of which organization
// it belonged to, so an org_admin minting many of their OWN organization's
// API keys could evict a completely unrelated organization's scope purely by
// volume. The two-level split bounds organizations first (maxOrgsPerServer)
// and, within each organization, its own keys (maxKeysPerOrg) — so a key can
// only ever evict a scope belonging to its OWN organization, and an
// organization can only ever evict its OWN prior keys.

// ---- Record/Known basics ---------------------------------------------------

func TestSessionRegistry_RecordThenKnown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		recordVal string
		checkVal  string
		wantKnown bool
	}{
		{
			name:      "a recorded session is known",
			recordVal: "session-abc",
			checkVal:  "session-abc",
			wantKnown: true,
		},
		{
			name:      "an unrecorded session is unknown",
			recordVal: "session-abc",
			checkVal:  "session-never-recorded",
			wantKnown: false,
		},
		{
			name:      "the empty session ID is never recorded, so it is never known",
			recordVal: "",
			checkVal:  "",
			wantKnown: false,
		},
		{
			name:      "a session ID exactly at maxSessionIDLength (512 bytes) is recorded and known",
			recordVal: strings.Repeat("a", 512),
			checkVal:  strings.Repeat("a", 512),
			wantKnown: true,
		},
		{
			name:      "a session ID one byte over maxSessionIDLength (513 bytes) is never recorded",
			recordVal: strings.Repeat("a", 513),
			checkVal:  strings.Repeat("a", 513),
			wantKnown: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := mcp.NewSessionRegistry()
			const serverID = "server-1"
			const scope = mcp.SessionScope("scope-1")

			reg.Record(serverID, scope, tc.recordVal)
			if got := reg.Known(serverID, scope, tc.checkVal); got != tc.wantKnown {
				t.Errorf("Known(%q) = %v, want %v", tc.checkVal, got, tc.wantKnown)
			}
		})
	}
}

// TestSessionRegistry_Known_UnseenServerOrScope_NeverPanicsAlwaysFalse
// verifies Known's zero-value behavior: a server ID or scope the registry
// has never seen a Record call for must report false, not panic — this is
// the ordinary shape of the very FIRST request through a fresh registry
// (e.g. after a process restart, see SessionRegistry's own doc on why it
// does not survive one).
func TestSessionRegistry_Known_UnseenServerOrScope_NeverPanicsAlwaysFalse(t *testing.T) {
	t.Parallel()

	reg := mcp.NewSessionRegistry()

	if got := reg.Known("never-seen-server", "never-seen-scope", "some-session"); got {
		t.Error("Known() on a completely fresh registry = true, want false")
	}

	reg.Record("server-1", "scope-1", "session-1")

	if got := reg.Known("server-1", "never-seen-scope", "session-1"); got {
		t.Error("Known() for a scope never recorded on a known server = true, want false")
	}
	if got := reg.Known("never-seen-server", "scope-1", "session-1"); got {
		t.Error("Known() for a server never recorded, even with a real scope/session pair = true, want false")
	}
}

// TestSessionRegistry_IsolatedPerServerAndScope verifies that a session
// recorded for one (serverID, scope) pair is never known under any other
// (serverID, scope) pair — the core tenant-isolation property the whole
// mechanism exists to provide.
func TestSessionRegistry_IsolatedPerServerAndScope(t *testing.T) {
	t.Parallel()

	reg := mcp.NewSessionRegistry()

	reg.Record("server-A", "scope-1", "session-x")

	cases := []struct {
		name     string
		server   string
		scope    mcp.SessionScope
		wantKnow bool
	}{
		{"same server, same scope", "server-A", "scope-1", true},
		{"same server, different scope", "server-A", "scope-2", false},
		{"different server, same scope", "server-B", "scope-1", false},
		{"different server, different scope", "server-B", "scope-2", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reg.Known(tc.server, tc.scope, "session-x"); got != tc.wantKnow {
				t.Errorf("Known(%q, %q, session-x) = %v, want %v", tc.server, tc.scope, got, tc.wantKnow)
			}
		})
	}
}

// ---- maxSessionsPerScope: 64 sessions per scope, oldest evicted first -----

// TestSessionRegistry_MaxSessionsPerScope_OldestEvictedFirst is the unit-level
// regression test for maxSessionsPerScope: exactly 64 sessions are
// remembered per (server, scope); the 65th recording evicts the single
// oldest, leaving everything else — including sessions recorded in between —
// untouched.
func TestSessionRegistry_MaxSessionsPerScope_OldestEvictedFirst(t *testing.T) {
	t.Parallel()

	reg := mcp.NewSessionRegistry()
	const serverID = "server-1"
	const scope = mcp.SessionScope("org-at-the-cap")
	const sessionCap = 64

	sessions := make([]string, sessionCap)
	for i := range sessions {
		sessions[i] = fmt.Sprintf("session-%03d", i)
		reg.Record(serverID, scope, sessions[i])
	}

	// All 64 must be known — no eviction has happened yet.
	for i, sid := range sessions {
		if !reg.Known(serverID, scope, sid) {
			t.Fatalf("session #%d (%q) not known before the 65th recording, want known", i, sid)
		}
	}

	// The 65th recording evicts the very first one.
	const session65 = "session-065"
	reg.Record(serverID, scope, session65)

	if reg.Known(serverID, scope, sessions[0]) {
		t.Errorf("oldest session %q is still known after the 65th recording, want it evicted", sessions[0])
	}
	for i := 1; i < sessionCap; i++ {
		if !reg.Known(serverID, scope, sessions[i]) {
			t.Errorf("session #%d (%q) is no longer known after the 65th recording, want only the single oldest evicted", i, sessions[i])
		}
	}
	if !reg.Known(serverID, scope, session65) {
		t.Error("the 65th session itself is not known immediately after being recorded")
	}
}

// TestSessionRegistry_MaxSessionsPerScope_ReRecordingKnownSessionDoesNotEvict
// verifies that re-recording a session ID the registry already knows about
// (the legacy era refreshes the session on every response, see
// http_transport.go's doCall) is a no-op for eviction purposes: it must not
// itself count as a new entry and must not evict anything.
func TestSessionRegistry_MaxSessionsPerScope_ReRecordingKnownSessionDoesNotEvict(t *testing.T) {
	t.Parallel()

	reg := mcp.NewSessionRegistry()
	const serverID = "server-1"
	const scope = mcp.SessionScope("org-rerecord")
	const sessionCap = 64

	sessions := make([]string, sessionCap)
	for i := range sessions {
		sessions[i] = fmt.Sprintf("session-%03d", i)
		reg.Record(serverID, scope, sessions[i])
	}

	// Re-record the oldest session sessionCap-1 more times — if this
	// incorrectly counted as new entries, it alone would be enough to evict
	// everything else.
	for i := 0; i < sessionCap-1; i++ {
		reg.Record(serverID, scope, sessions[0])
	}

	for i, sid := range sessions {
		if !reg.Known(serverID, scope, sid) {
			t.Errorf("session #%d (%q) not known after re-recording session #0 repeatedly, want re-recording an "+
				"already-known session to never evict anything", i, sid)
		}
	}
}

// ---- maxOrgsPerServer: 512 organizations per server, LRU-evicted ----------

// TestSessionRegistry_MaxOrgsPerServer_LRUEvictsLeastRecentlyUsedOrg is the
// unit-level regression test for maxOrgsPerServer, the top level of the
// two-level split that replaced the old, flat maxScopesPerServer bound (see
// this file's own doc): a server accumulates at most 512 distinct
// organizations; once a new one would exceed that, the LEAST recently used
// org is evicted — not merely the oldest by creation order — and a Known
// lookup counts as use, exactly like Record. Every scope here is built via
// mcp.NewClientSessionScope, exactly as HandleMCPProxy — SessionRegistry's
// only production caller — always builds them.
func TestSessionRegistry_MaxOrgsPerServer_LRUEvictsLeastRecentlyUsedOrg(t *testing.T) {
	t.Parallel()

	reg := mcp.NewSessionRegistry()
	const serverID = "server-with-many-tenants"
	const capOrgs = 512 // maxOrgsPerServer

	// Fill the registry to exactly the cap: org-0000 .. org-0511, one key
	// each, in order — org-0000 is therefore the least recently used entry
	// once the loop finishes.
	orgSession := func(i int) (mcp.SessionScope, string) {
		scope := mcp.NewClientSessionScope(fmt.Sprintf("org-%04d", i), "key-1")
		return scope, fmt.Sprintf("session-for-org-%04d", i)
	}
	for i := 0; i < capOrgs; i++ {
		scope, sid := orgSession(i)
		reg.Record(serverID, scope, sid)
	}

	// Touch org-0000 via a Known lookup — this counts as use (see
	// SessionRegistry.Known's doc) and must move it to the front of the org
	// LRU, protecting it from the eviction about to happen.
	scope0, sid0 := orgSession(0)
	if !reg.Known(serverID, scope0, sid0) {
		t.Fatal("org-0000's session not known before touching it, test setup is broken")
	}

	// Adding one more organization beyond the cap must evict the LEAST
	// recently used org — org-0001, since org-0000 was just refreshed by the
	// Known call above — not org-0000.
	newScope, newSid := mcp.NewClientSessionScope("org-0512-the-overflow", "key-1"), "session-for-the-overflow-org"
	reg.Record(serverID, newScope, newSid)

	if !reg.Known(serverID, scope0, sid0) {
		t.Error("org-0000 was evicted despite being the most recently touched org, want the LRU eviction to spare it")
	}
	scope1, sid1 := orgSession(1)
	if reg.Known(serverID, scope1, sid1) {
		t.Error("org-0001 is still known after the overflow, want it to be the one evicted (least recently used)")
	}
	if !reg.Known(serverID, newScope, newSid) {
		t.Error("the newly added, 513th org is not known immediately after being recorded")
	}

	// An org untouched by either the eviction or the LRU refresh above
	// (org-0256, comfortably in the middle) must be completely unaffected.
	midScope, midSid := orgSession(256)
	if !reg.Known(serverID, midScope, midSid) {
		t.Error("an unrelated, untouched org (org-0256) lost its session after a single overflow eviction, want it unaffected")
	}
}

// TestSessionRegistry_MaxOrgsPerServer_EvictedOrgTakesAllItsKeysWithIt
// verifies the other half of maxOrgsPerServer's contract: when the
// least-recently-used ORGANIZATION is evicted, every one of ITS keys is
// evicted along with it in one shot — not just the single key that happened
// to be least recently used within that org.
func TestSessionRegistry_MaxOrgsPerServer_EvictedOrgTakesAllItsKeysWithIt(t *testing.T) {
	t.Parallel()

	reg := mcp.NewSessionRegistry()
	const serverID = "server-1"
	const capOrgs = 512
	const victimOrg = "org-0000"

	// The victim org has THREE distinct keys, all recorded — they must all
	// disappear together when the org itself is evicted.
	victimScopes := make([]mcp.SessionScope, 3)
	victimSids := make([]string, 3)
	for k := range victimScopes {
		victimScopes[k] = mcp.NewClientSessionScope(victimOrg, fmt.Sprintf("key-%d", k))
		victimSids[k] = fmt.Sprintf("session-victim-key-%d", k)
		reg.Record(serverID, victimScopes[k], victimSids[k])
	}
	for k := range victimScopes {
		if !reg.Known(serverID, victimScopes[k], victimSids[k]) {
			t.Fatalf("victim org's key #%d not known before filling the registry, test setup is broken", k)
		}
	}

	// Fill the remaining capOrgs-1 organizations (org-0001..org-0511), one
	// key each, so the registry sits at exactly capOrgs distinct orgs —
	// victimOrg is therefore the least recently used.
	for i := 1; i < capOrgs; i++ {
		scope := mcp.NewClientSessionScope(fmt.Sprintf("org-%04d", i), "key-0")
		reg.Record(serverID, scope, fmt.Sprintf("session-org-%04d", i))
	}

	// One more, brand new organization overflows the cap, evicting victimOrg
	// entirely.
	overflowScope := mcp.NewClientSessionScope("org-overflow", "key-0")
	reg.Record(serverID, overflowScope, "session-overflow")

	for k := range victimScopes {
		if reg.Known(serverID, victimScopes[k], victimSids[k]) {
			t.Errorf("victim org's key #%d is still known after the org itself was evicted, want ALL of its keys gone together", k)
		}
	}
}

// ---- maxKeysPerOrg: 16 keys per organization, LRU-evicted, isolated ------
// ---- from every sibling organization ---------------------------------------

// TestSessionRegistry_MaxKeysPerOrg_ChurnStaysWithinOrg_SiblingOrgUnaffected
// is the single most important regression test for this two-level split
// (see this file's own doc): the exact cross-tenant disruption the old flat
// LRU allowed — one organization's own API-key churn evicting a completely
// UNRELATED organization's scope purely by volume — must no longer be
// possible. Organization A mints far more than maxKeysPerOrg (16) API keys
// and keeps every one of their sessions active; Organization B's own,
// completely untouched session must survive throughout.
func TestSessionRegistry_MaxKeysPerOrg_ChurnStaysWithinOrg_SiblingOrgUnaffected(t *testing.T) {
	t.Parallel()

	reg := mcp.NewSessionRegistry()
	const serverID = "server-shared-globally"

	scopeB := mcp.NewClientSessionScope("org-B", "key-B")
	const sidB = "session-org-b"
	reg.Record(serverID, scopeB, sidB)
	if !reg.Known(serverID, scopeB, sidB) {
		t.Fatal("Org B's session not known right after recording it, test setup is broken")
	}

	// Org A creates far more keys than maxKeysPerOrg — an org_admin is free
	// to mint as many API keys as they like (see NewClientSessionScope's
	// doc) — each opening its own session against this server.
	const orgAKeyCount = 100
	for i := 0; i < orgAKeyCount; i++ {
		scope := mcp.NewClientSessionScope("org-A", fmt.Sprintf("key-%03d", i))
		reg.Record(serverID, scope, fmt.Sprintf("session-org-a-key-%03d", i))
	}

	if !reg.Known(serverID, scopeB, sidB) {
		t.Error("Org B's session was evicted by Org A's own key churn — a cross-tenant eviction, exactly what the " +
			"two-level (org, key) split exists to prevent")
	}
}

// TestSessionRegistry_MaxKeysPerOrg_LRUEvictsLeastRecentlyUsedKeyWithinOrg is
// the key-level counterpart of
// TestSessionRegistry_MaxOrgsPerServer_LRUEvictsLeastRecentlyUsedOrg: within
// a single organization, at most 16 distinct API keys are remembered; the
// 17th key evicts the LEAST recently used one of that SAME organization's
// own keys — never a sibling organization's — and a Known lookup counts as
// use, exactly like Record.
func TestSessionRegistry_MaxKeysPerOrg_LRUEvictsLeastRecentlyUsedKeyWithinOrg(t *testing.T) {
	t.Parallel()

	reg := mcp.NewSessionRegistry()
	const serverID = "server-1"
	const orgID = "org-with-many-keys"
	const capKeys = 16 // maxKeysPerOrg

	keySession := func(i int) (mcp.SessionScope, string) {
		scope := mcp.NewClientSessionScope(orgID, fmt.Sprintf("key-%02d", i))
		return scope, fmt.Sprintf("session-for-key-%02d", i)
	}
	for i := 0; i < capKeys; i++ {
		scope, sid := keySession(i)
		reg.Record(serverID, scope, sid)
	}

	// Touch key-00 via a Known lookup — this counts as use and must move it
	// to the front of this org's own key LRU, protecting it from the
	// eviction about to happen.
	scope0, sid0 := keySession(0)
	if !reg.Known(serverID, scope0, sid0) {
		t.Fatal("key-00's session not known before touching it, test setup is broken")
	}

	// Adding one more key, still within the SAME org, beyond the cap must
	// evict the LEAST recently used key — key-01, since key-00 was just
	// refreshed above — not key-00.
	newScope, newSid := mcp.NewClientSessionScope(orgID, "key-overflow"), "session-for-the-overflow-key"
	reg.Record(serverID, newScope, newSid)

	if !reg.Known(serverID, scope0, sid0) {
		t.Error("key-00 was evicted despite being the most recently touched key, want the LRU eviction to spare it")
	}
	scope1, sid1 := keySession(1)
	if reg.Known(serverID, scope1, sid1) {
		t.Error("key-01 is still known after the overflow, want it to be the one evicted (least recently used)")
	}
	if !reg.Known(serverID, newScope, newSid) {
		t.Error("the newly added, 17th key is not known immediately after being recorded")
	}
}

// ---- NewClientSessionScope: collision-safe encoding ------------------------

// TestNewClientSessionScope_CollisionSafety constructs (orgID, apiKeyID)
// pairs that WOULD collide under naive string concatenation (orgID+apiKeyID)
// and proves NewClientSessionScope's length-prefixed encoding keeps them
// distinct — verified two ways: the returned SessionScope values themselves
// differ, and a session recorded under one pair's scope is never known under
// the other's.
func TestNewClientSessionScope_CollisionSafety(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		orgA, keyA string
		orgB, keyB string
	}{
		{
			name: "digit boundary shift",
			orgA: "12", keyA: "3abc",
			orgB: "123", keyB: "abc",
		},
		{
			name: "empty apiKeyID vs orgID absorbing it",
			orgA: "org1", keyA: "",
			orgB: "org", keyB: "1",
		},
		{
			name: "both empty vs a real-looking single-char pair",
			orgA: "", keyA: "",
			orgB: "0", keyB: "",
		},
		{
			name: "colon inside orgID does not create an ambiguous boundary",
			orgA: "org:evil", keyA: "key1",
			orgB: "org", keyB: "evil:key1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			scopeA := mcp.NewClientSessionScope(tc.orgA, tc.keyA)
			scopeB := mcp.NewClientSessionScope(tc.orgB, tc.keyB)
			if scopeA == scopeB {
				t.Fatalf("NewClientSessionScope(%q, %q) == NewClientSessionScope(%q, %q) == %q, want distinct scopes",
					tc.orgA, tc.keyA, tc.orgB, tc.keyB, scopeA)
			}

			reg := mcp.NewSessionRegistry()
			const serverID = "server-1"
			const sid = "session-shared-across-both-attempts"

			reg.Record(serverID, scopeA, sid)
			if reg.Known(serverID, scopeB, sid) {
				t.Errorf("a session recorded under org=%q key=%q is known under the DIFFERENT pair org=%q key=%q — "+
					"the encoding collided", tc.orgA, tc.keyA, tc.orgB, tc.keyB)
			}
			if !reg.Known(serverID, scopeA, sid) {
				t.Error("the session is not even known under its OWN scope, test setup is broken")
			}
		})
	}
}

// TestNewClientSessionScope_SameInputsAlwaysProduceTheSameScope is the
// positive control for the collision tests above: two calls with IDENTICAL
// inputs must always produce the same SessionScope, so a legitimate caller's
// own repeated requests keep hitting the same bucket.
func TestNewClientSessionScope_SameInputsAlwaysProduceTheSameScope(t *testing.T) {
	t.Parallel()

	a := mcp.NewClientSessionScope("org-123", "key-456")
	b := mcp.NewClientSessionScope("org-123", "key-456")
	if a != b {
		t.Errorf("NewClientSessionScope called twice with identical inputs produced %q and %q, want identical", a, b)
	}
}

// ---- decodeClientSessionScope: reversing NewClientSessionScope's encoding -

// TestDecodeClientSessionScope_RoundTripsWithNewClientSessionScope verifies
// mcp.DecodeClientSessionScope (the test-only export of the unexported
// decodeClientSessionScope) exactly reverses NewClientSessionScope's
// encoding for a spread of inputs designed to stress the length-prefixed
// scheme: values containing colons of their own, values that start with
// digits (so they could be mistaken for another length prefix if the parser
// were sloppy), and empty organization or key IDs.
func TestDecodeClientSessionScope_RoundTripsWithNewClientSessionScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		orgID, keyID string
	}{
		{name: "plain values", orgID: "org-123", keyID: "key-456"},
		{name: "colon inside orgID", orgID: "org:evil", keyID: "key1"},
		{name: "colon inside apiKeyID", orgID: "org1", keyID: "key:evil"},
		{name: "orgID starts with digits", orgID: "123abc", keyID: "key-1"},
		{name: "apiKeyID starts with digits", orgID: "org-1", keyID: "456xyz"},
		{name: "empty orgID", orgID: "", keyID: "key-1"},
		{name: "empty apiKeyID", orgID: "org-1", keyID: ""},
		{name: "both empty", orgID: "", keyID: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			scope := mcp.NewClientSessionScope(tc.orgID, tc.keyID)
			gotOrg, gotKey, ok := mcp.DecodeClientSessionScope(scope)
			if !ok {
				t.Fatalf("DecodeClientSessionScope(%q) ok = false, want true (built by NewClientSessionScope)", scope)
			}
			if gotOrg != tc.orgID || gotKey != tc.keyID {
				t.Errorf("DecodeClientSessionScope(%q) = (%q, %q), want (%q, %q)", scope, gotOrg, gotKey, tc.orgID, tc.keyID)
			}
		})
	}
}

// TestDecodeClientSessionScope_NonEncodedScope_FallsBackCleanly verifies
// that a SessionScope value never built via NewClientSessionScope — every
// literal SessionScope this package's own tests construct by hand, and
// Call's own per-org scope (see eraBinding.scopes' doc) — is reported as
// undecodable (ok = false) rather than misparsed into a bogus (orgID,
// apiKeyID) pair, and that SessionRegistry isolation still holds across two
// such non-encoded scopes.
func TestDecodeClientSessionScope_NonEncodedScope_FallsBackCleanly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		scope mcp.SessionScope
	}{
		{name: "plain org id, no colon at all", scope: "org-plain"},
		{name: "colon present but the prefix is not a valid decimal length", scope: "not-a-number:rest"},
		{name: "decimal prefix claims more bytes than the string actually has", scope: "99:short"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, ok := mcp.DecodeClientSessionScope(tc.scope)
			if ok {
				t.Errorf("DecodeClientSessionScope(%q) ok = true, want false (not built by NewClientSessionScope)", tc.scope)
			}
		})
	}

	// Isolation must still hold across two independent, non-encoded scopes:
	// scopeBucket's fallback names each one's bucket after its own full text,
	// so two DIFFERENT literal scopes never collide with each other.
	reg := mcp.NewSessionRegistry()
	const serverID = "server-1"
	reg.Record(serverID, mcp.SessionScope("org-plain-a"), "session-a")
	if reg.Known(serverID, mcp.SessionScope("org-plain-b"), "session-a") {
		t.Error("a session recorded under one non-encoded scope is known under a different one, want isolated")
	}
}

// TestSessionRegistry_ExactScopeIsolation_WithinSharedBucket is the
// regression test for keying scopeSessions by the exact, full SessionScope
// string (via keyScopes, one level below the (orgID, apiKeyID) bucket
// scopeBucket computes) rather than by that bucket alone. The bucket exists
// purely to decide which entries rise and fall together for
// maxOrgsPerServer's and maxKeysPerOrg's LRU eviction; it must never decide
// which sessions a Known lookup for one particular scope can see — those are
// two separate concerns, and this test proves the second one stays exact
// even when the first one happens to coincide for two DIFFERENT scopes.
//
// A shared bucket is constructible: mcp.NewClientSessionScope("foo", "")
// encodes to the literal string "3:foo", which decodeClientSessionScope
// recovers as (orgID: "foo", apiKeyID: ""). A plain, never-encoded
// SessionScope("foo") — exactly the shape used throughout this very test
// file, and by Call's own per-org scope (see eraBinding.scopes' doc: "Call
// always passes mcp.SessionScope(ki.OrgID)") — has no colon at all, so
// decodeClientSessionScope reports it undecodable and scopeBucket falls back
// to (string(scope), "") = ("foo", ""): THE SAME BUCKET. Despite that, a
// session recorded under the encoded scope must stay invisible to a Known
// lookup under the unrelated literal scope, and vice versa.
//
// This exact shared-bucket construction is not reachable through
// HandleMCPProxy today — SessionRegistry's only production caller, which
// always builds scope via mcp.NewClientSessionScope(ki.OrgID, ki.ID)
// (internal/api/admin/mcp_proxy.go) — so it is exercised here purely as a
// worst-case unit-level guarantee, independent of what any current caller
// happens to do.
//
// A sharper version of this test — two scopes that are BOTH built via
// NewClientSessionScope yet land in the same (orgID, apiKeyID) bucket — is
// constructively impossible to build: decodeClientSessionScope always
// recovers the EXACT (orgID, apiKeyID) pair NewClientSessionScope encoded,
// so two calls with different (orgID, apiKeyID) pairs always decode to
// different buckets, and two calls with the SAME pair always produce the
// same encoded string in the first place (see
// TestNewClientSessionScope_SameInputsAlwaysProduceTheSameScope) — i.e. the
// same scope, not two different ones sharing a bucket. See
// TestNewClientSessionScope_CollisionSafety for the general form of that
// injectivity proof. The only way two DIFFERENT scope strings can ever share
// a bucket is exactly the shape exercised above: one genuinely encoded, one
// never encoded at all.
func TestSessionRegistry_ExactScopeIsolation_WithinSharedBucket(t *testing.T) {
	t.Parallel()

	reg := mcp.NewSessionRegistry()
	const serverID = "server-1"

	encoded := mcp.NewClientSessionScope("foo", "") // -> "3:foo"
	literal := mcp.SessionScope("foo")              // never built via NewClientSessionScope

	// Confirm the premise before asserting on it: the two really do share a
	// bucket (decodeClientSessionScope recovers exactly literal's own text as
	// orgID, with an empty apiKeyID — the same pair scopeBucket's fallback
	// assigns literal itself).
	orgID, apiKeyID, ok := mcp.DecodeClientSessionScope(encoded)
	if !ok || orgID != string(literal) || apiKeyID != "" {
		t.Fatalf("test setup: DecodeClientSessionScope(%q) = (%q, %q, %v), want (%q, %q, true) to share a bucket with literal scope %q",
			encoded, orgID, apiKeyID, ok, string(literal), "", literal)
	}

	reg.Record(serverID, encoded, "session-encoded")
	if reg.Known(serverID, literal, "session-encoded") {
		t.Errorf("session recorded under %q is known under the unrelated scope %q — they share an LRU bucket but must "+
			"not share session bookkeeping", encoded, literal)
	}
	if !reg.Known(serverID, encoded, "session-encoded") {
		t.Error("the session is not even known under its OWN scope, test setup is broken")
	}

	// And the reverse direction: a session recorded under the literal scope
	// must stay invisible to the encoded scope.
	reg.Record(serverID, literal, "session-literal")
	if reg.Known(serverID, encoded, "session-literal") {
		t.Errorf("session recorded under %q is known under the unrelated scope %q — they share an LRU bucket but must "+
			"not share session bookkeeping", literal, encoded)
	}
	if !reg.Known(serverID, literal, "session-literal") {
		t.Error("the session is not even known under its OWN scope, test setup is broken")
	}
}

// TestSessionRegistry_MaxKeysPerOrg_EvictionTakesTheEntireSharedBucketWithIt
// closes a potential gap the keyScopes intermediate layer could have opened:
// maxKeysPerOrg still evicts by (orgID, apiKeyID) BUCKET (org.keyLRU holds
// *keyScopes, not individual scopes), and — in the deliberately-constructed
// shared-bucket shape from
// TestSessionRegistry_ExactScopeIsolation_WithinSharedBucket — a single
// bucket can hold more than one distinct scope's sessions. When that bucket
// is evicted, ALL of its scopes must disappear together, not just the one
// that happened to be recorded first; a partial eviction would silently
// leave one of the two scopes' sessions alive past its bucket's own
// lifetime, defeating the memory bound this test's sibling
// (TestSessionRegistry_MaxKeysPerOrg_LRUEvictsLeastRecentlyUsedKeyWithinOrg)
// otherwise verifies for the ordinary, one-scope-per-bucket case.
func TestSessionRegistry_MaxKeysPerOrg_EvictionTakesTheEntireSharedBucketWithIt(t *testing.T) {
	t.Parallel()

	reg := mcp.NewSessionRegistry()
	const serverID = "server-1"
	const orgID = "org-with-a-shared-bucket"
	const capKeys = 16 // maxKeysPerOrg

	// Two DIFFERENT scope strings sharing the SAME (orgID, apiKeyID="")
	// bucket within this one organization — see
	// TestSessionRegistry_ExactScopeIsolation_WithinSharedBucket for why this
	// pairing (one encoded, one never encoded) is the only way to construct
	// two distinct scopes sharing a bucket at all.
	encoded := mcp.NewClientSessionScope(orgID, "")
	literal := mcp.SessionScope(orgID)

	reg.Record(serverID, encoded, "session-encoded")
	reg.Record(serverID, literal, "session-literal")
	if !reg.Known(serverID, encoded, "session-encoded") || !reg.Known(serverID, literal, "session-literal") {
		t.Fatal("both scopes sharing the bucket must be known right after recording, test setup is broken")
	}

	// Fill the remaining capKeys-1 keys in the SAME org so the shared bucket
	// (apiKeyID "") is the least recently used once done.
	for i := 1; i < capKeys; i++ {
		scope := mcp.NewClientSessionScope(orgID, fmt.Sprintf("key-%02d", i))
		reg.Record(serverID, scope, fmt.Sprintf("session-key-%02d", i))
	}

	// One more key, still within the same org, overflows the cap, evicting
	// the shared bucket entirely.
	overflow := mcp.NewClientSessionScope(orgID, "key-overflow")
	reg.Record(serverID, overflow, "session-overflow")

	if reg.Known(serverID, encoded, "session-encoded") {
		t.Error("the encoded scope's session is still known after its shared bucket was evicted, want it gone")
	}
	if reg.Known(serverID, literal, "session-literal") {
		t.Error("the literal scope's session is still known after its shared bucket was evicted, want it gone TOO — " +
			"the whole bucket must evict as one unit, not just the scope recorded first")
	}
}

// ---- Reconcile: pruning entries for servers that left the active set ------

// TestSessionRegistry_Reconcile_RemovesInactiveServer verifies that a server
// ID no longer present in the active set passed to Reconcile has every scope
// and session it ever held forgotten: a previously KNOWN session becomes
// unknown immediately afterward.
func TestSessionRegistry_Reconcile_RemovesInactiveServer(t *testing.T) {
	t.Parallel()

	reg := mcp.NewSessionRegistry()
	const serverID = "server-to-be-removed"
	const scope = mcp.SessionScope("org-1")
	const sid = "session-1"

	reg.Record(serverID, scope, sid)
	if !reg.Known(serverID, scope, sid) {
		t.Fatal("session not known before Reconcile, test setup is broken")
	}

	reg.Reconcile([]string{"some-other-server"})

	if reg.Known(serverID, scope, sid) {
		t.Error("session still known after Reconcile removed serverID from the active set, want it forgotten")
	}
}

// TestSessionRegistry_Reconcile_KeepsActiveServer verifies that a server ID
// still present in the active set passed to Reconcile is completely
// unaffected — Reconcile only ever prunes servers that LEFT the set, never
// touches the ones that stayed.
func TestSessionRegistry_Reconcile_KeepsActiveServer(t *testing.T) {
	t.Parallel()

	reg := mcp.NewSessionRegistry()
	const serverID = "server-to-keep"
	const scope = mcp.SessionScope("org-1")
	const sid = "session-1"

	reg.Record(serverID, scope, sid)
	reg.Reconcile([]string{serverID, "some-other-server"})

	if !reg.Known(serverID, scope, sid) {
		t.Error("session no longer known after Reconcile with serverID still in the active set, want it unaffected")
	}
}

// TestSessionRegistry_Reconcile_EmptySet_ClearsEverythingButStaysUsable
// verifies Reconcile(nil) — the "no server is active" case (e.g. every MCP
// server has been deleted or deactivated) — prunes every server this
// registry ever held, and that the registry remains fully usable afterward:
// a fresh Record/Known cycle against a server ID that was JUST pruned works
// exactly as it would on a brand new registry.
func TestSessionRegistry_Reconcile_EmptySet_ClearsEverythingButStaysUsable(t *testing.T) {
	t.Parallel()

	reg := mcp.NewSessionRegistry()
	const serverID = "server-1"
	const scope = mcp.SessionScope("org-1")
	const sid = "session-1"

	reg.Record(serverID, scope, sid)
	reg.Reconcile(nil)

	if reg.Known(serverID, scope, sid) {
		t.Error("session still known after Reconcile(nil), want everything pruned")
	}

	reg.Record(serverID, scope, sid)
	if !reg.Known(serverID, scope, sid) {
		t.Error("Record/Known no longer work on serverID after Reconcile(nil), want the registry to stay fully usable")
	}
}

// TestSessionRegistry_Reconcile_LateRecordRevivesRemovedServer_NextReconcileRemovesItAgain
// documents and locks in ACCEPTED, KNOWN behavior — not a bug still open:
// Record (via serverFor) unconditionally recreates a server's entry, so a
// Record call that was already in flight when Reconcile ran (e.g. a proxied
// request whose response, carrying a fresh Mcp-Session-Id, is still being
// processed by HandleMCPProxy at the moment an admin deletes or deactivates
// that same server) can re-insert the very entry Reconcile just removed. That
// race is not closed here, and is not this test's concern.
//
// What this test locks in is the property the periodic 30-second Reconcile
// backstop in internal/app.Application.Start relies on to make that race
// self-healing rather than permanent: a revived entry is not "stuck" — the
// NEXT Reconcile call that still excludes the server's ID removes it again,
// exactly as if the late Record had never happened. Without this property,
// adding a periodic Reconcile call would not actually bound how long a
// deleted/deactivated server's sessions stay reachable; a revived entry could
// survive every subsequent tick forever. internal/app has no test harness
// that constructs a real *Application to drive that ticker directly (Start
// wires it inline against a fully constructed application), so this test
// exercises the registry-level property in isolation instead.
func TestSessionRegistry_Reconcile_LateRecordRevivesRemovedServer_NextReconcileRemovesItAgain(t *testing.T) {
	t.Parallel()

	reg := mcp.NewSessionRegistry()
	const serverID = "server-deleted-mid-flight"
	const scope = mcp.SessionScope("org-1")
	const sid = "session-from-a-late-returning-record"

	// An ordinary session, recorded before the server ever left the active
	// set.
	reg.Record(serverID, scope, sid)
	if !reg.Known(serverID, scope, sid) {
		t.Fatal("session not known right after Record, test setup is broken")
	}

	// The server leaves the active set (deleted or deactivated) — Reconcile
	// removes it.
	reg.Reconcile([]string{"some-other-server"})
	if reg.Known(serverID, scope, sid) {
		t.Fatal("session still known after the first Reconcile, test setup is broken")
	}

	// A Record call that started before the deletion — e.g. HandleMCPProxy
	// still processing an in-flight response — returns late and re-inserts
	// the server's entry. This is the "late Record revives a removed server"
	// scenario itself: proven here as a precondition, not as this test's
	// point.
	reg.Record(serverID, scope, sid)
	if !reg.Known(serverID, scope, sid) {
		t.Fatal("a late Record after Reconcile did not revive the entry — test setup is broken " +
			"(this precondition is what the periodic Reconcile backstop exists to correct for)")
	}

	// The property under test: the periodic backstop's NEXT tick — another
	// Reconcile still excluding serverID — catches the revived entry and
	// removes it again, exactly as it did the first time. If this failed, a
	// revived entry would survive every subsequent Reconcile call forever,
	// and adding a periodic call to Reconcile would not actually bound
	// anything.
	reg.Reconcile([]string{"some-other-server"})
	if reg.Known(serverID, scope, sid) {
		t.Error("session still known after the SECOND Reconcile, want the revived entry removed again — a " +
			"repeated Reconcile must catch a late-Record revival, which is the property the periodic ticker " +
			"backstop in internal/app depends on")
	}
}

// ---- Concurrency: Record and Known from many goroutines, -race -----------

// TestSessionRegistry_ConcurrentRecordAndKnown_NoRaceNoMixing drives many
// goroutines across several distinct (server, scope) pairs, each repeatedly
// recording its own session and checking it is known, while never observing
// a NEIGHBOR's session as known for its own scope. Run with -race.
func TestSessionRegistry_ConcurrentRecordAndKnown_NoRaceNoMixing(t *testing.T) {
	t.Parallel()

	reg := mcp.NewSessionRegistry()

	const servers = 3
	const scopesPerServer = 8
	const roundsPerScope = 200

	type pair struct {
		server string
		scope  mcp.SessionScope
		sid    string
	}
	var pairs []pair
	for s := 0; s < servers; s++ {
		for sc := 0; sc < scopesPerServer; sc++ {
			pairs = append(pairs, pair{
				server: fmt.Sprintf("server-%d", s),
				scope:  mcp.SessionScope(fmt.Sprintf("scope-%d", sc)),
				sid:    fmt.Sprintf("session-server%d-scope%d", s, sc),
			})
		}
	}

	var wg sync.WaitGroup
	errCh := make(chan string, len(pairs)*roundsPerScope)

	for i, p := range pairs {
		p := p
		// neighbor is the next scope on the SAME server (wrapping within that
		// server's own block of scopesPerServer entries) — its session must
		// never be observed as known under p's own scope.
		neighbor := pairs[(i/scopesPerServer)*scopesPerServer+(i+1)%scopesPerServer]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < roundsPerScope; r++ {
				reg.Record(p.server, p.scope, p.sid)
				if !reg.Known(p.server, p.scope, p.sid) {
					errCh <- fmt.Sprintf("server=%s scope=%s: own session not known immediately after Record (round %d)", p.server, p.scope, r)
				}
				if reg.Known(p.server, p.scope, neighbor.sid) {
					errCh <- fmt.Sprintf("server=%s scope=%s: neighbor's session %q leaked in", p.server, p.scope, neighbor.sid)
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)

	var failures int
	for msg := range errCh {
		failures++
		if failures <= 20 {
			t.Error(msg)
		}
	}
	if failures > 20 {
		t.Errorf("... and %d more failures", failures-20)
	}
}

// TestSessionRegistry_ConcurrentReconcile_NoRaceWithRecordAndKnown drives
// concurrent Record/Known traffic against several servers while Reconcile
// concurrently prunes and restores server IDs from the active set — the
// production shape of admin.Handler.refreshMCPCaches running after an MCP
// server mutation while ordinary proxied traffic is in flight against OTHER
// servers at the same time. Run with -race. Reconcile must never make an
// in-flight Record/Known call panic or deadlock; the only property under
// test here (beyond the absence of a data race) is that the registry stays
// fully usable throughout and afterward — a specific interleaving of which
// Known call sees which state is not, since Reconcile's whole point is to
// change that state out from under concurrent readers.
func TestSessionRegistry_ConcurrentReconcile_NoRaceWithRecordAndKnown(t *testing.T) {
	t.Parallel()

	reg := mcp.NewSessionRegistry()
	const servers = 4
	const rounds = 300

	serverIDs := make([]string, servers)
	for i := range serverIDs {
		serverIDs[i] = fmt.Sprintf("reconcile-server-%d", i)
	}

	var wg sync.WaitGroup

	for _, sid := range serverIDs {
		sid := sid
		wg.Add(1)
		go func() {
			defer wg.Done()
			const scope = mcp.SessionScope("org-1")
			for r := 0; r < rounds; r++ {
				session := fmt.Sprintf("session-%d", r)
				reg.Record(sid, scope, session)
				reg.Known(sid, scope, session)
			}
		}()
	}

	// Reconcile goroutine: alternates between the full active set and an
	// empty one, concurrently with the traffic above.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for r := 0; r < rounds; r++ {
			if r%2 == 0 {
				reg.Reconcile(serverIDs)
			} else {
				reg.Reconcile(nil)
			}
		}
	}()

	wg.Wait()

	// The registry must still be fully usable after all of this.
	reg.Record("post-reconcile-server", "scope-x", "session-x")
	if !reg.Known("post-reconcile-server", "scope-x", "session-x") {
		t.Error("registry not usable after concurrent Reconcile/Record/Known, want a fresh Record/Known cycle to still work")
	}
}
