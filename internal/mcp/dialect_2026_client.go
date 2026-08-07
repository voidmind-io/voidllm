package mcp

import (
	"bytes"
	"context"
	"fmt"
	"strconv"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// dialect2026Client implements ClientDialect for V20260728, which carries
// protocol version, identity, and capabilities as per-request params._meta
// instead of an initialize handshake. See dialect_2026.go for the
// inbound-facing counterpart.
type dialect2026Client struct {
	version    Version
	clientInfo ClientInfo
}

// newDialect2026Client returns a ClientDialect for the given modern version,
// self-identifying as clientInfo in every request's
// params._meta["io.modelcontextprotocol/clientInfo"]. Callers are
// responsible for only passing a version whose Era is EraModern.
func newDialect2026Client(v Version, clientInfo ClientInfo) ClientDialect {
	return &dialect2026Client{version: v, clientInfo: clientInfo}
}

// Version implements ClientDialect.
func (d *dialect2026Client) Version() Version {
	return d.version
}

// Warmup implements ClientDialect. It performs no I/O: the defining property
// of the 2026-07-28 revision's stateless core is that every request carries
// its own identity and capabilities, so there is nothing to negotiate ahead
// of time (docs/mcp-v2.md §2, §3.2).
func (d *dialect2026Client) Warmup(_ context.Context, _ RoundTripper, _ *UpstreamState) error {
	return nil
}

// Prepare implements ClientDialect. It injects protocolVersion,
// clientCapabilities, and clientInfo into params._meta — an empty client
// capability object, since VoidLLM declares no extensions yet (see
// docs/mcp-v2.md §6) — then returns the standard request headers:
// MCP-Protocol-Version always, Mcp-Method for every request, and Mcp-Name
// for tools/call, resources/read, and prompts/get (MCP Streamable HTTP
// §4.2). Header values that are not visible ASCII are base64-sentinel-
// encoded via EncodeHeaderValue.
//
// Any _meta the caller already set on raw — a progressToken, logLevel, or
// any other key VoidLLM does not itself interpret — is preserved: only the
// three keys VoidLLM owns are set or overwritten, everything else in the
// existing _meta object passes through unchanged. VoidLLM is a gateway, not
// the origin of the request; silently discarding caller metadata would
// corrupt it.
//
// For a tools/call whose req.HeaderParams is non-empty, Prepare also mirrors
// each resolvable binding onto an Mcp-Param-{Name} header (MCP 2026-07-28
// §4.3): this is the only ClientDialect that does, since Mcp-Param-* is a
// modern-era-only header family (see legacyClientDialect.Prepare's doc).
// Prepare returns a non-nil error, building nothing, if any binding's
// mirrored value is too long to forward in full (MaxParamHeaderValueLength)
// — see the mirroring loop's own inline comment for why this is fail-closed
// rather than a silently-skipped header.
func (d *dialect2026Client) Prepare(req *CallRequest, _ *UpstreamState) ([]byte, MapHeader, error) {
	raw := req.Raw

	var top map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &top); err != nil {
		return nil, nil, fmt.Errorf("dialect2026 client: parse outbound request: %w", err)
	}

	var method string
	if m, ok := top["method"]; ok {
		if err := jsonx.Unmarshal(m, &method); err != nil {
			return nil, nil, fmt.Errorf("dialect2026 client: parse outbound method: %w", err)
		}
	}

	var params map[string]jsonx.RawMessage
	if p, ok := top["params"]; ok && len(p) > 0 {
		if err := jsonx.Unmarshal(p, &params); err != nil {
			return nil, nil, fmt.Errorf("dialect2026 client: parse outbound params: %w", err)
		}
	}
	if params == nil {
		params = map[string]jsonx.RawMessage{}
	}

	// Preserve any _meta the caller already set — merge our three keys into
	// it rather than replacing the object outright.
	var meta map[string]jsonx.RawMessage
	if existingMeta, ok := params["_meta"]; ok && len(existingMeta) > 0 {
		if err := jsonx.Unmarshal(existingMeta, &meta); err != nil {
			return nil, nil, fmt.Errorf("dialect2026 client: parse outbound _meta: %w", err)
		}
	}
	if meta == nil {
		meta = map[string]jsonx.RawMessage{}
	}

	protocolVersionRaw, err := jsonx.Marshal(d.version)
	if err != nil {
		return nil, nil, fmt.Errorf("dialect2026 client: marshal protocol version: %w", err)
	}
	meta[metaProtocolVersion] = protocolVersionRaw

	clientCapsRaw, err := jsonx.Marshal(map[string]any{})
	if err != nil {
		return nil, nil, fmt.Errorf("dialect2026 client: marshal client capabilities: %w", err)
	}
	meta[metaClientCapabilities] = clientCapsRaw

	clientInfoRaw, err := jsonx.Marshal(d.clientInfo)
	if err != nil {
		return nil, nil, fmt.Errorf("dialect2026 client: marshal client info: %w", err)
	}
	meta[metaClientInfo] = clientInfoRaw

	metaRaw, err := jsonx.Marshal(meta)
	if err != nil {
		return nil, nil, fmt.Errorf("dialect2026 client: marshal _meta: %w", err)
	}
	params["_meta"] = metaRaw

	paramsRaw, err := jsonx.Marshal(params)
	if err != nil {
		return nil, nil, fmt.Errorf("dialect2026 client: marshal params: %w", err)
	}
	top["params"] = paramsRaw

	prepared, err := jsonx.Marshal(top)
	if err != nil {
		return nil, nil, fmt.Errorf("dialect2026 client: marshal request: %w", err)
	}

	hdr := MapHeader{
		HeaderProtocolVersion: string(d.version),
		HeaderMethod:          EncodeHeaderValue(method),
	}
	if name := targetName(method, params); name != "" {
		hdr[HeaderName] = EncodeHeaderValue(name)
	}

	// x-mcp-header mirroring (MCP 2026-07-28 §4.3). Gated on method and on
	// len(req.HeaderParams) — for the overwhelming majority of tools/call
	// requests (a tool whose schema carries no x-mcp-header annotation at
	// all) this is a single slice-length comparison, and every other
	// JSON-RPC method never even reaches the length check.
	if method == "tools/call" && len(req.HeaderParams) > 0 {
		argsRaw := params["arguments"]
		for _, p := range req.HeaderParams {
			val, ok := headerParamValue(argsRaw, p)
			if !ok {
				// Missing, null, or type-mismatched argument — see
				// headerParamValue's doc for why this is silently skipped,
				// not an error.
				continue
			}
			encoded := EncodeHeaderValue(val)
			// A too-long encoded value fails the WHOLE request closed,
			// before any upstream I/O — Prepare's caller (doCall) never
			// calls rawPost when this returns a non-nil error (see doCall's
			// own doc). This mirrors collectMCPParamHeaders'
			// (internal/api/admin/mcp_proxy.go) identical fail-closed
			// treatment of the same condition on the inbound side
			// (docs/mcp-v2.md review round, Fund 3): silently omitting this
			// one header and sending the request anyway would let the
			// outbound Mcp-Param-{Name} header set describe a different
			// call than the JSON-RPC body's arguments carry. Any OTHER
			// reason ValidParamHeaderValue might reject encoded — it cannot,
			// in practice: EncodeHeaderValue's own output is always either
			// unchanged visible ASCII or a base64-sentinel wrapper, both
			// non-empty and visible ASCII by construction — is deliberately
			// not distinguished here; length is the only failure mode this
			// encoding can produce. The error names only the configured
			// limit, never the property name or its value.
			if ParamHeaderValueTooLong(encoded) {
				return nil, nil, fmt.Errorf(
					"dialect2026 client: x-mcp-header value exceeds the limit of %d bytes",
					MaxParamHeaderValueLength)
			}
			hdr[HeaderParamPrefix+p.Name] = encoded
		}
	}

	return prepared, hdr, nil
}

// targetName extracts the params field TargetParamKey names for method (e.g.
// params.name for tools/call, params.uri for resources/read) for the
// Mcp-Name header, per MCP Streamable HTTP §4.2. Returns "" for a method that
// carries no Mcp-Name header, and for a missing or non-string field.
func targetName(method string, params map[string]jsonx.RawMessage) string {
	key, ok := TargetParamKey(method)
	if !ok {
		return ""
	}
	raw, ok := params[key]
	if !ok {
		return ""
	}
	var s string
	if jsonx.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// Parse implements ClientDialect. It returns a non-nil *Error only for
// malformed JSON or a resultType of "input_required" — see the doc on
// ClientDialect.Parse for why an ordinary wire-level JSON-RPC error is
// deliberately NOT treated the same way.
func (d *dialect2026Client) Parse(raw []byte) (*Result, *Error) {
	var resp struct {
		Result jsonx.RawMessage `json:"result"`
		Error  *Error           `json:"error"`
	}
	if err := jsonx.Unmarshal(raw, &resp); err != nil {
		return nil, &Error{Code: CodeParseError, Message: "parse error"}
	}
	if resp.Error != nil || len(resp.Result) == 0 {
		// A genuine wire-level JSON-RPC error, or a result-less response,
		// carries no resultType to inspect for MRTR. Nothing to flag: Call
		// forwards it unchanged, exactly like an ordinary success payload.
		return &Result{}, nil
	}

	var rt struct {
		ResultType string           `json:"resultType"`
		TTLMs      jsonx.RawMessage `json:"ttlMs"`
		CacheScope jsonx.RawMessage `json:"cacheScope"`
	}
	if err := jsonx.Unmarshal(resp.Result, &rt); err != nil {
		return nil, &Error{Code: CodeParseError, Message: "parse error: result"}
	}
	resultType := rt.ResultType
	if resultType == "" {
		// MCP 2026-07-28 §3.8: clients MUST treat an absent resultType (a
		// legacy-shaped result reaching a modern dialect) as "complete".
		resultType = "complete"
	}
	if resultType == "input_required" {
		return nil, &Error{
			Code:    CodeMissingRequiredClientCapability,
			Message: `upstream returned resultType "input_required" (multi round-trip requests are not yet supported)`,
		}
	}

	var payload map[string]any
	if err := jsonx.Unmarshal(resp.Result, &payload); err != nil {
		return nil, &Error{Code: CodeParseError, Message: "parse error: result"}
	}

	result := &Result{Payload: payload}
	if resultType == "complete" {
		// CacheableResult hints (MCP 2026-07-28 §5) are only defined for
		// resultType:"complete" — a "task" result (Tasks extension) or any
		// other future resultType carries no such hint to parse.
		result.Cache = parseCacheHint(rt.TTLMs, rt.CacheScope)
	}
	return result, nil
}

// parseCacheHint interprets the ttlMs and cacheScope fields of a
// resultType:"complete" response per MCP 2026-07-28 §5 (CacheableResult). It
// is called only from Parse's resultType == "complete" branch above.
//
// §5 makes ttlMs mandatory, and defines a missing value as 0 ("Fehlt es,
// gilt 0") — but that default applies to a missing ttlMs WITHIN an already
// present CacheableResult hint. It does not, by itself, say what an upstream
// that implements CacheableResult at all looks like on the wire. A server
// that predates, or simply never implements, CacheableResult emits neither
// ttlMs NOR cacheScope — there is nothing in a resultType:"complete" result
// shape that distinguishes "this server has no cache hint to offer" from
// "this server offers ttlMs: 0" if the two fields' mere absence, on its own,
// were read as the latter. This function therefore treats the two fields
// together, not ttlMs in isolation: BOTH absent means no hint was offered at
// all (CacheHint{}, TTLMsSet false — ToolCache.resolveTTL falls back to its
// own configured maxAge, exactly as it does for a legacy response, which
// predates CacheableResult entirely and is never passed through this
// function — see legacyClientDialect.Parse's doc). The moment EITHER field
// is present, the hint is being offered, and §5's own default for a missing
// ttlMs applies within it: TTLMsSet is true and a still-absent or unusable
// ttlMs normalizes to 0, per the rest of this function's doc below.
//
// This distinction adds no era-awareness anywhere outside this dialect:
// ToolCache.resolveTTL, the sole reader of TTLMsSet, still only ever asks
// whether TTLMsSet is true, never which dialect produced the CacheHint —
// the era-neutrality of resolveTTL, a load-bearing property of this
// package's design, is unchanged by this function alone learning to look at
// both fields together instead of unconditionally setting TTLMsSet.
//
// ttlMsRaw, once the hint is established as present, is honored as the
// literal value only when it is a bare JSON integer literal — no decimal
// point, exponent, surrounding quotes, or brackets. Any other shape (a
// non-integer number such as 1000.0, a JSON string, object, or array, or
// the field being absent while cacheScope alone establishes the hint) is an
// unusable value and is normalized to 0 — the same outcome §5 already
// prescribes for a present-but-negative value ("negative -> treated as 0").
// Every CacheHint this function returns for a present hint therefore has
// TTLMsSet == true and an already-normalized, non-negative TTLMs.
//
// cacheScopeRaw is honored only when it decodes to exactly "public" or
// "private"; any other value leaves Scope at its zero value — §5 defines no
// fallback scope for a missing or invalid cacheScope, unlike ttlMs.
func parseCacheHint(ttlMsRaw, cacheScopeRaw jsonx.RawMessage) CacheHint {
	if len(ttlMsRaw) == 0 && len(cacheScopeRaw) == 0 {
		// Neither field is present at all: this response carries no
		// CacheableResult hint to interpret, as opposed to an explicit
		// ttlMs: 0 — see the doc above. The zero CacheHint (TTLMsSet false,
		// Scope "") is exactly what tells ToolCache.resolveTTL to fall back
		// to its own configured maxAge instead of the 1-second floor an
		// explicit-but-unusable hint would trigger.
		return CacheHint{}
	}

	// At least one of the two fields is present, so a CacheableResult hint
	// IS being offered: TTLMsSet is true regardless of whether ttlMs itself,
	// specifically, is one of the present fields — see the doc above.
	hint := CacheHint{TTLMsSet: true}

	if trimmed := bytes.TrimSpace(ttlMsRaw); len(trimmed) > 0 {
		if n, err := strconv.ParseInt(string(trimmed), 10, 64); err == nil {
			hint.TTLMs = max(0, n)
		}
	}

	var scope string
	if len(cacheScopeRaw) > 0 && jsonx.Unmarshal(cacheScopeRaw, &scope) == nil {
		if scope == CacheScopePublic || scope == CacheScopePrivate {
			hint.Scope = scope
		}
	}

	return hint
}
