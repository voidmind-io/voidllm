package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// ErrUnsupportedProtocolVersion is returned by probeEra (via
// highestSupportedVersion) when an upstream's CodeUnsupportedProtocolVersion
// error explicitly lists the protocol versions it supports, and that list
// names nothing this VoidLLM build recognizes. Unlike a missing
// data.supported field — tolerated because the reserved error code alone
// already proves the server is modern-aware, so probeEra defaults to
// V20260728 — a server that positively lists only revisions VoidLLM has
// never heard of cannot be connected to at all; there is no self-correcting
// default. The error message names VoidLLM's own supported version list and
// how many entries the upstream listed — never the upstream's own
// data.supported strings themselves. Those are, by definition of reaching
// this branch, values Version.Valid() has already rejected: not verified
// protocol version data, but arbitrary upstream-controlled text that could
// name anything, and VoidLLM's zero-knowledge logging rule (docs/mcp-v2.md
// §11.5) forbids echoing upstream free-form content into a message every
// caller of probeEra logs via err.Error() (docs/mcp-v2.md §11.2, review
// finding E).
var ErrUnsupportedProtocolVersion = errors.New("mcp: upstream and voidllm share no supported protocol version")

// ResolvePinnedVersion interprets the mcp_servers.protocol_version column
// value: "auto" (or the empty string, for callers predating the column)
// means "no pin — probe the upstream" and is reported back as the empty
// Version, which HTTPTransport treats as "call probeEra". Any other
// recognized value pins the server to that specific revision and skips
// probeEra entirely. An unrecognized value falls back to "auto" rather than
// failing outright: a corrupted or forward-versioned override should degrade
// to the self-correcting probe path instead of taking the server offline.
func ResolvePinnedVersion(raw string) Version {
	if raw == "" || raw == "auto" {
		return ""
	}
	v := Version(raw)
	if !v.Valid() {
		return ""
	}
	return v
}

// probeEra determines which protocol Version the upstream at t speaks,
// following MCP Streamable HTTP §4.6 exactly (docs/mcp-v2.md §4.6):
//
//  1. Send a modern server/discover request.
//  2. HTTP 200 (or 202) whose body is a genuine modern result — a JSON-RPC
//     success carrying supportedVersions VoidLLM recognizes — → modern,
//     using the highest of those versions (see modernVersionFromDiscoverResult).
//     A LEGACY server receiving an unknown method also answers with HTTP 200
//     per JSON-RPC convention, just with a -32601 error body instead of a
//     result — VoidLLM's own EraLegacy dialect deliberately keeps
//     CodeMethodNotFound at HTTP 200 for exactly this reason (see
//     hintForError in server.go), so status code alone can never
//     distinguish the two here; the body must be inspected.
//  3. HTTP 400 whose body carries a well-formed JSON-RPC 2.0 error object
//     with a recognizable modern code (-32020 HeaderMismatch, -32021
//     MissingRequiredClientCapability, -32022 UnsupportedProtocolVersion) →
//     the server is dual/modern-aware. For -32022 the version used is the
//     highest entry in the error's data.supported that VoidLLM also
//     understands (which may itself be a legacy revision — see
//     highestSupportedVersion); when data.supported is absent, V20260728 is
//     used directly, since the reserved error code's mere presence on the
//     wire already presupposes 2026-07-28; when data.supported IS present
//     but names nothing VoidLLM recognizes, probeEra fails outright with
//     ErrUnsupportedProtocolVersion instead of guessing — that is a genuine
//     incompatibility, not something to paper over. The other two codes
//     always use V20260728 directly, the same way.
//  4. Anything else — HTTP 400/404/405 with no recognizable modern error
//     body, a 200/202 whose body isn't a genuine modern result (including a
//     JSON-RPC error of ANY code, not just -32601), or an unparsable body —
//     falls through to a legacy initialize handshake (probeLegacy). Being
//     wrong about "legacy" costs one redundant handshake; being wrong about
//     "modern" makes the connection unusable, so every ambiguous case here
//     resolves to legacy, never to modern.
//  5. 404/405 on the legacy attempt too → ErrSSENotSupported, unchanged from
//     the detectTransport this function replaces.
//
// The result is a property of the upstream SERVER, not of any one request
// (docs/mcp-v2.md §4.7): resolveBinding caches it for the lifetime of this
// HTTPTransport and re-probes only when that assumption later fails (see
// invalidateBinding).
func (t *HTTPTransport) probeEra(ctx context.Context) (Version, error) {
	discoverReq, err := jsonx.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      "probe-discover",
		"method":  "server/discover",
		"params": map[string]any{
			"_meta": map[string]any{
				metaProtocolVersion:    V20260728,
				metaClientCapabilities: map[string]any{},
				metaClientInfo:         t.clientInfo,
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("probe: marshal server/discover: %w", err)
	}

	discoverHdr := MapHeader{
		HeaderProtocolVersion: string(V20260728),
		HeaderMethod:          "server/discover",
	}
	res, err := t.rawPost(ctx, t.endpoint, discoverReq, discoverHdr)
	if err != nil {
		return "", fmt.Errorf("probe: server/discover: %w", err)
	}

	switch res.status {
	case http.StatusOK, http.StatusAccepted:
		if v, ok := modernVersionFromDiscoverResult(res.body); ok {
			return v, nil
		}
		// A 200/202 whose body is not a genuine modern result — most notably
		// a legacy server's -32601 for the unrecognized server/discover
		// method, itself delivered at HTTP 200 by design (see probeEra's
		// doc) — falls through to the legacy probe below, exactly like a
		// plain 404/405.
	case http.StatusBadRequest:
		v, ok, err := modernVersionFromErrorBody(res.body)
		if err != nil {
			return "", err
		}
		if ok {
			return v, nil
		}
		// Body present but not a recognizable modern error — fall through to
		// the legacy probe below, exactly like a plain 404/405.
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		// Fall through to the legacy probe below.
	default:
		return "", fmt.Errorf("probe: unexpected server/discover status %d", res.status)
	}

	return t.probeLegacy(ctx)
}

// probeLegacy attempts a legacy initialize handshake as the fallback step of
// probeEra, once a modern server/discover attempt was inconclusive.
func (t *HTTPTransport) probeLegacy(ctx context.Context) (Version, error) {
	initReq, err := jsonx.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      "probe-initialize",
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": string(V20250326),
			"capabilities":    map[string]any{},
			"clientInfo":      t.clientInfo,
		},
	})
	if err != nil {
		return "", fmt.Errorf("probe: marshal initialize: %w", err)
	}

	res, err := t.rawPost(ctx, t.endpoint, initReq, nil)
	if err != nil {
		return "", fmt.Errorf("probe: initialize: %w", err)
	}

	switch res.status {
	case http.StatusOK, http.StatusAccepted:
		return legacyVersionFromInitializeBody(res.body), nil
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return "", ErrSSENotSupported
	default:
		return "", fmt.Errorf("probe: unexpected initialize status %d", res.status)
	}
}

// modernVersionFromDiscoverResult inspects the body of an HTTP 200/202
// response to the modern server/discover probe and reports whether it is
// actually a modern result, as opposed to a LEGACY server's JSON-RPC error
// for the unrecognized "server/discover" method — which, per JSON-RPC
// convention, VoidLLM's own EraLegacy dialect keeps at HTTP 200 rather than
// 404 (see hintForError in server.go), so it is indistinguishable from a
// modern success by status code alone.
//
// Returns ok == false — meaning probeEra must fall through to probeLegacy —
// for any of: an unparsable body, a JSON-RPC error object of ANY code
// (including but not limited to -32601 Method Not Found), or a result that
// carries no supportedVersions this VoidLLM build recognizes. Per probeEra's
// doc, every ambiguous case here resolves to "not modern": a wrong legacy
// guess costs one redundant handshake, a wrong modern guess makes the
// connection unusable.
func modernVersionFromDiscoverResult(body []byte) (Version, bool) {
	var resp struct {
		Result *struct {
			SupportedVersions []string `json:"supportedVersions"`
		} `json:"result"`
		Error *Error `json:"error"`
	}
	if err := jsonx.Unmarshal(body, &resp); err != nil {
		return "", false
	}
	if resp.Error != nil {
		return "", false
	}
	if resp.Result == nil {
		return "", false
	}
	return highestKnownVersion(resp.Result.SupportedVersions)
}

// modernVersionFromErrorBody inspects the JSON-RPC error body of an HTTP 400
// response for one of the three modern-era error codes (docs/mcp-v2.md §4.6,
// §8). Returns ok == false if the body is not a recognizable modern error at
// all, in which case probeEra falls back to the legacy probe. A body is only
// accepted as a modern error if it is a well-formed JSON-RPC 2.0 response
// (jsonrpc == "2.0" and a present error object) carrying one of the three
// codes — matching the numeric code alone is not enough: a legacy server or
// an unrelated gateway could reuse -32020..-32022 for its own purposes, and
// treating that as proof of modernity would violate probeEra's "when in
// doubt, legacy" rule. Returns a non-nil error only for
// CodeUnsupportedProtocolVersion, and only when the upstream's data.supported
// list is present but names nothing VoidLLM recognizes — see
// highestSupportedVersion.
func modernVersionFromErrorBody(body []byte) (Version, bool, error) {
	var resp struct {
		JSONRPC string `json:"jsonrpc"`
		Error   *Error `json:"error"`
	}
	if err := jsonx.Unmarshal(body, &resp); err != nil || resp.JSONRPC != "2.0" || resp.Error == nil {
		return "", false, nil
	}

	switch resp.Error.Code {
	case CodeHeaderMismatch, CodeMissingRequiredClientCapability:
		return V20260728, true, nil
	case CodeUnsupportedProtocolVersion:
		v, err := highestSupportedVersion(resp.Error.Data)
		if err != nil {
			return "", false, err
		}
		return v, true, nil
	default:
		return "", false, nil
	}
}

// highestSupportedVersion picks the newest version VoidLLM understands from
// an UnsupportedProtocolVersionError's data.supported list (docs/mcp-v2.md
// §8), which may name either era. Two cases return the V20260728 default
// rather than an error: data.supported is absent entirely, or data itself
// fails to parse as the expected shape — in both, the reserved error code's
// mere presence on the wire already proves the server is modern-aware, so
// there is nothing to lose by assuming the newest revision. The one case
// that IS an error: data.supported is present, parses cleanly, and names
// only revisions VoidLLM does not recognize — a genuine version mismatch the
// caller needs to know about, not something to silently paper over with a
// guess (see ErrUnsupportedProtocolVersion).
//
// The returned error's message deliberately omits *parsed.Supported itself:
// those strings are upstream-controlled and, having just failed
// highestKnownVersion, are proven to be something other than a protocol
// version VoidLLM recognizes — not verified version data, but arbitrary
// attacker-controlled text a malicious upstream can set to anything (see
// ErrUnsupportedProtocolVersion's doc, docs/mcp-v2.md review finding E).
// Only the count of entries the upstream listed, and VoidLLM's own supported
// versions, are included.
func highestSupportedVersion(data any) (Version, error) {
	raw, err := jsonx.Marshal(data)
	if err != nil {
		return V20260728, nil
	}
	var parsed struct {
		Supported *[]string `json:"supported"`
	}
	if err := jsonx.Unmarshal(raw, &parsed); err != nil || parsed.Supported == nil {
		return V20260728, nil
	}
	if v, ok := highestKnownVersion(*parsed.Supported); ok {
		return v, nil
	}
	return "", fmt.Errorf("%w: upstream listed %d supported version(s) voidllm does not recognize, voidllm supports %v",
		ErrUnsupportedProtocolVersion, len(*parsed.Supported), versionStrings(SupportedVersions()))
}

// versionStrings converts a []Version to []string for use in error messages
// and other contexts that need the plain string form.
func versionStrings(versions []Version) []string {
	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = string(v)
	}
	return out
}

// highestKnownVersion returns the newest version among versions that this
// VoidLLM build recognizes (Version.Valid), or ok == false if none are —
// versions is otherwise attacker/upstream-controlled and may name revisions
// VoidLLM has never heard of, which must be skipped rather than compared.
func highestKnownVersion(versions []string) (Version, bool) {
	var best Version
	for _, s := range versions {
		v := Version(s)
		if !v.Valid() {
			continue
		}
		if best == "" || v.Compare(best) > 0 {
			best = v
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// legacyVersionFromInitializeBody extracts the negotiated protocolVersion
// from a successful legacy initialize response when present and valid,
// falling back to V20250326 — the version probeLegacy asked for — matching
// legacyDialect.Decode's own leniency toward a missing or unrecognized
// protocolVersion field.
func legacyVersionFromInitializeBody(body []byte) Version {
	var resp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := jsonx.Unmarshal(body, &resp); err != nil {
		return V20250326
	}
	v := Version(resp.Result.ProtocolVersion)
	if v.Valid() && v.Era() == EraLegacy {
		return v
	}
	return V20250326
}
