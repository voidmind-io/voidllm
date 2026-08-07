package mcp

import (
	"bytes"
	"strings"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// Meta key names the 2026-07-28 revision defines for per-request identity
// and capability metadata (carried in params._meta) and for server
// self-identification (carried in every result's _meta).
const (
	metaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	metaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	metaClientInfo         = "io.modelcontextprotocol/clientInfo"
	metaLogLevel           = "io.modelcontextprotocol/logLevel"
	metaServerInfo         = "io.modelcontextprotocol/serverInfo"
)

// dialect2026 implements ServerDialect for V20260728, which carries protocol
// version, identity, and capabilities as per-request metadata instead of an
// initialize handshake, and requires every result to self-identify its
// server and (for cacheable methods) carry a CacheableResult hint.
type dialect2026 struct {
	version       Version
	serverName    string
	serverVersion string
}

// newDialect2026 returns a ServerDialect for the given modern version,
// self-identifying as serverName/serverVersion in every result's
// _meta["io.modelcontextprotocol/serverInfo"]. Callers (Server.dialectFor)
// are responsible for only passing a version whose Era is EraModern.
func newDialect2026(v Version, serverName, serverVersion string) ServerDialect {
	return &dialect2026{version: v, serverName: serverName, serverVersion: serverVersion}
}

// Version implements ServerDialect.
func (d *dialect2026) Version() Version {
	return d.version
}

// Decode implements ServerDialect. hdr is unused: MCP-Protocol-Version,
// Mcp-Method, and Mcp-Name are validated against the body by the caller
// (internal/api/admin/mcp_handler.go) before Decode ever runs, and Decode
// itself reads identity and capabilities from params._meta as the spec
// requires, not from headers.
//
// protocolVersion and clientCapabilities are MUST fields (docs/mcp-v2.md
// §3.2). A request whose params._meta omits either one is malformed and is
// rejected with CodeInvalidParams — its Error.Hint is explicitly
// HintBadRequest, so Server.Handle reports it as HTTP 400 regardless of
// hintForError's ordinary (HTTP 200) mapping for CodeInvalidParams, per the
// spec's verbatim requirement quoted in docs/mcp-v2.md §3.2:
//
//	A request missing any required field is malformed; the server MUST
//	reject it with JSON-RPC error code -32602 (Invalid params). On HTTP,
//	the response status MUST be 400 Bad Request.
//
// Filling in a default instead — an empty {} for a missing
// clientCapabilities, for instance — is deliberately NOT done: a fabricated
// empty object would claim a capability declaration the client never sent,
// making "the client declared no extensions" indistinguishable from "the
// client did not declare anything at all", which is exactly the ambiguity
// RequireExtensions's -32021 check (server.go) depends on being able to
// tell apart.
//
// This is NOT a compatibility break for legacy clients. Negotiate's rule 4
// (negotiate.go) defaults every request that carries neither a modern
// MCP-Protocol-Version header nor an "initialize" body to V20250326 — a
// legacy version — so a request never reaches this dialect's Decode at all
// unless it (or its MCP-Protocol-Version header) explicitly claimed a modern
// revision in the first place. Only a caller that already claims
// "2026-07-28" and then omits a MUST field of its own claimed revision is
// affected.
func (d *dialect2026) Decode(raw []byte, _ Header) (*Envelope, *Error) {
	var req Request
	if err := jsonx.Unmarshal(raw, &req); err != nil {
		return nil, &Error{Code: CodeParseError, Message: "parse error"}
	}
	if req.JSONRPC != "2.0" {
		return nil, &Error{Code: CodeInvalidRequest, Message: `jsonrpc must be "2.0"`}
	}

	var params struct {
		Meta map[string]jsonx.RawMessage `json:"_meta"`
	}
	// A single pass unmarshals req.Params into topParams once and derives all
	// three distinctions below from it, instead of three separate full
	// unmarshals of req.Params (isJSONObject, then paramsMetaIsExplicitNull,
	// then this same shape decoded a third time into params) that a previous
	// version of this function performed — each of which re-parsed the
	// entire body, including a large tools/call "arguments" payload that has
	// nothing to do with _meta at all (docs/mcp-v2.md, FIX 5):
	//
	//   - Unmarshal fails, or succeeds with a nil map (the JSON literal
	//     null unmarshals into a nil map with no error — the same pitfall
	//     isJSONObject used to guard against) → req.Params is not a JSON
	//     object at all → CodeInvalidParams, HintBadRequest, below.
	//   - The "_meta" key is absent from topParams → certainly no _meta —
	//     tolerated here, checked (for the two MUST fields) immediately
	//     below.
	//   - The "_meta" key is present and its raw value is the JSON literal
	//     null → rejected the same as any other wrongly-typed _meta, NOT
	//     tolerated as if the key were simply absent: both shapes would
	//     otherwise unmarshal to the same nil params.Meta map, so this
	//     explicit check is what tells "missing" and "present but null"
	//     apart.
	//   - Otherwise "_meta"'s raw value is unmarshaled into params.Meta — the
	//     only unmarshal here that ever runs against something other than
	//     the full req.Params, and it is cheap: only the _meta sub-object's
	//     own bytes, never the surrounding arguments.
	//
	// Every shape that leaves params.Meta nil, or lacking one of the two MUST
	// keys, is rejected by the presence check that follows this switch — see
	// missingRequiredMetaError. A single _meta FIELD that IS present but
	// wrongly typed (including the explicit-null case above) is a further,
	// separate case: rejected with an ordinary (HTTP 200) CodeInvalidParams
	// via metaTypeError instead, since a caller that got the shape of its
	// own request wrong is a different failure mode than one that omitted a
	// required field outright.
	switch {
	case len(req.Params) == 0:
		// No params at all, so certainly no _meta — checked immediately
		// below.
	default:
		var topParams map[string]jsonx.RawMessage
		if err := jsonx.Unmarshal(req.Params, &topParams); err != nil || topParams == nil {
			return nil, &Error{Code: CodeInvalidParams, Message: "invalid params: expected an object", Hint: HintBadRequest}
		}
		metaRaw, hasMeta := topParams["_meta"]
		switch {
		case !hasMeta:
			// No _meta key at all — tolerated, checked immediately below.
		case bytes.Equal(bytes.TrimSpace(metaRaw), []byte("null")):
			return nil, metaTypeError("_meta")
		default:
			if err := jsonx.Unmarshal(metaRaw, &params.Meta); err != nil {
				// The only way this Unmarshal can fail once metaRaw is known
				// to be neither absent nor the JSON literal null is a _meta
				// value of a type map[string]jsonx.RawMessage cannot hold
				// (e.g. "_meta": "not an object" or "_meta": [1,2,3]).
				return nil, metaTypeError("_meta")
			}
		}
	}

	protocolVersionRaw, hasProtocolVersion := params.Meta[metaProtocolVersion]
	clientCapsRaw, hasClientCaps := params.Meta[metaClientCapabilities]
	switch {
	case !hasProtocolVersion && !hasClientCaps:
		return nil, missingRequiredMetaError(metaProtocolVersion, metaClientCapabilities)
	case !hasProtocolVersion:
		return nil, missingRequiredMetaError(metaProtocolVersion)
	case !hasClientCaps:
		return nil, missingRequiredMetaError(metaClientCapabilities)
	}

	env := &Envelope{
		ID:             req.ID,
		Method:         req.Method,
		Params:         req.Params,
		Version:        d.version,
		IsNotification: req.IsNotification(),
	}

	{
		var s string
		if err := jsonx.Unmarshal(protocolVersionRaw, &s); err != nil {
			return nil, metaTypeError(metaProtocolVersion)
		}
		// An unrecognized (but well-typed) version string is not itself an
		// error: it is accepted only within EraModern — see the matching
		// guard in dialect_legacy.go's Decode and the warning on
		// Envelope.Version in dialect.go — and otherwise silently falls back
		// to d.version, exactly as an absent field would.
		if v := Version(s); v.Valid() && v.Era() == d.version.Era() {
			env.Version = v
		}
	}
	{
		if !isJSONObject(clientCapsRaw) {
			return nil, metaTypeError(metaClientCapabilities)
		}
		// A genuine copy, not just a type conversion: clientCapsRaw aliases
		// req's backing array (itself a slice of the caller-supplied body
		// bytes). Capabilities(clientCapsRaw) would compile — []byte-alias
		// types convert for free — but would leave env.ClientCaps sharing
		// that same backing array, so Decode's safety would depend entirely
		// on every caller having already copied the body before calling it
		// (which mcp_handler.go happens to do today, but Decode's own
		// contract should not rely on that).
		clientCaps := make(Capabilities, len(clientCapsRaw))
		copy(clientCaps, clientCapsRaw)
		env.ClientCaps = clientCaps
	}
	if raw, ok := params.Meta[metaClientInfo]; ok {
		var ci ClientInfo
		if err := jsonx.Unmarshal(raw, &ci); err != nil {
			return nil, metaTypeError(metaClientInfo)
		}
		env.ClientInfo = ci
	}
	if raw, ok := params.Meta[metaLogLevel]; ok {
		var s string
		if err := jsonx.Unmarshal(raw, &s); err != nil {
			return nil, metaTypeError(metaLogLevel)
		}
		env.LogLevel = s
	}

	// Populated for every method TargetParamKey names — not only tools/call —
	// so a future dispatch of resources/read or prompts/get inherits a
	// correctly populated Envelope.Name instead of silently getting an empty
	// string (docs/mcp-v2.md Fund 5). See envelopeTargetName's own doc.
	env.Name = envelopeTargetName(req.Method, req.Params)

	return env, nil
}

// EncodeResult implements ServerDialect. Every result carries
// resultType: "complete" and self-identifies the server under
// _meta["io.modelcontextprotocol/serverInfo"] (SHOULD, per the handshake's
// removal in favor of per-request/per-result identity). When r.Cache carries
// a non-empty Scope — set by the Server for the methods it treats as
// cacheable — ttlMs and cacheScope are added alongside resultType. r must
// not be nil — see errNilResult.
//
// resultType, ttlMs, cacheScope, and _meta are all set AFTER r.Payload's
// fields are merged in, not before: a handler-supplied payload merged in
// first would let a payload field of the same name silently win over one of
// these wrapper keys, corrupting the wire format for every modern client
// (docs/mcp-v2.md, review finding D). No handler sets a "resultType" payload
// field today, so this was not yet reachable — this ordering is what keeps
// it that way for any handler written in the future, exactly as it already
// did for ttlMs, cacheScope, and _meta.
func (d *dialect2026) EncodeResult(id jsonx.RawMessage, r *Result) ([]byte, error) {
	if r == nil {
		return nil, errNilResult
	}

	out := map[string]any{}

	for k, v := range payloadAsMap(r.Payload) {
		out[k] = v
	}

	out["resultType"] = "complete"
	if r.Cache.Scope != "" {
		ttl := r.Cache.TTLMs
		if ttl < 0 {
			ttl = 0
		}
		out["ttlMs"] = ttl
		out["cacheScope"] = r.Cache.Scope
	}

	out["_meta"] = map[string]any{
		metaServerInfo: map[string]any{
			"name":    d.serverName,
			"version": d.serverVersion,
		},
	}

	return jsonx.Marshal(NewResponse(id, out))
}

// EncodeError implements ServerDialect. The JSON-RPC error envelope is
// identical across eras; only successful results changed shape in
// V20260728.
func (d *dialect2026) EncodeError(id jsonx.RawMessage, e *Error) ([]byte, error) {
	return jsonx.Marshal(Response{JSONRPC: "2.0", ID: id, Error: e})
}

// metaTypeError returns a CodeInvalidParams error naming the malformed
// params._meta key — but never its value. VoidLLM's zero-knowledge logging
// rule extends to error messages returned to the caller: a _meta value is
// caller-supplied content (it can carry, for example, elicitation or
// sampling payloads in later phases) and must never be echoed back, even in
// an error about its own shape.
func metaTypeError(key string) *Error {
	return &Error{Code: CodeInvalidParams, Message: "invalid _meta field: " + key}
}

// missingRequiredMetaError returns the CodeInvalidParams error MCP
// 2026-07-28 mandates for a request whose params._meta omits protocolVersion
// or clientCapabilities (docs/mcp-v2.md §3.2, quoted verbatim on Decode's
// doc). Unlike metaTypeError, its Hint is explicitly HintBadRequest: the
// spec requires HTTP 400 specifically for this violation, where an ordinary
// CodeInvalidParams error keeps the HTTP 200 JSON-RPC-error convention (see
// hintForError). keys names which _meta key(s) are missing — never their
// values, since values are caller-supplied content VoidLLM's zero-knowledge
// logging rule forbids echoing back (see metaTypeError).
func missingRequiredMetaError(keys ...string) *Error {
	return &Error{
		Code:    CodeInvalidParams,
		Message: "missing required _meta field(s): " + strings.Join(keys, ", "),
		Hint:    HintBadRequest,
	}
}

// isJSONObject reports whether raw is a well-formed JSON object ("{...}"),
// which is the type MCP Streamable HTTP requires for
// _meta["io.modelcontextprotocol/clientCapabilities"]. The JSON literal null
// is explicitly NOT an object and reports false, even though json.Unmarshal
// happily decodes null into a nil map without error — a caller-supplied null
// must be rejected the same as any other wrong type, not silently accepted
// as an (empty) object. Unmarshaling into a map is used rather than
// inspecting raw's first byte so that surrounding whitespace and JSON's own
// escaping rules are handled correctly. Decode applies the equivalent check
// to top-level params itself inline, since that case needs the decoded map
// for more than a single boolean (see Decode's doc on why it is not routed
// through this same helper).
func isJSONObject(raw jsonx.RawMessage) bool {
	var m map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &m); err != nil {
		return false
	}
	return m != nil
}

// payloadAsMap converts an arbitrary JSON-serializable result payload into a
// map[string]any so its fields can be merged with the resultType/_meta/ttlMs/
// cacheScope wrapper keys V20260728 adds around every result. Payloads that
// are not JSON objects at the top level — unexpected for any MCP result type
// VoidLLM produces — are placed under a "value" key rather than silently
// dropped.
func payloadAsMap(payload any) map[string]any {
	if payload == nil {
		return map[string]any{}
	}
	if m, ok := payload.(map[string]any); ok {
		return m
	}

	raw, err := jsonx.Marshal(payload)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := jsonx.Unmarshal(raw, &m); err != nil {
		return map[string]any{"value": jsonx.RawMessage(raw)}
	}
	return m
}
