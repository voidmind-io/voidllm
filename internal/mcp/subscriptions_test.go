package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// ---- dispatch: honored-filter table -----------------------------------------

// TestSubscriptionsListen_Dispatch_HonoredFilter is the table-driven core of
// subscriptions/listen dispatch coverage (MCP 2026-07-28 §3.4): what a given
// server configuration + requested filter combination actually honors in its
// acknowledgement, for every server the request reaches through Server.Handle
// itself (never the registry limits or CloseSubscriptions — those get their
// own tests below).
func TestSubscriptionsListen_Dispatch_HonoredFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		toolsSourceEnabled  bool
		notifications       map[string]any // nil = omit params.notifications entirely
		wantHonoredToolsSet bool
	}{
		{
			name:                "code-mode-style server (source enabled) honors requested toolsListChanged",
			toolsSourceEnabled:  true,
			notifications:       map[string]any{"toolsListChanged": true},
			wantHonoredToolsSet: true,
		},
		{
			name:                "management-style server (source disabled) never honors toolsListChanged",
			toolsSourceEnabled:  false,
			notifications:       map[string]any{"toolsListChanged": true},
			wantHonoredToolsSet: false,
		},
		{
			name:                "source enabled but toolsListChanged not requested",
			toolsSourceEnabled:  true,
			notifications:       map[string]any{"toolsListChanged": false},
			wantHonoredToolsSet: false,
		},
		{
			name:               "missing params.notifications entirely honors nothing",
			toolsSourceEnabled: true,
			notifications:      nil,
		},
		{
			name:               "explicit empty notifications object honors nothing",
			toolsSourceEnabled: true,
			notifications:      map[string]any{},
		},
		{
			name:               "unsupported types requested alongside toolsListChanged are never honored",
			toolsSourceEnabled: true,
			notifications: map[string]any{
				"toolsListChanged":      true,
				"promptsListChanged":    true,
				"resourcesListChanged":  true,
				"resourceSubscriptions": []string{"file:///a"},
			},
			wantHonoredToolsSet: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestServer("voidllm", "0.1.0")
			s.SetToolsListChangedSource(tc.toolsSourceEnabled)

			extraParams := map[string]any{}
			if tc.notifications != nil {
				extraParams["notifications"] = tc.notifications
			}
			result := s.Handle(context.Background(),
				[]byte(modernRequestBody(1, "subscriptions/listen", extraParams, nil)),
				modernHeader())

			if result.Body != nil {
				t.Fatalf("Body = %s, want nil for a successful Listen result", result.Body)
			}
			if result.Listen == nil {
				t.Fatal("Listen = nil, want a non-nil ListenRequest")
			}
			t.Cleanup(result.Listen.Sub.Unregister)

			if got := result.Listen.Sub.Honored().ToolsListChanged; got != tc.wantHonoredToolsSet {
				t.Errorf("Honored().ToolsListChanged = %v, want %v", got, tc.wantHonoredToolsSet)
			}
			// Every OTHER field must never be honored — neither built-in server
			// ever sets them (subscriptions.go, ListenFilter's own doc).
			h := result.Listen.Sub.Honored()
			if h.PromptsListChanged || h.ResourcesListChanged || len(h.ResourceSubscriptions) != 0 {
				t.Errorf("Honored() = %+v, want only ToolsListChanged ever set", h)
			}

			// The ack event on the wire must carry exactly this same honored
			// subset, per the spec's "unsupported types are omitted" rule.
			ack := string(result.Listen.Ack)
			if tc.wantHonoredToolsSet {
				if !strings.Contains(ack, `"toolsListChanged":true`) {
					t.Errorf("Ack = %s, want toolsListChanged:true present", ack)
				}
			} else if strings.Contains(ack, "toolsListChanged") {
				t.Errorf("Ack = %s, want toolsListChanged omitted entirely", ack)
			}
			if strings.Contains(ack, "promptsListChanged") || strings.Contains(ack, "resourcesListChanged") ||
				strings.Contains(ack, "resourceSubscriptions") {
				t.Errorf("Ack = %s, want no unsupported notification type ever present", ack)
			}
		})
	}
}

// TestSubscriptionsListen_Dispatch_MalformedNotifications verifies that a
// params.notifications which is present but not a JSON object, or whose
// fields are mistyped, is rejected with CodeInvalidParams and HintBadRequest
// — never silently defaulted to an empty filter, and never opening a stream.
func TestSubscriptionsListen_Dispatch_MalformedNotifications(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		notifications any
	}{
		{name: "notifications is a JSON string, not an object", notifications: "not-an-object"},
		{name: "notifications is a JSON array, not an object", notifications: []string{"a", "b"}},
		{name: "toolsListChanged has the wrong JSON type", notifications: map[string]any{"toolsListChanged": "yes"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestServer("voidllm", "0.1.0")
			s.SetToolsListChangedSource(true)

			body := modernRequestBody(1, "subscriptions/listen",
				map[string]any{"notifications": tc.notifications}, nil)
			result := s.Handle(context.Background(), []byte(body), modernHeader())

			if result.Listen != nil {
				t.Cleanup(result.Listen.Sub.Unregister)
				t.Fatal("Listen != nil, want the malformed request rejected before ever opening a stream")
			}
			if result.Hint != mcp.HintBadRequest {
				t.Errorf("Hint = %v, want HintBadRequest (HTTP 400)", result.Hint)
			}
			var resp mcp.Response
			if err := json.Unmarshal(result.Body, &resp); err != nil {
				t.Fatalf("unmarshal error body: %v; raw: %s", err, result.Body)
			}
			if resp.Error == nil {
				t.Fatal("Error is nil, want CodeInvalidParams")
			}
			if resp.Error.Code != mcp.CodeInvalidParams {
				t.Errorf("Error.Code = %d, want %d (CodeInvalidParams)", resp.Error.Code, mcp.CodeInvalidParams)
			}
		})
	}
}

// TestSubscriptionsListen_LegacyEra_NeverOpensAStream is the table-driven
// counterpart of TestServer_SubscriptionsListen_LegacyEraStillMethodNotFound:
// every legacy dialect this package supports resolves subscriptions/listen to
// CodeMethodNotFound, never a Listen result, regardless of the notifications
// filter the (legacy, so era-inapplicable) request body happens to carry.
func TestSubscriptionsListen_LegacyEra_NeverOpensAStream(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)

	result := s.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true}}}`),
		mcp.MapHeader{})

	if result.Listen != nil {
		t.Cleanup(result.Listen.Sub.Unregister)
		t.Fatal("Listen != nil, want the legacy era to never open a stream")
	}
	var resp mcp.Response
	if err := json.Unmarshal(result.Body, &resp); err != nil {
		t.Fatalf("unmarshal: %v; raw: %s", err, result.Body)
	}
	if resp.Error == nil || resp.Error.Code != mcp.CodeMethodNotFound {
		t.Fatalf("Error = %+v, want CodeMethodNotFound", resp.Error)
	}
}

// ---- server/discover: tools.listChanged capability --------------------------

// TestDiscover_ToolsListChangedCapability verifies handleDiscover's own
// contract (SetToolsListChangedSource's doc): capabilities.tools.listChanged
// is present and true ONLY when the server was configured with
// SetToolsListChangedSource(true) — e.g. a Code Mode server — and entirely
// absent (not merely false) for a server like the built-in management server
// that never calls it.
func TestDiscover_ToolsListChangedCapability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		sourceEnabled bool
	}{
		{name: "code-mode-style server advertises listChanged", sourceEnabled: true},
		{name: "management-style server advertises no listChanged key at all", sourceEnabled: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestServer("voidllm", "0.1.0")
			s.SetToolsListChangedSource(tc.sourceEnabled)

			out := s.Handle(context.Background(), []byte(modernRequestBody(1, "server/discover", nil, nil)), modernHeader())
			result := decodeModernResult(t, out.Body)

			caps, _ := result["capabilities"].(map[string]any)
			if caps == nil {
				t.Fatalf("capabilities missing from server/discover result: %+v", result)
			}
			tools, _ := caps["tools"].(map[string]any)
			if tools == nil {
				t.Fatalf("capabilities.tools missing or not an object: %+v", caps)
			}
			listChanged, present := tools["listChanged"]
			if tc.sourceEnabled {
				if !present || listChanged != true {
					t.Errorf("capabilities.tools.listChanged = %v (present=%v), want true", listChanged, present)
				}
			} else if present {
				t.Errorf("capabilities.tools.listChanged = %v, want the key entirely absent", listChanged)
			}
		})
	}
}

// ---- subscriberRegistry limits -----------------------------------------------

// listenOnce issues one subscriptions/listen request against s for the given
// KeyIdentity and fails the test if it does not resolve to a Listen result at
// all (a malformed-request or era failure) — every call site here is
// exercising the REGISTRY's own accept/reject decision, not the request
// shape, so a non-Listen result always indicates a broken test fixture, not
// the condition under test.
func listenOnce(t *testing.T, s *mcp.Server, id mcp.KeyIdentity, reqID int) mcp.HandleResult {
	t.Helper()
	ctx := mcp.WithKeyIdentity(context.Background(), id)
	body := modernRequestBody(reqID, "subscriptions/listen", map[string]any{"notifications": map[string]any{"toolsListChanged": true}}, nil)
	return s.Handle(ctx, []byte(body), modernHeader())
}

// TestSubscriberRegistry_PerKeyLimit_FifthStreamRejected verifies
// maxListenStreamsPerKey (4): a caller may hold at most 4 concurrent
// subscriptions/listen registrations against one Server instance; the 5th is
// refused BEFORE any stream opens, with CodeTooManyListenStreams and
// HintTooManyRequests (HTTP 429 at the admin layer).
func TestSubscriberRegistry_PerKeyLimit_FifthStreamRejected(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)
	id := mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-per-limit"}

	var subs []*mcp.Subscriber
	for i := 0; i < 4; i++ {
		res := listenOnce(t, s, id, i+1)
		if res.Listen == nil {
			t.Fatalf("registration %d: Listen = nil, want success (body: %s)", i+1, res.Body)
		}
		subs = append(subs, res.Listen.Sub)
	}
	t.Cleanup(func() {
		for _, sub := range subs {
			sub.Unregister()
		}
	})

	fifth := listenOnce(t, s, id, 5)
	if fifth.Listen != nil {
		fifth.Listen.Sub.Unregister()
		t.Fatal("5th registration succeeded, want CodeTooManyListenStreams")
	}
	if fifth.Hint != mcp.HintTooManyRequests {
		t.Errorf("Hint = %v, want HintTooManyRequests (HTTP 429)", fifth.Hint)
	}
	var resp mcp.Response
	if err := json.Unmarshal(fifth.Body, &resp); err != nil {
		t.Fatalf("unmarshal: %v; raw: %s", err, fifth.Body)
	}
	if resp.Error == nil || resp.Error.Code != mcp.CodeTooManyListenStreams {
		t.Fatalf("Error = %+v, want CodeTooManyListenStreams", resp.Error)
	}

	// A DIFFERENT key is unaffected by this key's own limit.
	other := listenOnce(t, s, mcp.KeyIdentity{OrgID: "org-1", KeyID: "a-different-key"}, 6)
	if other.Listen == nil {
		t.Fatalf("a different key's registration failed: %s", other.Body)
	}
	other.Listen.Sub.Unregister()
}

// TestSubscriberRegistry_SlotsReleasedAfterUnregister verifies that
// Subscriber.Unregister actually frees its maxListenStreamsPerKey slot: once
// one of 4 concurrent registrations for a key is unregistered, a 5th
// registration for that same key succeeds.
func TestSubscriberRegistry_SlotsReleasedAfterUnregister(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)
	id := mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-release"}

	var subs []*mcp.Subscriber
	for i := 0; i < 4; i++ {
		res := listenOnce(t, s, id, i+1)
		if res.Listen == nil {
			t.Fatalf("registration %d failed: %s", i+1, res.Body)
		}
		subs = append(subs, res.Listen.Sub)
	}

	blocked := listenOnce(t, s, id, 5)
	if blocked.Listen != nil {
		blocked.Listen.Sub.Unregister()
		t.Fatal("5th registration succeeded before any slot was released")
	}

	// Release exactly one slot.
	subs[0].Unregister()
	t.Cleanup(func() {
		for _, sub := range subs[1:] {
			sub.Unregister()
		}
	})

	freed := listenOnce(t, s, id, 6)
	if freed.Listen == nil {
		t.Fatalf("registration after releasing a slot failed: %s", freed.Body)
	}
	t.Cleanup(freed.Listen.Sub.Unregister)
}

// TestSubscriberRegistry_PerServerLimit_1025thStreamRejected verifies
// maxListenStreamsPerServer (1024): the total number of concurrently open
// subscriptions/listen streams on one Server instance is capped regardless of
// how many distinct keys are involved. Exercised directly against
// Server.Handle (1025 fast, in-memory registrations) rather than through 1025
// real HTTP/SSE round trips — the registry accept/reject decision under test
// (subscriberRegistry.register) is identical either way, and driving it
// through the full HTTP stack 1025 times would only add process/goroutine
// overhead without exercising any additional code path; the admin layer's own
// HTTP 429 mapping for this same error code is already covered by the
// per-key test above and by the admin package's end-to-end listen tests.
func TestSubscriberRegistry_PerServerLimit_1025thStreamRejected(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)

	var subs []*mcp.Subscriber
	t.Cleanup(func() {
		for _, sub := range subs {
			sub.Unregister()
		}
	})

	// Spread registrations across many distinct orgs (one key per org) so
	// this test proves the SERVER-wide cap specifically, never tripping the
	// per-org cap (maxListenStreamsPerOrg, 64 — see its own test) along the
	// way to 1024.
	const maxListenStreamsPerServer = 1024
	for i := 0; i < maxListenStreamsPerServer; i++ {
		id := mcp.KeyIdentity{OrgID: fmt.Sprintf("server-limit-org-%d", i), KeyID: fmt.Sprintf("server-limit-key-%d", i)}
		res := listenOnce(t, s, id, i+1)
		if res.Listen == nil {
			t.Fatalf("registration %d/%d failed before reaching the server-wide cap: %s", i+1, maxListenStreamsPerServer, res.Body)
		}
		subs = append(subs, res.Listen.Sub)
	}

	// The 1025th registration, for an org/KeyID pair that has never
	// registered before (so it cannot be hitting the per-key or per-org limit
	// instead), must be rejected — the server-wide cap is what is under test
	// here.
	over := listenOnce(t, s, mcp.KeyIdentity{OrgID: "server-limit-org-overflow", KeyID: "server-limit-key-overflow"}, maxListenStreamsPerServer+1)
	if over.Listen != nil {
		over.Listen.Sub.Unregister()
		t.Fatal("1025th registration succeeded, want CodeTooManyListenStreams")
	}
	if over.Hint != mcp.HintTooManyRequests {
		t.Errorf("Hint = %v, want HintTooManyRequests", over.Hint)
	}
	var resp mcp.Response
	if err := json.Unmarshal(over.Body, &resp); err != nil {
		t.Fatalf("unmarshal: %v; raw: %s", err, over.Body)
	}
	if resp.Error == nil || resp.Error.Code != mcp.CodeTooManyListenStreams {
		t.Fatalf("Error = %+v, want CodeTooManyListenStreams", resp.Error)
	}
}

// ---- CloseSubscriptions -------------------------------------------------------

// TestCloseSubscriptions_SignalsDoneAndRefusesNewRegistrations verifies
// Server.CloseSubscriptions' own doc: every already-open Subscriber observes
// Done() closed (so its own stream handler can send the graceful-end
// response), and every subsequent subscriptions/listen request is refused
// with CodeSubscriptionsClosed / HintServiceUnavailable — never opening a new
// stream — from that point on, permanently.
func TestCloseSubscriptions_SignalsDoneAndRefusesNewRegistrations(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)
	id := mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-close"}

	res := listenOnce(t, s, id, 1)
	if res.Listen == nil {
		t.Fatalf("initial registration failed: %s", res.Body)
	}
	sub := res.Listen.Sub

	select {
	case <-sub.Done():
		t.Fatal("Done() already closed before CloseSubscriptions was ever called")
	default:
	}

	s.CloseSubscriptions()

	select {
	case <-sub.Done():
	default:
		t.Fatal("Done() not closed immediately after CloseSubscriptions")
	}

	complete := sub.CompleteMessage()
	if !strings.Contains(string(complete), `"resultType":"complete"`) {
		t.Errorf("CompleteMessage = %s, want resultType:complete", complete)
	}
	if !bytes.Contains(complete, []byte(`"id":1`)) {
		t.Errorf("CompleteMessage = %s, want id:1 echoed back", complete)
	}
	sub.Unregister()

	// A second CloseSubscriptions call must be a harmless no-op.
	s.CloseSubscriptions()

	after := listenOnce(t, s, id, 2)
	if after.Listen != nil {
		after.Listen.Sub.Unregister()
		t.Fatal("registration succeeded after CloseSubscriptions, want CodeSubscriptionsClosed")
	}
	if after.Hint != mcp.HintServiceUnavailable {
		t.Errorf("Hint = %v, want HintServiceUnavailable (HTTP 503)", after.Hint)
	}
	var resp mcp.Response
	if err := json.Unmarshal(after.Body, &resp); err != nil {
		t.Fatalf("unmarshal: %v; raw: %s", err, after.Body)
	}
	if resp.Error == nil || resp.Error.Code != mcp.CodeSubscriptionsClosed {
		t.Fatalf("Error = %+v, want CodeSubscriptionsClosed", resp.Error)
	}
}

// ---- NotifyToolsListChanged: coalescing and tenant scoping -------------------

// registerListener registers a subscriber for identity with toolsListChanged
// honored (requires the server to already have SetToolsListChangedSource(true)
// called), and returns its Subscriber for direct inspection of Events().
func registerListener(t *testing.T, s *mcp.Server, id mcp.KeyIdentity, reqID int) *mcp.Subscriber {
	t.Helper()
	res := listenOnce(t, s, id, reqID)
	if res.Listen == nil {
		t.Fatalf("registration failed: %s", res.Body)
	}
	if !res.Listen.Sub.Honored().ToolsListChanged {
		t.Fatal("registration did not honor toolsListChanged — fixture bug, not the condition under test")
	}
	t.Cleanup(res.Listen.Sub.Unregister)
	return res.Listen.Sub
}

// assertNoPendingEvent fails the test if sub has an event ready to read right
// now. Every call site in this file calls NotifyToolsListChanged synchronously
// from the SAME goroutine that then makes this assertion — delivery
// (subscriberRegistry.notify → Subscriber.deliver) is itself entirely
// synchronous, non-blocking, single-goroutine work (subscriptions.go), so
// there is no actual race to wait out here: if an event were going to be
// delivered, it already was, by the time Notify* returned.
func assertNoPendingEvent(t *testing.T, sub *mcp.Subscriber) {
	t.Helper()
	select {
	case body := <-sub.Events():
		t.Fatalf("unexpected pending event: %s", body)
	default:
	}
}

// requirePendingEvent fails the test if sub has no event ready to read right
// now, and returns the event body otherwise. See assertNoPendingEvent's own
// doc for why no wait is needed.
func requirePendingEvent(t *testing.T, sub *mcp.Subscriber) []byte {
	t.Helper()
	select {
	case body := <-sub.Events():
		return body
	default:
		t.Fatal("no pending event, want one")
		return nil
	}
}

// TestNotifyToolsListChanged_Coalesces verifies Subscriber.deliver's own
// contract: a burst of NotifyToolsListChanged calls delivered before the
// subscriber's own handler ever reads Events() coalesces into exactly one
// still-pending notification — never queued, never dropped entirely.
func TestNotifyToolsListChanged_Coalesces(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)
	s.SetAccessChecker(func(mcp.KeyIdentity, string, *mcp.NotifiedServerScope) bool { return true })

	id := mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-coalesce"}
	sub := registerListener(t, s, id, 1)

	for i := 0; i < 5; i++ {
		s.NotifyToolsListChanged(mcp.NotifyScope{ServerID: "server-x"})
	}

	requirePendingEvent(t, sub)  // exactly one delivered...
	assertNoPendingEvent(t, sub) // ...not five.
}

// TestNotifyToolsListChanged_NoAccessChecker_MatchesNobody verifies
// NotifyScope.matches' own fail-closed doc: a ServerID-scoped notification
// with no AccessChecker installed on the Server reaches no subscriber at all,
// never treated as "allow everyone".
func TestNotifyToolsListChanged_NoAccessChecker_MatchesNobody(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)
	// Deliberately never call SetAccessChecker.

	sub := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-no-checker"}, 1)

	s.NotifyToolsListChanged(mcp.NotifyScope{ServerID: "server-x"})

	assertNoPendingEvent(t, sub)
}

// TestNotifyToolsListChanged_ServerIDScope_OnlyMatchingAccessDelivers
// verifies the AccessChecker-driven per-server scoping NotifyScope{ServerID}
// implements: only a subscriber whose own identity the AccessChecker reports
// as having access to the changed server ID receives the notification.
func TestNotifyToolsListChanged_ServerIDScope_OnlyMatchingAccessDelivers(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)
	// Only "key-allowed" has access to "server-x".
	s.SetAccessChecker(func(id mcp.KeyIdentity, serverID string, snapshot *mcp.NotifiedServerScope) bool {
		return snapshot == nil && id.KeyID == "key-allowed" && serverID == "server-x"
	})

	allowed := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-allowed"}, 1)
	denied := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-denied"}, 2)

	s.NotifyToolsListChanged(mcp.NotifyScope{ServerID: "server-x"})

	got := requirePendingEvent(t, allowed)
	if !strings.Contains(string(got), "notifications/tools/list_changed") {
		t.Errorf("event = %s, want the tools/list_changed notification method", got)
	}
	if !bytes.Contains(got, []byte(`"io.modelcontextprotocol/subscriptionId":1`)) {
		t.Errorf("event = %s, want subscriptionId 1 (this subscriber's own request id)", got)
	}
	assertNoPendingEvent(t, denied)
}

// TestNotifyToolsListChanged_OrgIDScope_OnlyMatchingOrgDelivers verifies
// NotifyScope{OrgID}: delivery reaches every subscriber whose own
// KeyIdentity.OrgID equals the scope's OrgID, and no other — used for a
// mutation (an MCP access allowlist change) that affects a whole
// organization's visibility without naming one specific server.
func TestNotifyToolsListChanged_OrgIDScope_OnlyMatchingOrgDelivers(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)

	orgA := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-a", KeyID: "key-a"}, 1)
	orgB := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-b", KeyID: "key-b"}, 2)

	s.NotifyToolsListChanged(mcp.NotifyScope{OrgID: "org-a"})

	requirePendingEvent(t, orgA)
	assertNoPendingEvent(t, orgB)
}

// TestNotifyToolsListChanged_EmptyScope_MatchesNobody verifies
// NotifyScope.matches' default case: a NotifyScope with neither ServerID nor
// OrgID set reaches no one at all.
func TestNotifyToolsListChanged_EmptyScope_MatchesNobody(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)
	s.SetAccessChecker(func(mcp.KeyIdentity, string, *mcp.NotifiedServerScope) bool { return true })

	sub := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-empty-scope"}, 1)

	s.NotifyToolsListChanged(mcp.NotifyScope{})

	assertNoPendingEvent(t, sub)
}

// TestNotifyToolsListChanged_UnhonoredSubscriberNeverDelivered verifies that a
// subscriber whose own registration did NOT honor toolsListChanged (e.g. the
// built-in management server, which never enables the source at all) never
// receives a notification, even when the AccessChecker would otherwise grant
// it and the NotifyScope matches its identity.
func TestNotifyToolsListChanged_UnhonoredSubscriberNeverDelivered(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	// Deliberately never call SetToolsListChangedSource(true): mirrors the
	// built-in management server, which always acknowledges an empty honored
	// set (subscriptions.go, SetToolsListChangedSource's own doc).
	s.SetAccessChecker(func(mcp.KeyIdentity, string, *mcp.NotifiedServerScope) bool { return true })

	res := listenOnce(t, s, mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-mgmt"}, 1)
	if res.Listen == nil {
		t.Fatalf("registration failed: %s", res.Body)
	}
	t.Cleanup(res.Listen.Sub.Unregister)
	if res.Listen.Sub.Honored().ToolsListChanged {
		t.Fatal("fixture bug: this subscriber must NOT honor toolsListChanged")
	}

	s.NotifyToolsListChanged(mcp.NotifyScope{ServerID: "server-x"})

	assertNoPendingEvent(t, res.Listen.Sub)
}

// ---- zero-knowledge logging ---------------------------------------------------

// TestSubscriptionsListen_NoContentInLogs guards against a future regression
// where a debug/diagnostic log line is added to the subscriptions/listen path
// that embeds request content — this package logs nothing at all along this
// path today (subscriptions.go has no slog call site whatsoever), and this
// test pins that down for the malformed-input path specifically, since that
// is exactly the kind of place a "log what we rejected and why" line tends to
// get added later. It is deliberately NOT t.Parallel(): it swaps the
// process-global slog default logger for its duration, mirroring
// TestServer_EncodingFailure_LoggedWithoutBodyContent's identical convention
// in server_test.go.
func TestSubscriptionsListen_NoContentInLogs(t *testing.T) {
	const marker = "sk-super-secret-would-never-belong-in-a-log-line"

	var buf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)

	// A malformed notifications value that embeds the marker — decodeListenFilter
	// rejects this with CodeInvalidParams before ever constructing a filter.
	body := modernRequestBody(1, "subscriptions/listen",
		map[string]any{"notifications": marker}, nil)
	result := s.Handle(context.Background(), []byte(body), modernHeader())
	if result.Listen != nil {
		result.Listen.Sub.Unregister()
		t.Fatal("malformed request unexpectedly opened a stream")
	}

	// A well-formed request/notify/close cycle too.
	id := mcp.KeyIdentity{OrgID: "org-" + marker, KeyID: "key-" + marker}
	res := listenOnce(t, s, id, 2)
	if res.Listen == nil {
		t.Fatalf("well-formed registration failed: %s", res.Body)
	}
	s.SetAccessChecker(func(mcp.KeyIdentity, string, *mcp.NotifiedServerScope) bool { return true })
	s.NotifyToolsListChanged(mcp.NotifyScope{ServerID: "server-" + marker})
	res.Listen.Sub.Unregister()
	s.CloseSubscriptions()

	if strings.Contains(buf.String(), marker) {
		t.Errorf("log output contains the content marker, want it never logged:\n%s", buf.String())
	}
}

// ---- maxListenStreamsPerOrg -------------------------------------------------

// TestSubscriberRegistry_PerOrgLimit_65thStreamRejected verifies
// maxListenStreamsPerOrg (64): every API key belonging to one organization
// may hold at most 64 concurrent subscriptions/listen registrations against
// one Server instance IN TOTAL, even though each individual key is well
// under its own maxListenStreamsPerKey (4) limit — 64 distinct keys, one
// stream each, all in the same org. The 65th (a brand new key in the same
// org) is refused with CodeTooManyListenStreams/HintTooManyRequests before
// ever opening a stream; a key in a DIFFERENT org is unaffected.
func TestSubscriberRegistry_PerOrgLimit_65thStreamRejected(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)

	const maxListenStreamsPerOrg = 64
	var subs []*mcp.Subscriber
	t.Cleanup(func() {
		for _, sub := range subs {
			sub.Unregister()
		}
	})
	for i := 0; i < maxListenStreamsPerOrg; i++ {
		id := mcp.KeyIdentity{OrgID: "org-per-org-limit", KeyID: fmt.Sprintf("org-limit-key-%d", i)}
		res := listenOnce(t, s, id, i+1)
		if res.Listen == nil {
			t.Fatalf("registration %d/%d failed before reaching the per-org cap: %s", i+1, maxListenStreamsPerOrg, res.Body)
		}
		subs = append(subs, res.Listen.Sub)
	}

	over := listenOnce(t, s, mcp.KeyIdentity{OrgID: "org-per-org-limit", KeyID: "org-limit-key-overflow"}, maxListenStreamsPerOrg+1)
	if over.Listen != nil {
		over.Listen.Sub.Unregister()
		t.Fatal("65th registration for the same org succeeded, want CodeTooManyListenStreams")
	}
	if over.Hint != mcp.HintTooManyRequests {
		t.Errorf("Hint = %v, want HintTooManyRequests", over.Hint)
	}
	var resp mcp.Response
	if err := json.Unmarshal(over.Body, &resp); err != nil {
		t.Fatalf("unmarshal: %v; raw: %s", err, over.Body)
	}
	if resp.Error == nil || resp.Error.Code != mcp.CodeTooManyListenStreams {
		t.Fatalf("Error = %+v, want CodeTooManyListenStreams", resp.Error)
	}

	// A different org, and a different key within it, is unaffected.
	other := listenOnce(t, s, mcp.KeyIdentity{OrgID: "org-per-org-limit-other", KeyID: "unaffected-key"}, maxListenStreamsPerOrg+2)
	if other.Listen == nil {
		t.Fatalf("a different org's registration failed: %s", other.Body)
	}
	other.Listen.Sub.Unregister()
}

// TestSubscriberRegistry_PerOrgLimit_SlotReleasedAfterUnregister verifies
// that Subscriber.Unregister frees its maxListenStreamsPerOrg slot exactly
// as it already does for maxListenStreamsPerKey.
func TestSubscriberRegistry_PerOrgLimit_SlotReleasedAfterUnregister(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)

	const maxListenStreamsPerOrg = 64
	var subs []*mcp.Subscriber
	for i := 0; i < maxListenStreamsPerOrg; i++ {
		id := mcp.KeyIdentity{OrgID: "org-per-org-release", KeyID: fmt.Sprintf("org-release-key-%d", i)}
		res := listenOnce(t, s, id, i+1)
		if res.Listen == nil {
			t.Fatalf("registration %d failed: %s", i+1, res.Body)
		}
		subs = append(subs, res.Listen.Sub)
	}

	blocked := listenOnce(t, s, mcp.KeyIdentity{OrgID: "org-per-org-release", KeyID: "org-release-key-overflow"}, maxListenStreamsPerOrg+1)
	if blocked.Listen != nil {
		blocked.Listen.Sub.Unregister()
		t.Fatal("registration beyond the per-org cap succeeded before any slot was released")
	}

	subs[0].Unregister()
	t.Cleanup(func() {
		for _, sub := range subs[1:] {
			sub.Unregister()
		}
	})

	freed := listenOnce(t, s, mcp.KeyIdentity{OrgID: "org-per-org-release", KeyID: "org-release-key-freed"}, maxListenStreamsPerOrg+2)
	if freed.Listen == nil {
		t.Fatalf("registration after releasing a slot failed: %s", freed.Body)
	}
	t.Cleanup(freed.Listen.Sub.Unregister)
}

// ---- NotifyScope: TeamID, KeyID, and Server (snapshot) variants -------------

// TestNotifyToolsListChanged_TeamIDScope_OnlyMatchingTeamDelivers verifies
// NotifyScope{TeamID}: delivery reaches only a subscriber whose own
// KeyIdentity.TeamID equals the scope's TeamID — used for SetTeamMCPAccess's
// own precisely-scoped trigger (item 3).
func TestNotifyToolsListChanged_TeamIDScope_OnlyMatchingTeamDelivers(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)

	teamA := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-1", TeamID: "team-a", KeyID: "key-a"}, 1)
	teamB := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-1", TeamID: "team-b", KeyID: "key-b"}, 2)

	s.NotifyToolsListChanged(mcp.NotifyScope{TeamID: "team-a"})

	requirePendingEvent(t, teamA)
	assertNoPendingEvent(t, teamB)
}

// TestNotifyToolsListChanged_KeyIDScope_OnlyMatchingKeyDelivers verifies
// NotifyScope{KeyID}: delivery reaches only the subscriber whose own
// KeyIdentity.KeyID equals the scope's KeyID — used for SetKeyMCPAccess's
// own precisely-scoped trigger (item 3).
func TestNotifyToolsListChanged_KeyIDScope_OnlyMatchingKeyDelivers(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)

	keyA := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-a"}, 1)
	keyB := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-b"}, 2)

	s.NotifyToolsListChanged(mcp.NotifyScope{KeyID: "key-a"})

	requirePendingEvent(t, keyA)
	assertNoPendingEvent(t, keyB)
}

// TestNotifyToolsListChanged_ServerScope_NoAccessChecker_MatchesNobody
// verifies NotifyScope.matches' own fail-closed doc for the Server case, the
// exact same guarantee TestNotifyToolsListChanged_NoAccessChecker_MatchesNobody
// already proves for ServerID: item 2 moved ALL access-rule evaluation for a
// NotifiedServerScope snapshot out of this package and into the caller's own
// AccessChecker, so a Server-scoped notification with no checker installed
// must fail closed exactly like a ServerID-scoped one, never fall back to a
// permissive default of its own.
func TestNotifyToolsListChanged_ServerScope_NoAccessChecker_MatchesNobody(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)
	// Deliberately never call SetAccessChecker.

	sub := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-1", TeamID: "team-1", KeyID: "key-1"}, 1)

	orgID := "org-1"
	s.NotifyToolsListChanged(mcp.NotifyScope{Server: &mcp.NotifiedServerScope{
		ID:              "sv-x",
		OrgID:           &orgID,
		CodeModeEnabled: true,
		Active:          true,
	}})

	assertNoPendingEvent(t, sub)
}

// TestNotifyToolsListChanged_ServerScope_DelegatesToAccessChecker verifies
// that a Server-scoped NotifyToolsListChanged call hands the snapshot,
// verbatim, to the installed AccessChecker as its third argument (serverID
// empty, snapshot non-nil) — this package performs no access-rule evaluation
// of its own against a NotifiedServerScope (see that type's own doc); every
// rule (MCPAccessCache, the alias-winner rule, CodeModeEnabled, tenant
// scoping) lives entirely in the AccessChecker the caller installs
// (internal/app's codeModeAccessChecker in production).
func TestNotifyToolsListChanged_ServerScope_DelegatesToAccessChecker(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)

	// subscriberRegistry.notify snapshots its subscribers from a Go map
	// (subscriptions.go), so the ORDER the checker is invoked in across
	// "key-granted" and "key-denied" is not deterministic — every field
	// captured below is instead checked per-call, keyed by which identity
	// that particular call was for, rather than assuming a fixed call order.
	var mu sync.Mutex
	gotServerIDByKey := map[string]string{}
	gotSnapshotByKey := map[string]*mcp.NotifiedServerScope{}
	s.SetAccessChecker(func(id mcp.KeyIdentity, serverID string, snapshot *mcp.NotifiedServerScope) bool {
		mu.Lock()
		gotServerIDByKey[id.KeyID] = serverID
		gotSnapshotByKey[id.KeyID] = snapshot
		mu.Unlock()
		return snapshot != nil && id.KeyID == "key-granted"
	})

	granted := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-granted"}, 1)
	denied := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-denied"}, 2)

	orgID := "org-1"
	snapshot := mcp.NotifiedServerScope{ID: "sv-x", OrgID: &orgID, CodeModeEnabled: true, Active: true}
	s.NotifyToolsListChanged(mcp.NotifyScope{Server: &snapshot})

	requirePendingEvent(t, granted)
	assertNoPendingEvent(t, denied)

	mu.Lock()
	defer mu.Unlock()
	for _, keyID := range []string{"key-granted", "key-denied"} {
		if gotServerIDByKey[keyID] != "" {
			t.Errorf("checker's serverID argument for %q = %q, want empty for a Server-scoped call", keyID, gotServerIDByKey[keyID])
		}
		got := gotSnapshotByKey[keyID]
		if got == nil || got.ID != "sv-x" {
			t.Errorf("checker's snapshot argument for %q = %+v, want the exact NotifiedServerScope passed to NotifyScope{Server: ...}", keyID, got)
		}
	}
}

// ---- KeyValidator: revalidation-driven delivery filtering -------------------

// TestNotifyToolsListChanged_KeyValidator_InvalidSubscriberNeverDelivered
// verifies notify's own doc: a subscriber whose captured identity fails the
// installed KeyValidator never receives a delivery, even when scope and
// AccessChecker both otherwise grant it.
func TestNotifyToolsListChanged_KeyValidator_InvalidSubscriberNeverDelivered(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)
	s.SetAccessChecker(func(mcp.KeyIdentity, string, *mcp.NotifiedServerScope) bool { return true })
	s.SetKeyValidator(func(id mcp.KeyIdentity) bool { return id.KeyID != "key-revoked" })

	valid := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-valid"}, 1)
	revoked := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-revoked"}, 2)

	s.NotifyToolsListChanged(mcp.NotifyScope{ServerID: "server-x"})

	requirePendingEvent(t, valid)
	assertNoPendingEvent(t, revoked)
}

// TestNotifyToolsListChanged_NilKeyValidator_IsPermissive verifies
// KeyValidator's own doc: a nil validator (the default, e.g. a Handler built
// directly in a test) never suppresses delivery — unlike AccessChecker's
// fail-closed default.
func TestNotifyToolsListChanged_NilKeyValidator_IsPermissive(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)
	s.SetAccessChecker(func(mcp.KeyIdentity, string, *mcp.NotifiedServerScope) bool { return true })
	// Deliberately never call SetKeyValidator.

	sub := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-1"}, 1)

	s.NotifyToolsListChanged(mcp.NotifyScope{ServerID: "server-x"})

	requirePendingEvent(t, sub)
}

// TestServer_SubscriberValid mirrors the KeyValidator tests above at the
// Server.SubscriberValid/mcp_handler.go-facing level: it reports true when
// no validator is installed, and reflects the installed validator's own
// verdict once one is.
func TestServer_SubscriberValid(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)

	sub := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-1"}, 1)

	if !s.SubscriberValid(sub) {
		t.Error("SubscriberValid() = false with no KeyValidator installed, want true")
	}

	s.SetKeyValidator(func(mcp.KeyIdentity) bool { return false })
	if s.SubscriberValid(sub) {
		t.Error("SubscriberValid() = true, want false once the installed KeyValidator rejects this identity")
	}

	s.SetKeyValidator(func(mcp.KeyIdentity) bool { return true })
	if !s.SubscriberValid(sub) {
		t.Error("SubscriberValid() = false, want true once the installed KeyValidator accepts this identity again")
	}
}

// ---- decodeListenFilter: explicit JSON null on a notifications field -------

// TestSubscriptionsListen_ExplicitNullNotificationField_Rejected verifies
// decodeListenFilter's own doc: an explicit JSON null on any one of
// params.notifications' own four fields is rejected with CodeInvalidParams
// and HintBadRequest — never silently treated as "field absent" (the
// ordinary, and very different, encoding/json null-into-bool/[]string
// no-op).
func TestSubscriptionsListen_ExplicitNullNotificationField_Rejected(t *testing.T) {
	t.Parallel()

	fields := []string{"toolsListChanged", "promptsListChanged", "resourcesListChanged", "resourceSubscriptions"}

	for _, field := range fields {
		t.Run(field, func(t *testing.T) {
			t.Parallel()

			s := newTestServer("voidllm", "0.1.0")
			s.SetToolsListChangedSource(true)

			body := modernRequestBody(1, "subscriptions/listen",
				map[string]any{"notifications": json.RawMessage(`{"` + field + `":null}`)}, nil)
			result := s.Handle(context.Background(), []byte(body), modernHeader())

			if result.Listen != nil {
				t.Cleanup(result.Listen.Sub.Unregister)
				t.Fatalf("field %q: Listen != nil, want the explicit null rejected before ever opening a stream", field)
			}
			if result.Hint != mcp.HintBadRequest {
				t.Errorf("field %q: Hint = %v, want HintBadRequest (HTTP 400)", field, result.Hint)
			}
			var resp mcp.Response
			if err := json.Unmarshal(result.Body, &resp); err != nil {
				t.Fatalf("field %q: unmarshal error body: %v; raw: %s", field, err, result.Body)
			}
			if resp.Error == nil || resp.Error.Code != mcp.CodeInvalidParams {
				t.Errorf("field %q: Error = %+v, want CodeInvalidParams", field, resp.Error)
			}
		})
	}
}

// TestSubscriptionsListen_NullNotificationsItself_Rejected verifies
// decodeListenFilter's own doc for the OUTER field: params.notifications
// itself being an explicit JSON null is rejected with CodeInvalidParams and
// HintBadRequest, never silently treated as "field absent" — unlike a
// MISSING params.notifications key, which stays allowed (see
// TestSubscriptionsListen_Dispatch_HonoredFilter's "missing params.notifications
// entirely" case).
func TestSubscriptionsListen_NullNotificationsItself_Rejected(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)

	body := modernRequestBody(1, "subscriptions/listen", map[string]any{"notifications": nil}, nil)
	result := s.Handle(context.Background(), []byte(body), modernHeader())

	if result.Listen != nil {
		t.Cleanup(result.Listen.Sub.Unregister)
		t.Fatalf("Listen != nil, want an explicit null params.notifications rejected before ever opening a stream")
	}
	if result.Hint != mcp.HintBadRequest {
		t.Errorf("Hint = %v, want HintBadRequest (HTTP 400)", result.Hint)
	}
	var resp mcp.Response
	if err := json.Unmarshal(result.Body, &resp); err != nil {
		t.Fatalf("unmarshal error body: %v; raw: %s", err, result.Body)
	}
	if resp.Error == nil || resp.Error.Code != mcp.CodeInvalidParams {
		t.Errorf("Error = %+v, want CodeInvalidParams", resp.Error)
	}
}

// TestSubscriptionsListen_MissingNotifications_StaysAllowed verifies the
// counterpart of TestSubscriptionsListen_NullNotificationsItself_Rejected: a
// params.notifications key that is simply ABSENT (as opposed to present and
// explicitly null) is still treated as an empty filter and the stream still
// opens — decodeListenFilter's "missing means no value" convention is
// unchanged by the explicit-null rejection above.
func TestSubscriptionsListen_MissingNotifications_StaysAllowed(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)

	body := modernRequestBody(1, "subscriptions/listen", map[string]any{}, nil)
	result := s.Handle(context.Background(), []byte(body), modernHeader())
	if result.Listen == nil {
		t.Fatalf("registration failed: %s", result.Body)
	}
	t.Cleanup(result.Listen.Sub.Unregister)
	if result.Listen.Sub.Honored().ToolsListChanged {
		t.Error("Honored().ToolsListChanged = true, want false when params.notifications is absent")
	}
}

// ---- Server.WaitForSubscriptionsDrain ---------------------------------------

// TestServer_WaitForSubscriptionsDrain_WaitsThenReportsResult verifies both
// outcomes: it blocks until every currently registered Subscriber has been
// unregistered and reports true when that happens before the timeout, and
// reports false when at least one is still registered once the timeout
// elapses — the mechanism internal/app.Application.WaitForShutdown relies on
// (item 5) to bound how long shutdown waits for in-flight subscriptions/listen
// streams to finish after CloseSubscriptions.
func TestServer_WaitForSubscriptionsDrain_WaitsThenReportsResult(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.SetToolsListChangedSource(true)

	sub := registerListener(t, s, mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-drain"}, 1)

	// Not yet unregistered: a short wait must time out (false).
	if s.WaitForSubscriptionsDrain(20 * time.Millisecond) {
		t.Fatal("WaitForSubscriptionsDrain() = true before the subscriber was ever unregistered, want false")
	}

	// Unregister concurrently, after a short delay, proving this call
	// actually BLOCKS until that happens rather than merely polling once.
	go func() {
		time.Sleep(30 * time.Millisecond)
		sub.Unregister()
	}()
	if !s.WaitForSubscriptionsDrain(2 * time.Second) {
		t.Fatal("WaitForSubscriptionsDrain() = false, want true once the only registered subscriber unregistered")
	}

	// Nothing left registered: an immediate call reports true with no wait.
	if !s.WaitForSubscriptionsDrain(time.Millisecond) {
		t.Error("WaitForSubscriptionsDrain() = false with nothing registered, want true")
	}
}
