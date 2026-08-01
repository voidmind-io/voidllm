package mcp

import "github.com/voidmind-io/voidllm/internal/jsonx"

// legacyDialect implements ServerDialect for the MCP eras that negotiate via
// an initialize handshake (V20250326 through V20251125). It carries no
// per-request metadata: identity and capabilities, when present at all,
// arrive once in the initialize request and are not repeated afterward.
type legacyDialect struct {
	version Version
}

// newLegacyDialect returns a ServerDialect for the given legacy version.
// Callers (Negotiate and Server.dialectFor) are responsible for only passing
// a version whose Era is EraLegacy.
func newLegacyDialect(v Version) ServerDialect {
	return &legacyDialect{version: v}
}

// Version implements ServerDialect.
func (d *legacyDialect) Version() Version {
	return d.version
}

// Decode implements ServerDialect. hdr is unused: legacy requests carry no
// per-request routing headers — method and tool name are read from the body
// alone, and no params._meta is consulted.
func (d *legacyDialect) Decode(raw []byte, _ Header) (*Envelope, *Error) {
	var req Request
	if err := jsonx.Unmarshal(raw, &req); err != nil {
		return nil, &Error{Code: CodeParseError, Message: "parse error"}
	}
	if req.JSONRPC != "2.0" {
		return nil, &Error{Code: CodeInvalidRequest, Message: `jsonrpc must be "2.0"`}
	}

	env := &Envelope{
		ID:             req.ID,
		Method:         req.Method,
		Params:         req.Params,
		Version:        d.version,
		IsNotification: req.IsNotification(),
	}

	switch req.Method {
	case "initialize":
		var init struct {
			ProtocolVersion string           `json:"protocolVersion"`
			ClientInfo      ClientInfo       `json:"clientInfo"`
			Capabilities    jsonx.RawMessage `json:"capabilities"`
		}
		// A malformed or absent initialize body still yields a usable
		// Envelope: the dialect's negotiated version and empty capabilities
		// are used as fallbacks rather than rejecting the request.
		_ = jsonx.Unmarshal(req.Params, &init)
		// The client's requested protocolVersion is accepted only when it
		// names a version in the SAME era this dialect was negotiated for —
		// it may refine env.Version within EraLegacy (e.g. from V20250326 to
		// V20250618), but it can never switch the request to EraModern. Era
		// is fixed by Negotiate at the edge; a body value cannot override it
		// (see the warning on Envelope.Version in dialect.go).
		if v := Version(init.ProtocolVersion); v.Valid() && v.Era() == d.version.Era() {
			env.Version = v
		}
		env.ClientInfo = init.ClientInfo
		env.ClientCaps = Capabilities(init.Capabilities)
	}

	// Populated for every method TargetParamKey names — not only tools/call —
	// so a future dispatch of resources/read or prompts/get inherits a
	// correctly populated Envelope.Name instead of silently getting an empty
	// string (docs/mcp-v2.md Fund 5), symmetrically with dialect_2026.go's
	// Decode. See envelopeTargetName's own doc.
	env.Name = envelopeTargetName(req.Method, req.Params)

	return env, nil
}

// EncodeResult implements ServerDialect. Legacy results carry the payload
// verbatim under "result" — no resultType, ttlMs, or cacheScope wrapper.
// r.Cache is ignored: legacy revisions predate CacheableResult and have no
// on-wire representation for it. r must not be nil — see errNilResult.
func (d *legacyDialect) EncodeResult(id jsonx.RawMessage, r *Result) ([]byte, error) {
	if r == nil {
		return nil, errNilResult
	}
	return jsonx.Marshal(NewResponse(id, r.Payload))
}

// EncodeError implements ServerDialect.
func (d *legacyDialect) EncodeError(id jsonx.RawMessage, e *Error) ([]byte, error) {
	return jsonx.Marshal(Response{JSONRPC: "2.0", ID: id, Error: e})
}
