package mcp

import (
	"bytes"
	"context"
	"sync"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// maxListenStreamsPerKey bounds how many concurrent subscriptions/listen
// streams a single API key may hold open against one *Server instance at
// once. The built-in management server and the Code Mode server are two
// separate *Server instances, each with its own subscriberRegistry, so this
// limit applies independently to each. Checked by subscriberRegistry.register
// BEFORE a Subscriber (and therefore a stream) is ever handed back to a
// caller — see that method's own doc.
const maxListenStreamsPerKey = 4

// maxListenStreamsPerServer bounds how many concurrent subscriptions/listen
// streams a single *Server instance will hold open in total, across every
// caller, protecting the process from unbounded goroutine and channel growth
// regardless of how many distinct API keys are involved.
const maxListenStreamsPerServer = 1024

// ListenFilter is the set of notification types a subscriptions/listen
// request named (MCP 2026-07-28 §3.4), and — for Subscriber.Honored — the
// subset a Server actually delivers. Only ToolsListChanged is ever honored
// by either built-in server today; the other three fields are parsed (so a
// well-formed request naming them is never rejected) but never honored,
// since neither built-in server has a prompts or resources list-changed
// source. Bool fields use `omitempty` so a rendered ListenFilter (the
// acknowledgement's own params.notifications) carries only the honored
// subset, per the spec's "unsupported types are omitted" rule.
type ListenFilter struct {
	ToolsListChanged      bool     `json:"toolsListChanged,omitempty"`
	PromptsListChanged    bool     `json:"promptsListChanged,omitempty"`
	ResourcesListChanged  bool     `json:"resourcesListChanged,omitempty"`
	ResourceSubscriptions []string `json:"resourceSubscriptions,omitempty"`
}

// AccessChecker reports whether the caller identified by id currently has
// access to the MCP server identified by serverID — the same access
// decision that determines what id would actually see in that server's own
// tools/list content (see internal/app/code_mode.go's codeModeAccessChecker
// for the concrete implementation VoidLLM wires here, which mirrors
// codeModeService.accessibleServers for a single server ID). NotifyToolsListChanged
// consults it, per subscriber, to implement the tenant scoping documented on
// NotifyScope: a subscriber is only notified about a NotifyScope.ServerID
// change when this reports true for its own identity.
//
// Must not perform blocking I/O of unbounded duration, and production
// implementations must be built exclusively from in-memory caches — never a
// database call — since NotifyToolsListChanged may be invoked once per
// subscriber on every qualifying change.
type AccessChecker func(id KeyIdentity, serverID string) bool

// NotifyScope describes which subscribers a NotifyToolsListChanged call
// should reach. Exactly one of ServerID or OrgID should be set; callers that
// can name the specific MCP server a change affects should always prefer
// ServerID — it is checked against the subscriber's OWN identity via the
// Server's AccessChecker, the most precise scoping this package can
// express. OrgID is for a mutation that affects a whole organization's
// visibility without naming one specific server (an MCP access allowlist
// change). A NotifyScope with neither field set matches nobody — see
// NotifyScope.matches' own doc for the exact per-field decision.
type NotifyScope struct {
	// ServerID scopes delivery to subscribers whose identity the Server's
	// AccessChecker reports as having access to this MCP server ID.
	ServerID string
	// OrgID scopes delivery to subscribers whose own KeyIdentity.OrgID
	// equals this value.
	OrgID string
}

// matches reports whether a subscriber with the given identity should
// receive a notification for this NotifyScope, consulting checker (the
// Server's own AccessChecker, possibly nil) only for the ServerID case.
//
// A NotifyScope naming a ServerID with no AccessChecker installed (checker
// == nil) fails closed: matches returns false for every subscriber rather
// than guessing. This is deliberate — see AccessChecker's own doc on why a
// missing checker must never be treated as "allow everyone".
func (n NotifyScope) matches(id KeyIdentity, checker AccessChecker) bool {
	switch {
	case n.ServerID != "":
		return checker != nil && checker(id, n.ServerID)
	case n.OrgID != "":
		return n.OrgID == id.OrgID
	default:
		return false
	}
}

// Subscriber represents one open subscriptions/listen stream for a single
// caller identity. Server.handleSubscriptionsListen registers one via
// subscriberRegistry.register and returns it embedded in ListenRequest.Sub.
// The HTTP handler that owns the actual SSE connection
// (internal/api/admin/mcp_handler.go) reads Events() in a loop and MUST call
// Unregister exactly once — via defer, immediately after obtaining a
// Subscriber — on every exit path: a client disconnect, a write/flush
// error, the route's own max-duration timer firing, or Done() being
// signaled by Server.CloseSubscriptions on process shutdown. Skipping
// Unregister on any path would leak this Subscriber's slot in both
// maxListenStreamsPerKey and maxListenStreamsPerServer forever.
type Subscriber struct {
	id       uint64
	identity KeyIdentity
	honored  ListenFilter
	reqID    jsonx.RawMessage
	// toolsChangedBody is the pre-encoded notifications/tools/list_changed
	// event body for this subscriber (fixed for the subscriber's whole
	// lifetime, since it depends only on reqID and the constant method
	// name), or nil if honored.ToolsListChanged is false. Precomputed once at
	// registration so NotifyToolsListChanged's delivery loop performs no
	// per-notification JSON encoding.
	toolsChangedBody []byte
	events           chan []byte
	done             <-chan struct{}
	registry         *subscriberRegistry
	unregOnce        sync.Once
}

// Events returns the channel this Subscriber's stream handler must select on
// for pre-encoded SSE "data:" payload bytes — each a complete JSON-RPC
// notification (MCP 2026-07-28 §3.4). Buffered to depth 1 and coalescing: a
// burst of changes while the previous notification is still pending
// delivery is coalesced into that one still-pending notification rather than
// queued — see Server.NotifyToolsListChanged's own doc. Never closed; the
// handler learns the stream should end via Done(), not via this channel
// closing.
func (sub *Subscriber) Events() <-chan []byte { return sub.events }

// Done returns a channel that is closed when Server.CloseSubscriptions is
// called, signaling this stream's handler to send the graceful end response
// (see CompleteMessage) and return. It is never closed for any other reason
// — an ordinary Unregister does not close it.
func (sub *Subscriber) Done() <-chan struct{} { return sub.done }

// ID returns the JSON-RPC request ID of the subscriptions/listen request
// that opened this Subscriber, verbatim — the same value already encoded
// into every notification and the eventual graceful-end response on this
// stream as params._meta["io.modelcontextprotocol/subscriptionId"] (or, for
// the graceful end, echoed as its own top-level "id"). Exposed so an
// HTTP-aware caller that must refuse to actually open the stream after
// registration already succeeded (e.g. no viable max-duration budget — see
// internal/api/admin/mcp_handler.go) can still correlate its own error
// response to the request that triggered it.
func (sub *Subscriber) ID() jsonx.RawMessage { return sub.reqID }

// Honored reports which notification types this subscription actually
// honors — the same value already encoded into the initial acknowledgement
// this Subscriber's stream began with.
func (sub *Subscriber) Honored() ListenFilter { return sub.honored }

// CompleteMessage renders the graceful-end JSON-RPC response (MCP 2026-07-28
// §3.4, resultType: "complete") this Subscriber's stream handler must write
// and flush, then close the connection, whenever the server itself ends the
// subscription (a max-duration timer, or Server.CloseSubscriptions) — as
// opposed to the client cancelling by simply closing the stream, which needs
// no response at all.
func (sub *Subscriber) CompleteMessage() []byte {
	return encodeListenComplete(sub.reqID)
}

// Unregister removes this Subscriber from its Server's subscriberRegistry,
// releasing its maxListenStreamsPerKey/maxListenStreamsPerServer slot. Safe
// to call more than once — only the first call has any effect — and safe to
// call concurrently with NotifyToolsListChanged. Callers MUST call this on
// every exit path; see the type's own doc.
func (sub *Subscriber) Unregister() {
	sub.unregOnce.Do(func() { sub.registry.unregister(sub) })
}

// deliver attempts to push body onto this Subscriber's Events() channel
// without blocking. If a notification is already pending delivery (the
// buffered channel is full), body is dropped rather than queued: the
// pending notification already tells the client to re-fetch tools/list, and
// VoidLLM's list_changed notifications carry no per-change payload beyond
// that signal, so a second, redundant one adds nothing worth blocking the
// caller of NotifyToolsListChanged for.
func (sub *Subscriber) deliver(body []byte) {
	select {
	case sub.events <- body:
	default:
	}
}

// subscriberRegistry tracks every open subscriptions/listen Subscriber for
// one *Server instance, enforcing maxListenStreamsPerKey and
// maxListenStreamsPerServer before a stream is ever opened (see register's
// own doc), and supporting a one-way close (Server.CloseSubscriptions) that
// signals every currently open stream to end gracefully and permanently
// refuses any further registration.
type subscriberRegistry struct {
	mu      sync.Mutex
	byKey   map[string]int
	all     map[uint64]*Subscriber
	nextID  uint64
	closed  bool
	closeCh chan struct{}
}

// newSubscriberRegistry returns a ready-to-use, empty subscriberRegistry.
func newSubscriberRegistry() *subscriberRegistry {
	return &subscriberRegistry{
		byKey:   make(map[string]int),
		all:     make(map[uint64]*Subscriber),
		closeCh: make(chan struct{}),
	}
}

// errListenTooManyStreams is the JSON-RPC error register returns when
// registering would exceed maxListenStreamsPerKey or
// maxListenStreamsPerServer. A single shared *Error value: it carries no
// per-request state (the message is static, per this package's
// zero-knowledge-logging rule — no caller-supplied data is ever embedded in
// it), so there is no reason to allocate a fresh one per rejection.
var errListenTooManyStreams = &Error{
	Code:    CodeTooManyListenStreams,
	Message: "too many concurrent subscriptions/listen streams",
	Hint:    HintTooManyRequests,
}

// errListenRegistryClosed is the JSON-RPC error register returns once close
// has already been called — see close's own doc. Its Hint is explicitly
// HintServiceUnavailable so an HTTP-aware caller answers 503 rather than the
// ordinary HintOK (200) JSON-RPC-error convention.
var errListenRegistryClosed = &Error{
	Code:    CodeSubscriptionsClosed,
	Message: "subscriptions/listen is no longer accepted: the server is shutting down",
	Hint:    HintServiceUnavailable,
}

// register creates and records a new Subscriber for identity, honoring
// honored and acknowledging reqID, enforcing maxListenStreamsPerKey and
// maxListenStreamsPerServer BEFORE the Subscriber is ever constructed or
// handed back — a caller that receives a non-nil *Subscriber from this
// method is guaranteed to have consumed exactly one slot under both limits,
// and a caller that receives an error is guaranteed to have opened no stream
// at all. identity.KeyID is the per-key bucket key; an empty KeyID (a
// request with no authenticated identity attached — see KeyIdentityFromCtx)
// shares a single bucket, which is a strictly more conservative limit for
// that case, never a looser one.
func (r *subscriberRegistry) register(identity KeyIdentity, honored ListenFilter, reqID jsonx.RawMessage) (*Subscriber, *Error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil, errListenRegistryClosed
	}
	if len(r.all) >= maxListenStreamsPerServer {
		return nil, errListenTooManyStreams
	}
	if r.byKey[identity.KeyID] >= maxListenStreamsPerKey {
		return nil, errListenTooManyStreams
	}

	r.nextID++
	sub := &Subscriber{
		id:       r.nextID,
		identity: identity,
		honored:  honored,
		reqID:    append(jsonx.RawMessage(nil), reqID...),
		events:   make(chan []byte, 1),
		done:     r.closeCh,
		registry: r,
	}
	if honored.ToolsListChanged {
		sub.toolsChangedBody = encodeListenNotification(methodToolsListChanged, sub.reqID)
	}
	r.all[sub.id] = sub
	r.byKey[identity.KeyID]++
	return sub, nil
}

// unregister removes sub from the registry, releasing its slot. A no-op if
// sub was already unregistered (or never made it into the registry, which
// never actually happens in practice — register always inserts before
// returning a non-nil Subscriber).
func (r *subscriberRegistry) unregister(sub *Subscriber) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.all[sub.id]; !ok {
		return
	}
	delete(r.all, sub.id)
	r.byKey[sub.identity.KeyID]--
	if r.byKey[sub.identity.KeyID] <= 0 {
		delete(r.byKey, sub.identity.KeyID)
	}
}

// close permanently disables this registry: closeCh is closed exactly once,
// signaling Done() on every currently registered Subscriber, and every
// future call to register fails with errListenRegistryClosed. Safe to call
// more than once — only the first call has any effect.
func (r *subscriberRegistry) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	close(r.closeCh)
}

// notify delivers body to every currently registered Subscriber that honors
// toolsListChanged and whose identity scope matches, per checker (see
// NotifyScope.matches). The registry's own lock is held only long enough to
// snapshot the current subscriber list — never while calling checker or
// Subscriber.deliver, both of which run outside it — so a slow or
// long-running AccessChecker cannot block a concurrent register/unregister
// call.
func (r *subscriberRegistry) notify(scope NotifyScope, checker AccessChecker) {
	r.mu.Lock()
	subs := make([]*Subscriber, 0, len(r.all))
	for _, sub := range r.all {
		subs = append(subs, sub)
	}
	r.mu.Unlock()

	for _, sub := range subs {
		if !sub.honored.ToolsListChanged || sub.toolsChangedBody == nil {
			continue
		}
		if !scope.matches(sub.identity, checker) {
			continue
		}
		sub.deliver(sub.toolsChangedBody)
	}
}

// ListenRequest is HandleResult's payload for a successful
// subscriptions/listen dispatch (see HandleResult.Listen's own doc): instead
// of an ordinary JSON-RPC response body, it carries everything an
// HTTP-aware caller needs to open and drive the resulting SSE stream itself.
type ListenRequest struct {
	// Ack is the pre-encoded notifications/subscriptions/acknowledged event
	// body (MCP 2026-07-28 §3.4) — the exact bytes the caller must write as
	// the stream's first event, flushed, before entering its own
	// notification delivery loop.
	Ack []byte
	// Sub is the registered Subscriber for this stream. The caller reads
	// Sub.Events() in a loop and MUST call Sub.Unregister() on every exit
	// path — see Subscriber's own doc.
	Sub *Subscriber
}

// SetToolsListChangedSource declares whether this Server actually has a
// source of tools/list_changed events to deliver — i.e. whether
// NotifyToolsListChanged will ever meaningfully be called for it. It drives
// two things together, so they can never drift apart:
//
//   - handleDiscover's own capabilities.tools.listChanged, which MUST only
//     be advertised true when the server actually emits the notification
//     (MCP 2026-07-28 §1a).
//   - handleSubscriptionsListen's own honored-filter decision: a client
//     asking for toolsListChanged when this is false gets an
//     acknowledgement with an EMPTY honored set (MCP 2026-07-28 §3.4:
//     "unsupported types are omitted"), and the resulting stream then never
//     receives any notification — only keep-alives — until it ends.
//
// Defaults to false. The built-in management server (voidllm) never calls
// this: its own tool list never changes at runtime, so it always
// acknowledges with an empty honored set. The Code Mode server calls this
// with true, since its tools/list content changes whenever an upstream
// MCP server's tools change (see internal/app wiring, code_mode.go).
//
// Safe to call concurrently with Handle; production wiring calls this once,
// at startup, before serving requests.
func (s *Server) SetToolsListChangedSource(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolsListChangedSource = enabled
}

// SetAccessChecker installs checker as this Server's AccessChecker (see that
// type's own doc). A nil checker (the default) makes NotifyToolsListChanged
// fail closed for any NotifyScope naming a ServerID — see NotifyScope.matches.
// Safe to call concurrently with Handle.
func (s *Server) SetAccessChecker(checker AccessChecker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accessChecker = checker
}

// NotifyToolsListChanged delivers a notifications/tools/list_changed event
// (MCP 2026-07-28 §3.4) to every currently open Subscriber that honored
// toolsListChanged and whose identity scope matches scope, per this Server's
// own AccessChecker — the tenant scoping this package implements for
// subscriptions/listen; see NotifyScope's own doc for the precise per-field
// rules. Delivery is non-blocking and coalescing per subscriber
// (Subscriber.deliver): this call never waits on a subscriber's own stream
// handler to drain its channel.
//
// Safe to call concurrently with Handle, with itself, and with a
// subscriber's own Unregister.
func (s *Server) NotifyToolsListChanged(scope NotifyScope) {
	s.mu.RLock()
	checker := s.accessChecker
	s.mu.RUnlock()
	s.subscribers.notify(scope, checker)
}

// CloseSubscriptions signals every currently open subscriptions/listen
// stream on this Server to send its graceful-end response and return (via
// Subscriber.Done), and permanently refuses any further subscriptions/listen
// registration from this point forward. Call this for every *Server the
// process serves subscriptions on BEFORE shutting down the Fiber app(s)
// hosting them (internal/app.Application.WaitForShutdown), so an in-flight
// stream's handler observes Done() and writes its own graceful-end response
// well before the hosting app's own WriteTimeout — or Shutdown itself —
// would otherwise terminate the connection uncleanly.
//
// Safe to call more than once; only the first call has any effect. Does not
// block waiting for handlers to actually finish writing their own
// graceful-end response.
func (s *Server) CloseSubscriptions() {
	s.subscribers.close()
}

// handleSubscriptionsListen implements the modern-era subscriptions/listen
// RPC (MCP 2026-07-28 §3.4). Unlike every other modern-era method, it does
// not produce an ordinary JSON-RPC response body: on success it returns a
// *Result whose Listen field carries the registered Subscriber and the
// pre-encoded initial acknowledgement event — Server.Handle recognizes this
// (see its own doc) and hands both to its own caller via
// HandleResult.Listen, unencoded, instead of calling a ServerDialect's
// EncodeResult.
//
// A request carrying no JSON-RPC id (env.IsNotification) is answered with an
// empty, discarded *Result and no registration at all: the acknowledgement a
// subscriptions/listen response depends on can never be delivered to a
// notification's caller in the first place — Handle's own IsNotification
// check (which runs immediately after dispatch returns, before this
// Result's Listen field would ever be consulted) already discards whatever
// this method returns, so registering a Subscriber here would only leak its
// slot forever with no path to Unregister it.
func (s *Server) handleSubscriptionsListen(ctx context.Context, env *Envelope) (*Result, *Error) {
	if env.IsNotification {
		return &Result{}, nil
	}

	requested, decErr := decodeListenFilter(env.Params)
	if decErr != nil {
		return nil, decErr
	}

	s.mu.RLock()
	toolsSourceEnabled := s.toolsListChangedSource
	s.mu.RUnlock()

	honored := ListenFilter{}
	if requested.ToolsListChanged && toolsSourceEnabled {
		honored.ToolsListChanged = true
	}

	identity := KeyIdentityFromCtx(ctx)
	sub, regErr := s.subscribers.register(identity, honored, env.ID)
	if regErr != nil {
		return nil, regErr
	}

	ack := encodeListenAck(env.ID, honored)
	return &Result{Listen: &ListenRequest{Ack: ack, Sub: sub}}, nil
}

// listenNotificationsField is the shape decodeListenFilter parses
// params.notifications into — the raw request's own field names (MCP
// 2026-07-28 §3.4), independent of ListenFilter's `omitempty`-tagged
// rendering shape used for the acknowledgement's honored subset.
type listenNotificationsField struct {
	ToolsListChanged      bool     `json:"toolsListChanged"`
	PromptsListChanged    bool     `json:"promptsListChanged"`
	ResourcesListChanged  bool     `json:"resourcesListChanged"`
	ResourceSubscriptions []string `json:"resourceSubscriptions"`
}

// decodeListenFilter parses params — a subscriptions/listen request's raw
// JSON-RPC params, verbatim (Envelope.Params) — into the ListenFilter it
// requested. A missing or explicit-null params.notifications is treated as
// an empty filter (matching this package's existing convention — see
// bodyProtocolVersion's identical "absent means no value" treatment). A
// params.notifications present but not a JSON object, or with a field of the
// wrong type, is rejected with CodeInvalidParams and HintBadRequest — an
// explicit override, mirroring dispatch's own tools/list cursor-rejection
// precedent (see dispatch's own doc for the other case that already carries
// this same override) — since a malformed subscriptions/listen request is
// exactly the kind of caller-visible protocol violation the modern era
// always reports as HTTP 400.
func decodeListenFilter(params jsonx.RawMessage) (ListenFilter, *Error) {
	if len(params) == 0 {
		return ListenFilter{}, nil
	}
	var top map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(params, &top); err != nil {
		return ListenFilter{}, &Error{Code: CodeInvalidParams, Message: "invalid params", Hint: HintBadRequest}
	}
	notifRaw, ok := top["notifications"]
	if !ok || bytes.Equal(bytes.TrimSpace(notifRaw), []byte("null")) {
		return ListenFilter{}, nil
	}
	var f listenNotificationsField
	if err := jsonx.Unmarshal(notifRaw, &f); err != nil {
		return ListenFilter{}, &Error{Code: CodeInvalidParams, Message: "invalid params: notifications must be an object", Hint: HintBadRequest}
	}
	return ListenFilter{
		ToolsListChanged:      f.ToolsListChanged,
		PromptsListChanged:    f.PromptsListChanged,
		ResourcesListChanged:  f.ResourcesListChanged,
		ResourceSubscriptions: f.ResourceSubscriptions,
	}, nil
}

// listenMeta renders the params._meta object every subscriptions/listen
// acknowledgement, notification, and graceful-end response carries: the
// JSON-RPC id of the originating subscriptions/listen request, under
// metaSubscriptionID (MCP 2026-07-28 §3.4). Field name matches
// listen_client.go's own reader of an identical message shape received FROM
// an upstream server — this type is the server-rendering mirror of that
// client-side parser.
type listenMeta struct {
	SubscriptionID jsonx.RawMessage `json:"io.modelcontextprotocol/subscriptionId"`
}

// encodeListenAck renders the notifications/subscriptions/acknowledged
// message (MCP 2026-07-28 §3.4) that must be the first event on every
// subscriptions/listen stream, naming id as the subscriptionId and honored
// as the honored notification subset. honored's own `omitempty` tags ensure
// only the actually-honored types appear in the rendered notifications
// object, per the spec's "unsupported types are omitted" rule.
func encodeListenAck(id jsonx.RawMessage, honored ListenFilter) []byte {
	msg := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			Meta          listenMeta   `json:"_meta"`
			Notifications ListenFilter `json:"notifications"`
		} `json:"params"`
	}{JSONRPC: "2.0", Method: methodSubscriptionsAcknowledged}
	msg.Params.Meta = listenMeta{SubscriptionID: id}
	msg.Params.Notifications = honored

	out, err := jsonx.Marshal(msg)
	if err != nil {
		// Every field here is either a compile-time-fixed string, an
		// already-validated jsonx.RawMessage decoded from the originating
		// request (id — see peekID's identical guarantee), or plain
		// bools/[]string — this is unreachable in practice. The fallback
		// mirrors encodeFallback's own reasoning (server.go): never let an
		// encoding failure silently produce an empty event.
		return []byte(`{"jsonrpc":"2.0","method":"` + methodSubscriptionsAcknowledged + `","params":{"_meta":{},"notifications":{}}}`)
	}
	return out
}

// encodeListenNotification renders a single server-initiated JSON-RPC
// notification carrying only params._meta.subscriptionId — the shape every
// subscriptions/listen notification type this package delivers uses (today,
// only notifications/tools/list_changed, via methodToolsListChanged).
func encodeListenNotification(method string, id jsonx.RawMessage) []byte {
	msg := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			Meta listenMeta `json:"_meta"`
		} `json:"params"`
	}{JSONRPC: "2.0", Method: method}
	msg.Params.Meta = listenMeta{SubscriptionID: id}

	out, err := jsonx.Marshal(msg)
	if err != nil {
		// See encodeListenAck's identical, effectively unreachable fallback.
		return []byte(`{"jsonrpc":"2.0","method":"` + method + `","params":{"_meta":{}}}`)
	}
	return out
}

// encodeListenComplete renders the server-initiated graceful-end response
// (MCP 2026-07-28 §3.4: id equal to the subscriptions/listen request's own
// id, result.resultType "complete") a stream handler writes and flushes
// before closing the connection, whenever the SERVER itself ends the
// subscription (as opposed to a client-initiated cancel, which needs no
// response — the client simply closes the stream).
func encodeListenComplete(id jsonx.RawMessage) []byte {
	msg := struct {
		JSONRPC string           `json:"jsonrpc"`
		ID      jsonx.RawMessage `json:"id"`
		Result  struct {
			ResultType string     `json:"resultType"`
			Meta       listenMeta `json:"_meta"`
		} `json:"result"`
	}{JSONRPC: "2.0", ID: id}
	msg.Result.ResultType = "complete"
	msg.Result.Meta = listenMeta{SubscriptionID: id}

	out, err := jsonx.Marshal(msg)
	if err != nil {
		// See encodeListenAck's identical, effectively unreachable fallback.
		idLiteral := "null"
		if len(id) > 0 {
			idLiteral = string(id)
		}
		return []byte(`{"jsonrpc":"2.0","id":` + idLiteral + `,"result":{"resultType":"complete"}}`)
	}
	return out
}
