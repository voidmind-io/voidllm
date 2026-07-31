package mcp

import (
	"context"
	"errors"
	"sync"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// errNilCapabilitiesReceiver guards Capabilities.UnmarshalJSON against a nil
// pointer receiver, mirroring encoding/json.RawMessage's own guard.
var errNilCapabilitiesReceiver = errors.New("mcp: Capabilities: UnmarshalJSON on nil pointer")

// errNilResult is returned by a ServerDialect's EncodeResult when r is nil.
// By the time Handle calls EncodeResult, a nil *Result always indicates a
// programming error in a dispatch path (env.IsNotification is false — a
// genuine notification never reaches EncodeResult at all — yet no Result was
// produced). This must never be allowed to silently degrade into a
// JSON-RPC response carrying neither "result" nor "error": returning a
// non-nil error here routes the failure through Handle's existing
// logEncodeFailure/encodeFallback path instead, which renders a
// CodeInternalError response.
var errNilResult = errors.New("mcp: EncodeResult called with a nil Result")

// Header abstracts inbound transport headers so internal/mcp has no
// compile-time dependency on Fiber or any other HTTP framework, keeping the
// package standalone (see the package doc in protocol.go). Get returns the
// header value for name, looked up case-insensitively as HTTP header names
// require, or the empty string if the header is absent.
type Header interface {
	Get(name string) string
}

// MapHeader is a Header backed by a plain map. It is useful for constructing
// synthetic requests — such as in-process tool calls that never travel over
// HTTP — and in tests, without depending on any HTTP framework's context
// type. Lookups are case-insensitive; a nil MapHeader behaves like an empty
// one.
type MapHeader map[string]string

// Get implements Header.
func (h MapHeader) Get(name string) string {
	for k, v := range h {
		if equalFoldASCII(k, name) {
			return v
		}
	}
	return ""
}

// equalFoldASCII reports whether a and b are equal under ASCII case-folding.
// HTTP header names are ASCII, so this avoids pulling in strings.EqualFold's
// full Unicode case-folding for a hot, small comparison.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// ClientInfo identifies the calling MCP client by name and version. It is
// populated from the initialize handshake's params.clientInfo (legacy) or
// from params._meta["io.modelcontextprotocol/clientInfo"] (modern). Both are
// SHOULD, not MUST, fields — a zero ClientInfo is valid and means the client
// did not self-identify.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Capabilities is a client's or server's declared MCP capability set,
// including any extensions, carried verbatim as raw JSON. It is a raw
// message rather than a struct because the capability object is
// intentionally open-ended: SDKs and extensions add keys VoidLLM does not
// need to interpret in Phase 1, only to store and round-trip.
type Capabilities jsonx.RawMessage

// MarshalJSON returns the raw JSON bytes c contains, or the JSON null literal
// if c is empty. Without this method, a named []byte type such as
// Capabilities would marshal as a base64 string instead of the JSON object it
// holds — the same pitfall JSONSchema in protocol.go guards against.
func (c Capabilities) MarshalJSON() ([]byte, error) {
	if len(c) == 0 {
		return []byte("null"), nil
	}
	return c, nil
}

// UnmarshalJSON sets *c to a copy of data. It implements json.Unmarshaler so
// Capabilities round-trips as a raw JSON value instead of base64.
func (c *Capabilities) UnmarshalJSON(data []byte) error {
	if c == nil {
		return errNilCapabilitiesReceiver
	}
	*c = append((*c)[0:0], data...)
	return nil
}

// Envelope is the era-neutral form of an inbound MCP request. Every
// ServerDialect decodes to and encodes from this type, so tool dispatch,
// access control and usage logging never branch on protocol version.
type Envelope struct {
	// ID is the JSON-RPC request ID, verbatim. Empty for notifications.
	ID jsonx.RawMessage
	// Method is the JSON-RPC method name.
	Method string
	// Name is the tool name or resource URI the request targets; empty when
	// the method has no such target (e.g. tools/list, server/discover).
	Name string
	// Params is the raw JSON-RPC params value, verbatim.
	Params jsonx.RawMessage
	// Version is body-derived, informational metadata only — for legacy
	// requests, the version the client asked to mirror in initialize; for
	// modern requests, params._meta["io.modelcontextprotocol/protocolVersion"].
	// Both default to the dialect's own (already-negotiated) Version when
	// absent, unrecognized, or naming a version outside the dialect's era —
	// dialects never let a body value switch era; see Decode on each
	// ServerDialect implementation.
	//
	// MUST NEVER DRIVE CONTROL FLOW. It is attacker-controlled: any caller
	// can put any string here. The protocol era — which methods exist, how
	// results are shaped — is resolved exactly once, at the edge, by
	// Negotiate, and lives in the ServerDialect a request was decoded with,
	// not in this field. Dispatch branches on the dialect's Version().Era(),
	// never on Envelope.Version.Era(). The only legitimate uses of this field
	// are read-only: mirroring it back in a result (e.g. legacy
	// initialize's protocolVersion) or logging.
	Version Version
	// ClientInfo is the caller's self-reported identity, if any.
	ClientInfo ClientInfo
	// ClientCaps is the caller's declared capability set, if any.
	ClientCaps Capabilities
	// LogLevel is the per-request log level from modern requests
	// (params._meta["io.modelcontextprotocol/logLevel"]). Empty for legacy
	// requests, which have no per-request log level.
	LogLevel string
	// IsNotification is true when the request carries no ID and therefore
	// requires no response, per JSON-RPC 2.0.
	IsNotification bool
}

// CacheHint carries the CacheableResult fields from MCP 2026-07-28: how long
// a result may be considered fresh, and whether it may be shared across
// callers. See docs/mcp-v2.md §5.
type CacheHint struct {
	// TTLMs is the freshness window in milliseconds. Must be >= 0; negative
	// values are treated as 0 by dialects that render this hint.
	TTLMs int64
	// TTLMsSet reports whether TTLMs carries a hint the consuming cache must
	// honor, as opposed to being the zero value because there was no hint at
	// all to read. Without this field, TTLMs == 0 would be ambiguous between
	// two things §5 requires to be treated differently: an explicit
	// ttlMs: 0 ("immediately stale") and an upstream that gave no hint at
	// all (the consuming cache falls back to its own default).
	//
	// For a modern resultType:"complete" response, TTLMsSet is true when
	// EITHER ttlMs or cacheScope is present on the wire, and false only when
	// BOTH are absent — see dialect2026Client.parseCacheHint's own doc for
	// why: §5's "a missing ttlMs defaults to 0" governs a ttlMs that is
	// missing WITHIN an otherwise-present CacheableResult hint, not the
	// absence of the hint itself, and a response with neither field carries
	// no hint to default anything within. Once at least one field is
	// present, that hint IS regarded as offered, and a still-missing or
	// unusable ttlMs within it does default to 0 per §5, exactly as before.
	// TTLMsSet is also false for a legacy-era response, which predates
	// CacheableResult entirely (legacyClientDialect.Parse), and for anything
	// that never reaches a resultType:"complete" Result at all — an HTTP 202
	// notification acknowledgement, or a resultType this package does not
	// treat as cacheable. dialect2026.EncodeResult (the server-rendering
	// direction) ignores this field entirely — it decides whether to emit
	// ttlMs/cacheScope from Scope being non-empty, not from TTLMsSet — since
	// TTLMsSet exists purely for the parse direction, where an upstream's
	// response is being interpreted rather than VoidLLM's own being built.
	TTLMsSet bool
	// Scope is CacheScopePublic or CacheScopePrivate. The zero value (empty
	// string) means "no cache hint" — dialects omit ttlMs/cacheScope from the
	// wire entirely rather than emitting an empty scope.
	Scope string
}

// CallRequest is the era-neutral form of an outbound MCP request: the input
// to ClientDialect.Prepare. It is the client-side mirror of Envelope — where
// Envelope lets tool dispatch, access control, and usage logging operate on
// an inbound request without ever branching on protocol era, CallRequest
// lets a caller of HTTPTransport.Call hand over an outbound request without
// knowing which era it will ultimately be prepared for.
type CallRequest struct {
	// Raw is the already-serialized outbound JSON-RPC request or
	// notification, exactly as ClientDialect.Prepare's raw parameter was
	// before this type existed.
	Raw []byte
	// HeaderParams is the addressed tool's validated x-mcp-header bindings
	// (MCP 2026-07-28 §4.3; see ToolHeaderParams). The caller (CallMCPTool)
	// sets this UNCONDITIONALLY, regardless of which era the resolved
	// upstream turns out to speak — it is never gated on an
	// `if era == modern` check anywhere in this codebase. Only
	// dialect2026Client.Prepare actually renders these as outbound
	// Mcp-Param-{Name} headers; legacyClientDialect.Prepare ignores this
	// field entirely (see its own doc for why). Which era understands
	// Mcp-Param-* at all is a property of the resolved ClientDialect, never
	// a property of the caller building this request — that is the entire
	// point of keeping this field era-agnostic here. Always empty for a
	// request whose method is not tools/call.
	HeaderParams []HeaderParam
}

// Cache scope values for CacheHint.Scope, per the CacheableResult vocabulary.
const (
	// CacheScopePublic marks a result as containing no caller-specific data:
	// any client, shared gateway, or caching proxy may store and reuse it for
	// any caller.
	CacheScopePublic = "public"
	// CacheScopePrivate marks a result as containing caller-specific data:
	// reuse is only safe within the same authorization context that produced
	// it.
	CacheScopePrivate = "private"
)

// Result is the era-neutral form of a successful MCP result. Payload is
// arbitrary rather than a concrete type because it carries fundamentally
// different shapes across methods — a tool call's ToolResult, a discover
// document, a tools/list document — and every ServerDialect must be able to
// encode all of them without internal/mcp modeling each method's response
// type. Cache is ignored by legacy dialects, which predate the
// CacheableResult concept and have no on-wire representation for it.
type Result struct {
	Payload any
	Cache   CacheHint
}

// ServerDialect renders inbound wire messages to Envelopes and Results back
// to the wire, for exactly one MCP era. Tool dispatch, access control, and
// usage logging operate purely on Envelope and Result; only a ServerDialect
// implementation is aware of which era's wire format is in play.
type ServerDialect interface {
	// Version returns the protocol revision this dialect was resolved for.
	Version() Version
	// Decode parses a raw JSON-RPC request into its era-neutral Envelope.
	// hdr provides the inbound transport headers; legacy dialects ignore it.
	// Returns a JSON-RPC error if raw is not a well-formed request this
	// dialect can decode.
	Decode(raw []byte, hdr Header) (*Envelope, *Error)
	// EncodeResult renders a successful Result as this dialect's wire format
	// for the given request ID. Returns a non-nil error if the underlying
	// JSON encoding fails, or if r is nil (see errNilResult) — callers
	// (Server.Handle) must not treat a nil byte slice on the success path as
	// "no response needed" the way they do for a genuine notification; that
	// signal is IsNotification, not an empty return here. See Handle's
	// fallback for this failure mode.
	EncodeResult(id jsonx.RawMessage, r *Result) ([]byte, error)
	// EncodeError renders a JSON-RPC error as this dialect's wire format for
	// the given request ID. Returns a non-nil error under the same
	// encoding-failure condition as EncodeResult.
	EncodeError(id jsonx.RawMessage, e *Error) ([]byte, error)
}

// RoundTripResult is the outcome of a single RoundTrip call: the HTTP status
// code, the response body, and the response's transport headers. The status
// is deliberately part of the contract, not a detail a ClientDialect could
// do without — without it, Warmup cannot tell a successful handshake from an
// upstream error (an HTTP 5xx, or an HTTP 200 wrapping a JSON-RPC error
// object) and would silently treat both as a healthy connection. See
// legacyClientDialect.Warmup for how it is used.
type RoundTripResult struct {
	// Status is the HTTP status code of the response.
	Status int
	// Body is the raw response body bytes.
	Body []byte
	// Header carries the response's transport headers.
	Header Header
}

// RoundTripper sends a single raw JSON-RPC HTTP POST to an upstream MCP
// server, with hdr merged in as extra request headers, and returns the
// response as a RoundTripResult. It is the narrow surface a ClientDialect
// needs to drive its own handshake (Warmup) — legacyClientDialect's
// initialize plus notifications/initialized — without depending on
// HTTPTransport itself, which would create an import cycle between the
// dialect implementations and the transport that resolves which one to use.
// It performs no interpretation of the JSON-RPC payload; it is a transport
// primitive, not a protocol one.
type RoundTripper interface {
	RoundTrip(ctx context.Context, raw []byte, hdr MapHeader) (*RoundTripResult, error)
}

// SessionScope isolates legacy MCP sessions between tenants that share the
// same upstream server. HTTPTransport caches one eraBinding per server ID,
// but a single global server (org_id IS NULL) may be reachable by many
// organizations; without a scope, every one of them would share a single
// legacy Mcp-Session-Id and could observe upstream state left behind by
// another org (docs/mcp-v2.md §11.3 Befund 1). Call takes the caller's
// organization ID as scope so EraLegacy's per-connection session lives in
// its own UpstreamState per org. In the modern era SessionScope is
// meaningless — there is no session to isolate — and Call's handling of it
// is a no-op for EraModern dialects.
//
// Forward — the transparent legacy proxy path (HandleMCPProxy) — never owns a
// session of its own (the caller's request IS the caller's own handshake)
// and does not take a SessionScope at all: it relays whatever
// Mcp-Session-Id, if any, it is given, exactly as given. The isolation a
// shared global server needs against one caller guessing or reusing another
// caller's session ID is enforced one layer up, by HandleMCPProxy itself,
// via a *SessionRegistry it holds independently of any one HTTPTransport
// (see SessionRegistry's own doc for why that independence matters).
// SessionRegistry's lookups are keyed by SessionScope too, but built one
// level finer than Call's — by (organization, API key) via
// NewClientSessionScope, not organization alone — since a caller-supplied
// session, unlike Call's own VoidLLM-established one, can be guessed or
// reused by any key in the organization, not just the one that received it.
//
// Pass the empty SessionScope for calls that have no caller organization to
// isolate — built-in tool discovery (ListTools) and health probes — so they
// get their own session, kept separate from every real org's.
type SessionScope string

// UpstreamState holds whatever per-connection state a ClientDialect's era
// needs across repeated calls to the same upstream server. One UpstreamState
// is created per SessionScope of one resolved eraBinding (see
// eraBinding.stateFor) — eraBinding itself is already cached per server ID
// by MCPTransportCache, and the scope on top of that keeps legacy session
// state from crossing between organizations that share one global server
// (docs/mcp-v2.md §11.3 Befund 1). Two Calls made with the same SessionScope
// against the same server legitimately share one UpstreamState; two Calls
// with different scopes never do.
//
// For EraModern it stays permanently zero: the 2026-07-28 revision carries
// no session concept, so dialect2026Client's Warmup performs no I/O and
// never touches it. For EraLegacy it holds the Mcp-Session-Id minted by the
// initialize handshake.
type UpstreamState struct {
	mu        sync.Mutex
	sessionID string
}

// SessionID returns the currently known legacy session ID, or "" if none has
// been established yet (or the dialect is EraModern, which never sets one).
func (s *UpstreamState) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// SetSessionID records the legacy session ID most recently returned by the
// upstream server.
func (s *UpstreamState) SetSessionID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionID = id
}

// ClearSessionID discards the current session ID, forcing the next Warmup to
// re-establish one. Called after the upstream reports the session expired.
func (s *UpstreamState) ClearSessionID() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionID = ""
}

// ClientDialect renders outbound MCP requests for exactly one protocol era
// and parses the responses. It is the client-side mirror of ServerDialect:
// where a ServerDialect turns inbound wire bytes into an Envelope and a
// Result back into wire bytes, a ClientDialect turns an already-built
// outbound JSON-RPC request into wire bytes plus transport headers, and
// turns the response back into a Result.
type ClientDialect interface {
	// Version returns the protocol revision this dialect speaks.
	Version() Version
	// Warmup performs whatever one-time, per-connection handshake this era
	// requires before the first real request can be sent, storing any
	// resulting state in st. EraModern dialects perform no I/O and return
	// nil immediately — the defining property of the 2026-07-28 revision's
	// stateless core (docs/mcp-v2.md §2, §3.2). EraLegacy dialects send
	// initialize followed by notifications/initialized over rt and record
	// the resulting session ID, if any, in st.
	Warmup(ctx context.Context, rt RoundTripper, st *UpstreamState) error
	// Prepare takes req — the era-neutral form of an already-serialized
	// outbound JSON-RPC request or notification — and returns the bytes to
	// actually send on the wire together with the transport headers to set.
	// EraLegacy dialects return req.Raw unchanged and set only
	// Mcp-Session-Id, when st holds one. EraModern dialects inject
	// params._meta (protocol version, client capabilities, client info) into
	// req.Raw and return the standard request headers (MCP-Protocol-Version,
	// Mcp-Method, and — for tools/call, resources/read, prompts/get —
	// Mcp-Name), applying the base64 sentinel encoding from
	// EncodeHeaderValue where needed.
	//
	// The header return type is the concrete MapHeader, not the Header
	// interface ServerDialect.Decode reads from: a caller applying these
	// headers to an outbound *http.Request needs to enumerate them, which
	// Header (Get-only, mirroring net/http's read side) cannot do.
	Prepare(req *CallRequest, st *UpstreamState) ([]byte, MapHeader, error)
	// Parse decodes a raw JSON-RPC response. It returns a non-nil *Error
	// only when the response cannot be safely forwarded to the caller as-is:
	// malformed JSON, or — EraModern only — a resultType of
	// "input_required", which requires Multi Round-Trip Request support
	// VoidLLM's Bridge does not implement yet (docs/mcp-v2.md §3.7; Bridge
	// and MRTR translation are explicitly out of scope for this phase). An
	// ordinary JSON-RPC error object on the wire is NOT such a case — it is
	// valid MCP content a caller must see, not a transport failure — so
	// Parse never synthesizes an error from one; it is reported inside the
	// returned Result exactly like an ordinary success payload, which
	// HTTPTransport.Call therefore forwards unchanged either way.
	Parse(raw []byte) (*Result, *Error)
}
