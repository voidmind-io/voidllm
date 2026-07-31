package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// defaultResultTTLMs is the default freshness window, in milliseconds, for
// CacheHints on cacheable results (server/discover, tools/list) when no
// value has been set via SetResultTTLMs.
const defaultResultTTLMs int64 = 60000

// ToolHandler is a function that handles a tool call.
// It receives the request context and the raw JSON arguments from the caller.
// Return a ToolResult on success, or an error for unexpected failures.
// Tool-level errors (e.g. invalid input) should be returned as ErrorResult,
// not as a Go error.
type ToolHandler func(ctx context.Context, args jsonx.RawMessage) (*ToolResult, error)

// OnToolsListHook is an optional callback invoked inside tools/list before the
// tool list is returned to the caller. It receives a copy of the registered
// tools and may return a modified slice. The hook must not retain references to
// the slice after it returns.
type OnToolsListHook func(tools []Tool) []Tool

// Server is an MCP server that handles JSON-RPC 2.0 requests across both the
// legacy (initialize-handshake) and modern (per-request metadata) protocol
// eras on the same endpoint. It is safe for concurrent use.
type Server struct {
	name        string
	version     string
	mu          sync.RWMutex
	tools       []Tool
	handlers    map[string]registeredTool
	onToolsList OnToolsListHook
	resultTTLMs int64
}

// NewServer creates a new MCP server with the given name and version.
func NewServer(name, version string) *Server {
	return &Server{
		name:        name,
		version:     version,
		handlers:    make(map[string]registeredTool),
		resultTTLMs: defaultResultTTLMs,
	}
}

// SetResultTTLMs sets the freshness window, in milliseconds, reported in the
// CacheHint of cacheable results (server/discover, tools/list). It is safe
// to call concurrently with Handle.
func (s *Server) SetResultTTLMs(ms int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resultTTLMs = ms
}

// RegisterTool adds a tool and its handler to the server. opts declare
// server-side registration policy — currently only RequireExtensions — that
// is enforced before handler runs, without appearing in the wire-visible
// Tool schema itself.
// It is not safe to call concurrently with Handle — register all tools
// before starting to handle requests.
func (s *Server) RegisterTool(tool Tool, handler ToolHandler, opts ...ToolOption) {
	rt := registeredTool{handler: handler}
	for _, opt := range opts {
		opt(&rt)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = append(s.tools, tool)
	s.handlers[tool.Name] = rt
}

// Tools returns a deep copy of the registered tool schemas. The returned
// slice — including every Tool's InputSchema bytes — is safe to use and
// mutate after the call; mutations do not affect the server's internal
// state. It is safe to call concurrently with Handle and RegisterTool.
func (s *Server) Tools() []Tool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Tool, len(s.tools))
	for i, t := range s.tools {
		out[i] = t.clone()
	}
	return out
}

// SetOnToolsList registers a hook that is called inside tools/list before the
// tool list is returned. The hook receives a deep copy of the registered
// tools — including each Tool's InputSchema bytes, see Tool.clone — and may
// return a modified slice. Setting a nil hook clears any previously
// registered hook. It is safe to call concurrently with Handle.
func (s *Server) SetOnToolsList(hook OnToolsListHook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onToolsList = hook
}

// StatusHint classifies a HandleResult for HTTP-aware callers, without
// internal/mcp itself importing net/http or any HTTP framework — this
// package is standalone (see the package doc in protocol.go). Callers map a
// StatusHint to whatever status code makes sense for their own transport;
// internal/mcp defines no HTTP status constants.
type StatusHint uint8

const (
	// HintOK covers a JSON-RPC success response and every JSON-RPC error
	// other than the ones the hints below single out. HTTP callers should
	// respond 200 OK: JSON-RPC-over-HTTP convention treats a JSON-RPC-level
	// error as protocol content, not a transport-level failure.
	HintOK StatusHint = iota
	// HintNotification indicates the request was a notification: Body is
	// nil and HTTP callers should respond 202 Accepted with no body. This is
	// the ONLY case in which Body is nil; an internal encoding failure never
	// produces this hint (or a nil Body) — see the fallback in Handle.
	HintNotification
	// HintBadRequest indicates CodeUnsupportedProtocolVersion,
	// CodeHeaderMismatch, or CodeMissingRequiredClientCapability. HTTP
	// callers should respond 400 Bad Request (MCP Streamable HTTP §4.5,
	// §4.6; docs/mcp-v2.md §4.6): dual-era clients probing a server rely on
	// this status to recognize a modern error body and retry with an
	// announced version instead of falling back to a legacy handshake.
	HintBadRequest
	// HintMethodNotFound indicates CodeMethodNotFound. HTTP callers should
	// respond 404 Not Found ONLY when Era is EraModern (MCP Streamable HTTP
	// §4.6). For EraLegacy, Handle never produces this hint — legacy
	// CodeMethodNotFound responses always carry HintOK (200) instead: legacy
	// clients have no fallback path for a non-200 status on an endpoint they
	// already know exists, and would misinterpret 404 as "wrong URL",
	// falling back to the deprecated HTTP+SSE transport.
	HintMethodNotFound
)

// HandleResult is the outcome of a single Handle call: the encoded response
// bytes plus enough information for an HTTP-aware caller to choose a status
// code, without internal/mcp depending on any HTTP framework.
type HandleResult struct {
	// Body is the encoded response bytes. Nil only when Hint is
	// HintNotification — never as a signal of an internal encoding failure;
	// see Handle's fallback for that case.
	Body []byte
	// Hint classifies Body for HTTP-aware callers. See StatusHint's values.
	Hint StatusHint
	// Era is the protocol era Body was encoded in. Only meaningful — and
	// only consulted by callers — when Hint is HintMethodNotFound, to decide
	// between 404 (modern) and 200 (legacy). Zero value is EraLegacy.
	Era Era
}

// Handle processes a raw JSON-RPC 2.0 request or notification and returns
// the outcome encoded in whichever protocol era the request negotiated to
// (see Negotiate).
//
// hdr provides the inbound transport headers used only for era negotiation;
// internal/mcp has no dependency on any specific HTTP framework (see the
// package doc in protocol.go). Callers with no real transport headers (e.g.
// in-process tool calls) may pass an empty MapHeader.
//
// The protocol version is resolved exactly once, at the top of Handle, via
// Negotiate. Everything after that — dispatch, tool handlers, access
// control, usage logging — operates on the era-neutral Envelope and Result
// types and never branches on protocol version again.
func (s *Server) Handle(ctx context.Context, raw []byte, hdr Header) HandleResult {
	version, negErr := Negotiate(raw, hdr)
	if negErr != nil {
		// negErr is always CodeUnsupportedProtocolVersion — the only error
		// Negotiate ever returns — which always maps to HintBadRequest
		// regardless of era; no dialect has been resolved yet to ask.
		id := peekID(raw)
		out, err := jsonx.Marshal(Response{JSONRPC: "2.0", ID: id, Error: negErr})
		if err != nil {
			logEncodeFailure(ctx, err, "")
			out = encodeFallback(id)
		}
		return HandleResult{Body: out, Hint: HintBadRequest}
	}

	dialect := s.dialectFor(version)
	era := dialect.Version().Era()

	env, decErr := dialect.Decode(raw, hdr)
	if decErr != nil {
		id := peekID(raw)
		out, err := dialect.EncodeError(id, decErr)
		if err != nil {
			logEncodeFailure(ctx, err, "")
			out = encodeFallback(id)
		}
		return HandleResult{Body: out, Hint: statusHintFor(decErr, era), Era: era}
	}

	result, dispatchErr := s.dispatch(ctx, dialect, env)

	if env.IsNotification {
		return HandleResult{Body: nil, Hint: HintNotification, Era: era}
	}

	if dispatchErr != nil {
		out, err := dialect.EncodeError(env.ID, dispatchErr)
		if err != nil {
			logEncodeFailure(ctx, err, env.Method)
			out = encodeFallback(env.ID)
		}
		return HandleResult{Body: out, Hint: statusHintFor(dispatchErr, era), Era: era}
	}

	out, err := dialect.EncodeResult(env.ID, result)
	if err != nil {
		logEncodeFailure(ctx, err, env.Method)
		out = encodeFallback(env.ID)
	}
	return HandleResult{Body: out, Hint: HintOK, Era: era}
}

// hintForError maps a JSON-RPC error code to the StatusHint an HTTP-aware
// caller should use, per MCP Streamable HTTP §4.5/§4.6 (docs/mcp-v2.md
// §4.5, §4.6). era distinguishes CodeMethodNotFound: only the modern era
// gets a distinct hint, since a legacy CodeMethodNotFound response MUST stay
// HTTP 200 — see HintMethodNotFound's doc.
func hintForError(code int, era Era) StatusHint {
	switch code {
	case CodeUnsupportedProtocolVersion, CodeHeaderMismatch, CodeMissingRequiredClientCapability:
		return HintBadRequest
	case CodeMethodNotFound:
		if era == EraModern {
			return HintMethodNotFound
		}
	}
	return HintOK
}

// statusHintFor returns the StatusHint Handle should use for e: e.Hint when
// the producer (a ServerDialect's Decode, in particular) set one explicitly,
// or hintForError's derivation from e.Code and era otherwise. This is what
// lets a single error code — CodeInvalidParams, ordinarily HintOK per
// hintForError — carry HTTP 400 for the one violation MCP 2026-07-28
// requires it for (a request missing a required params._meta field;
// docs/mcp-v2.md §3.2) while every other CodeInvalidParams error keeps the
// HTTP 200 JSON-RPC-error convention.
func statusHintFor(e *Error, era Era) StatusHint {
	if e.Hint != HintOK {
		return e.Hint
	}
	return hintForError(e.Code, era)
}

// logEncodeFailure logs a ServerDialect encoding failure via the process
// default logger (see cmd/voidllm/main.go's slog.SetDefault) with only the
// error cause and the JSON-RPC method name — never body content, per
// VoidLLM's zero-knowledge logging rule (no prompt, argument, or _meta
// content ever appears in a VoidLLM log). method is the empty string when
// Handle has not yet decoded far enough to know it (a Negotiate or Decode
// failure).
func logEncodeFailure(ctx context.Context, err error, method string) {
	slog.Default().LogAttrs(ctx, slog.LevelError, "mcp: failed to encode response",
		slog.String("error", err.Error()),
		slog.String("method", method),
	)
}

// encodeFallback renders a JSON-RPC CodeInternalError response by hand, with
// no call to jsonx.Marshal, so it can never itself fail to encode. It is
// Handle's last-resort fallback for the — in the current codebase,
// unreachable, since every value Handle asks a ServerDialect to encode is
// built from internally controlled, JSON-safe types — case where a dialect's
// EncodeResult or EncodeError fails to marshal its response. Without this
// fallback, Handle would return a nil Body under HintOK, indistinguishable
// from the HintNotification case where nil correctly means "no response is
// needed": an internal failure would be misreported to an HTTP-aware caller
// as a successful notification.
//
// id must already be valid JSON: every id Handle passes to encodeFallback is
// either verbatim jsonx.RawMessage decoded from the request body — which
// encoding/json validates as well-formed JSON while scanning a RawMessage
// value — or empty. An empty id is rendered as the JSON-RPC-legal null.
func encodeFallback(id jsonx.RawMessage) []byte {
	idLiteral := "null"
	if len(id) > 0 {
		idLiteral = string(id)
	}
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":"internal error"}}`,
		idLiteral, CodeInternalError))
}

// dialectFor returns the ServerDialect for v. Callers must ensure v.Valid()
// beforehand; Negotiate guarantees this for every Version it returns.
func (s *Server) dialectFor(v Version) ServerDialect {
	if v.Era() == EraModern {
		s.mu.RLock()
		name, version := s.name, s.version
		s.mu.RUnlock()
		return newDialect2026(v, name, version)
	}
	return newLegacyDialect(v)
}

// peekID extracts the JSON-RPC "id" field from a raw request without fully
// decoding it, for use in error responses produced before or during
// decoding. Returns nil if raw is not parseable as an object with an "id"
// field — the resulting error response then correctly carries a null ID, per
// JSON-RPC 2.0.
func peekID(raw []byte) jsonx.RawMessage {
	var probe struct {
		ID jsonx.RawMessage `json:"id"`
	}
	if err := jsonx.Unmarshal(raw, &probe); err != nil {
		return nil
	}
	return probe.ID
}

// dispatch routes env to its handler. Methods common to both eras
// (tools/list, tools/call) are handled first; era-specific methods are then
// routed by dialect.Version().Era() — the era chosen once, at the edge, by
// Negotiate — NEVER by env.Version. env.Version is body-derived metadata a
// caller controls and must never steer control flow; see the warning on
// Envelope.Version in dialect.go. A method belonging to the other era's
// vocabulary — e.g. "initialize" carried by a modern-era dialect, or
// "server/discover" carried by a legacy-era one — falls through to
// CodeMethodNotFound in the era-specific dispatcher.
func (s *Server) dispatch(ctx context.Context, dialect ServerDialect, env *Envelope) (*Result, *Error) {
	switch env.Method {
	case "tools/list":
		return s.handleToolsList(), nil
	case "tools/call":
		payload, err := s.handleToolsCall(ctx, env)
		if err != nil {
			return nil, err
		}
		return &Result{Payload: payload}, nil
	}

	if dialect.Version().Era() == EraModern {
		return s.dispatchModern(env)
	}
	return s.dispatchLegacy(env)
}

// dispatchLegacy routes the legacy-era methods: initialize,
// notifications/initialized, and ping.
func (s *Server) dispatchLegacy(env *Envelope) (*Result, *Error) {
	switch env.Method {
	case "initialize":
		return &Result{Payload: s.handleInitialize(env)}, nil
	case "notifications/initialized":
		// Notification — no response required by the MCP spec. When the
		// request carries no ID (the common, spec-compliant case),
		// env.IsNotification is true and Handle returns HintNotification
		// before this Result is ever encoded — see the check right after
		// dispatch in Handle. A client that mistakenly attaches an ID to
		// this notification, though, DOES reach EncodeResult with it: an
		// empty-but-non-nil Result keeps that response JSON-RPC-valid
		// (result: {}) instead of encoding neither "result" nor "error".
		// See also EncodeResult's own defensive rejection of a nil Result.
		return &Result{Payload: map[string]any{}}, nil
	case "ping":
		return &Result{Payload: map[string]any{}}, nil
	default:
		return nil, &Error{Code: CodeMethodNotFound, Message: fmt.Sprintf("method not found: %s", env.Method)}
	}
}

// dispatchModern routes the modern-era methods: server/discover and
// subscriptions/listen.
func (s *Server) dispatchModern(env *Envelope) (*Result, *Error) {
	switch env.Method {
	case "server/discover":
		return s.handleDiscover(), nil
	case "subscriptions/listen":
		// Phase 1 does not hold response streams open: Handle returns a
		// single, immediately-final []byte per call, and a spec-faithful
		// subscriptions/listen requires delivering
		// notifications/subscriptions/acknowledged as the first message on a
		// stream that then stays open for further notifications. Reporting
		// MethodNotFound here is an honest "not yet supported" rather than
		// acknowledging a subscription this server can never deliver
		// notifications on or gracefully close — see the Phase 1 report.
		// Real support is Phase 4 (streaming passthrough) work.
		return nil, &Error{Code: CodeMethodNotFound, Message: "method not found: subscriptions/listen (streaming not yet supported)"}
	default:
		return nil, &Error{Code: CodeMethodNotFound, Message: fmt.Sprintf("method not found: %s", env.Method)}
	}
}

// handleInitialize returns the server's capabilities and identity for the
// legacy initialize handshake, mirroring back the protocol version env
// carries — already validated and defaulted to the negotiated version by the
// legacy dialect's Decode step when the client's requested version is absent
// or unrecognized.
func (s *Server) handleInitialize(env *Envelope) any {
	s.mu.RLock()
	name, version := s.name, s.version
	s.mu.RUnlock()

	return map[string]any{
		"protocolVersion": string(env.Version),
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    name,
			"version": version,
		},
	}
}

// handleDiscover implements the modern-era server/discover RPC (MUST per
// SEP-2575). It reports every revision VoidLLM understands so a dual-era
// client can pick one without probing, and is itself cacheable with public
// scope since its content depends only on server configuration, never on
// caller identity.
func (s *Server) handleDiscover() *Result {
	s.mu.RLock()
	name, ttl := s.name, s.resultTTLMs
	s.mu.RUnlock()

	supported := SupportedVersions()
	versions := make([]string, len(supported))
	for i, v := range supported {
		versions[i] = string(v)
	}

	payload := map[string]any{
		"supportedVersions": versions,
		"capabilities": map[string]any{
			"tools":      map[string]any{},
			"extensions": map[string]any{},
		},
		"instructions": fmt.Sprintf("%s MCP server. Call tools/list to discover available tools.", name),
	}
	return &Result{
		Payload: payload,
		Cache:   CacheHint{TTLMs: ttl, Scope: CacheScopePublic},
	}
}

// handleToolsList returns the list of registered tools, sorted
// deterministically by name (SHOULD, per the 2026-07-28 minor changes —
// deterministic ordering improves LLM prompt-cache hits). If an
// OnToolsListHook has been set via SetOnToolsList, the hook is invoked with a
// copy of the tool list and its return value is used as the response
// payload.
//
// Because the hook filters by caller role, the result is scoped
// CacheScopePrivate under CacheableResult: even though the tool schemas
// themselves are caller-agnostic, which subset of them a given caller may
// see is not, and a public cache would leak an admin-only tool's existence
// to every caller sharing the cache.
func (s *Server) handleToolsList() *Result {
	s.mu.RLock()
	tools := make([]Tool, len(s.tools))
	for i, t := range s.tools {
		tools[i] = t.clone()
	}
	hook := s.onToolsList
	ttl := s.resultTTLMs
	s.mu.RUnlock()

	if hook != nil {
		tools = hook(tools)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	return &Result{
		Payload: map[string]any{"tools": tools},
		Cache:   CacheHint{TTLMs: ttl, Scope: CacheScopePrivate},
	}
}

// handleToolsCall dispatches a tools/call request to the registered handler.
// It takes the whole Envelope, not just its Params, because enforcing a
// tool's declared RequireExtensions needs env.ClientCaps — the caller's
// declared capabilities — which Params alone does not carry. Unexpected
// handler errors are converted to tool-level error results rather than
// JSON-RPC protocol errors, keeping protocol integrity intact.
func (s *Server) handleToolsCall(ctx context.Context, env *Envelope) (any, *Error) {
	var call struct {
		Name      string           `json:"name"`
		Arguments jsonx.RawMessage `json:"arguments"`
	}
	if err := jsonx.Unmarshal(env.Params, &call); err != nil {
		return nil, &Error{Code: CodeInvalidParams, Message: "invalid params: expected {name, arguments}"}
	}

	s.mu.RLock()
	rt, ok := s.handlers[call.Name]
	s.mu.RUnlock()

	if !ok {
		return nil, &Error{Code: CodeInvalidParams, Message: fmt.Sprintf("unknown tool: %s", call.Name)}
	}

	if missing := missingExtensions(rt.requires, env.ClientCaps); len(missing) > 0 {
		return nil, &Error{
			Code:    CodeMissingRequiredClientCapability,
			Message: "missing required client capability",
			Data:    MissingCapabilityData{RequiredCapabilities: missing},
		}
	}

	result, err := rt.handler(ctx, call.Arguments)
	if err != nil {
		// Unexpected handler error → tool-level error result, NOT a protocol error.
		// Internal error details are not forwarded to the caller.
		return ErrorResult("internal error"), nil
	}

	return result, nil
}
