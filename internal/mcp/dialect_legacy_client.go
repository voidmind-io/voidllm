package mcp

import (
	"context"
	"fmt"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// HeaderSessionID is the transport header legacy MCP servers use to convey
// the session established by initialize, and to accept it back on every
// subsequent request (removed entirely in 2026-07-28 — see docs/mcp-v2.md
// §3.1). It is exported so internal/api/admin's transparent proxy path
// (mcp_proxy.go's forwardHeaders, which mirrors a caller's own legacy session
// upstream unchanged — see Forward's doc in http_transport.go) can reuse the
// same constant this package's own client dialect and transport use
// internally, instead of maintaining a second definition of the same header
// name.
const HeaderSessionID = "Mcp-Session-Id"

// legacyClientDialect implements ClientDialect for the eras that negotiate
// via an initialize handshake (V20250326 through V20251125). See
// dialect_legacy.go for the inbound-facing counterpart.
type legacyClientDialect struct {
	version    Version
	clientInfo ClientInfo
}

// newLegacyClientDialect returns a ClientDialect for the given legacy
// version, self-identifying as clientInfo in the initialize handshake's
// clientInfo field. Callers are responsible for only passing a version whose
// Era is EraLegacy.
func newLegacyClientDialect(v Version, clientInfo ClientInfo) ClientDialect {
	return &legacyClientDialect{version: v, clientInfo: clientInfo}
}

// Version implements ClientDialect.
func (d *legacyClientDialect) Version() Version {
	return d.version
}

// Warmup implements ClientDialect. It sends initialize followed by
// notifications/initialized and records the session ID the upstream server
// returns for initialize, if any, in st. A server that returns no session ID
// at all is tolerated — some legacy servers behave statelessly in practice
// even though the era formally has a session concept — subsequent Prepare
// calls simply omit Mcp-Session-Id. Warmup DOES fail, however, when
// initialize itself failed: any HTTP status outside 2xx, a JSON-RPC error
// object in the response body (a legacy server that rejects initialize with
// a wire-level error still answers HTTP 200, per JSON-RPC convention), or a
// 2xx response whose body is not a well-formed JSON-RPC response carrying a
// "result" object — an empty body, an unparsable body, or a body with
// neither "result" nor "error" is treated exactly like an explicit error,
// since a 2xx status alone is not proof the handshake actually produced a
// usable result (see jsonRPCInitializeOutcome). Without these checks a
// failed initialize looked identical to a stateless success — no session
// header either way — so Call would never notice the upstream never
// actually warmed up. The notifications/initialized send remains
// fire-and-forget: its failure does not fail Warmup, matching the
// pre-Phase-2 behavior this replaces.
func (d *legacyClientDialect) Warmup(ctx context.Context, rt RoundTripper, st *UpstreamState) error {
	initReq, err := jsonx.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      "warmup-initialize",
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": string(d.version),
			"capabilities":    map[string]any{},
			"clientInfo":      d.clientInfo,
		},
	})
	if err != nil {
		return fmt.Errorf("legacy warmup: marshal initialize: %w", err)
	}

	res, err := rt.RoundTrip(ctx, initReq, nil)
	if err != nil {
		return fmt.Errorf("legacy warmup: initialize: %w", err)
	}
	if res.Status < 200 || res.Status >= 300 {
		return fmt.Errorf("legacy warmup: initialize: upstream returned HTTP %d", res.Status)
	}
	code, isErr, ok := jsonRPCInitializeOutcome(res.Body)
	if isErr {
		// The upstream's error message is free-form, upstream-controlled text
		// — never embed it here (docs/mcp-v2.md §11.2/§11.5). The numeric
		// JSON-RPC code is enough to diagnose from VoidLLM's side.
		return fmt.Errorf("legacy warmup: initialize: upstream returned HTTP %d with JSON-RPC error %d", res.Status, code)
	}
	if !ok {
		// 2xx with no explicit error, but also no well-formed "result" object
		// — an empty body, unparsable JSON, or neither field present. A 2xx
		// status alone is not proof the handshake succeeded.
		return fmt.Errorf("legacy warmup: initialize: upstream returned HTTP %d with no valid JSON-RPC result", res.Status)
	}

	var sessionID string
	if res.Header != nil {
		sessionID = res.Header.Get(HeaderSessionID)
	}
	if sessionID != "" {
		st.SetSessionID(sessionID)
	}

	notifyReq, err := jsonx.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	if err != nil {
		return fmt.Errorf("legacy warmup: marshal notifications/initialized: %w", err)
	}
	var notifyHdr MapHeader
	if sessionID != "" {
		notifyHdr = MapHeader{HeaderSessionID: sessionID}
	}
	_, _ = rt.RoundTrip(ctx, notifyReq, notifyHdr) // fire-and-forget: advisory only

	return nil
}

// jsonRPCInitializeOutcome classifies the body of a 2xx response to a legacy
// initialize request. A well-formed JSON-RPC response carrying a non-nil
// "error" object reports isErr == true with its numeric code. A well-formed
// response carrying a "result" object (present and non-null) reports
// ok == true. Any other body — empty, unparsable JSON, or valid JSON-RPC
// with neither "result" nor "error" present — reports both isErr and ok as
// false: Warmup treats that the same as an explicit error, since an HTTP 2xx
// status by itself is not proof the handshake actually produced a usable
// result (a blank or malformed 2xx body is exactly what a silently broken
// upstream would also return).
func jsonRPCInitializeOutcome(body []byte) (code int, isErr, ok bool) {
	var resp struct {
		Result *jsonx.RawMessage `json:"result"`
		Error  *Error            `json:"error"`
	}
	if err := jsonx.Unmarshal(body, &resp); err != nil {
		return 0, false, false
	}
	if resp.Error != nil {
		return resp.Error.Code, true, false
	}
	if resp.Result != nil {
		return 0, false, true
	}
	return 0, false, false
}

// Prepare implements ClientDialect. Legacy requests carry no per-request
// _meta: raw is returned unchanged. Two headers may be set:
//
//   - Mcp-Session-Id, when st holds a session established by Warmup.
//   - MCP-Protocol-Version, naming this dialect's negotiated version, but
//     only from V20250618 onward — the revision that introduced the header
//     in the first place (docs/mcp-v2.md, revision history table). V20250326
//     predates it entirely: the spec had no such header yet for that
//     revision to comply with, and sending one to a V20250326-only upstream
//     risks exactly the kind of header/body disagreement (docs/mcp-v2.md
//     §4.5) this header exists to prevent, not fix. V20250618 and V20251125
//     both formally require it on every request once the connection is
//     past initialize (docs/mcp-v2.md §4.2) — omitting it here (the prior
//     behavior, unconditionally) broke both of them (FIX 2).
//
// req.HeaderParams is deliberately NOT read here. Mcp-Param-{Name} headers
// (MCP 2026-07-28 §4.3) do not exist before the revision that introduced
// them — no era this dialect serves (V20250326 through V20251125) has ever
// heard of the header family, and a legacy server given an unrecognized
// Mcp-Param-* header has no defined reaction to it (it is not a header any
// of those specs reserve, forbid, or say anything at all about). This is the
// one place the era difference between the two ClientDialect implementations
// actually lives: no caller of Prepare branches on era to decide whether to
// populate CallRequest.HeaderParams in the first place (see that field's own
// doc) — dialect2026Client.Prepare renders it, this dialect silently does
// not, and CallMCPTool sets it unconditionally either way.
func (d *legacyClientDialect) Prepare(req *CallRequest, st *UpstreamState) ([]byte, MapHeader, error) {
	hdr := MapHeader{}
	if st != nil {
		if sid := st.SessionID(); sid != "" {
			hdr[HeaderSessionID] = sid
		}
	}
	if d.version.Compare(V20250618) >= 0 {
		hdr[HeaderProtocolVersion] = string(d.version)
	}
	return req.Raw, hdr, nil
}

// Parse implements ClientDialect. Legacy responses carry the payload
// verbatim under "result" (or an "error" object, which is not this
// dialect's concern to intercept — see the ClientDialect.Parse doc). The
// only case Parse itself flags is malformed JSON. The returned Result's
// Cache is always the zero value: the eras this dialect serves predate the
// CacheableResult concept entirely and have no ttlMs/cacheScope on the wire
// to read, so a zero CacheHint correctly means "no hint" here, and the
// caller's own default freshness policy applies automatically — without
// this dialect, or its caller, ever having to check which era produced the
// response (MCP 2026-07-28 §5).
func (d *legacyClientDialect) Parse(raw []byte) (*Result, *Error) {
	var resp Response
	if err := jsonx.Unmarshal(raw, &resp); err != nil {
		return nil, &Error{Code: CodeParseError, Message: "parse error"}
	}
	return &Result{Payload: resp.Result}, nil
}
