package admin

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/jsonx"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// metaProtocolVersionKey is the params._meta key the 2026-07-28 revision
// defines for the caller's requested protocol version (docs/mcp-v2.md §3.2),
// mirrored in the MCP-Protocol-Version header. Duplicated here rather than
// exported from internal/mcp: header-vs-body cross-validation is an
// HTTP-transport concern the mcp package deliberately stays free of — see
// its package doc.
const metaProtocolVersionKey = "io.modelcontextprotocol/protocolVersion"

// HandleMCP processes MCP JSON-RPC 2.0 requests over HTTP POST /api/v1/mcp/voidllm.
func (h *Handler) HandleMCP(c fiber.Ctx) error {
	return h.handleMCPRequest(c, h.MCPServer)
}

// HandleCodeModeMCP processes MCP JSON-RPC 2.0 requests over HTTP POST /api/v1/mcp.
func (h *Handler) HandleCodeModeMCP(c fiber.Ctx) error {
	return h.handleMCPRequest(c, h.CodeModeServer)
}

// fiberHeader adapts a Fiber request context to mcp.Header so internal/mcp
// stays decoupled from the HTTP framework it happens to run behind.
type fiberHeader struct {
	c fiber.Ctx
}

// Get implements mcp.Header.
func (h fiberHeader) Get(name string) string {
	return h.c.Get(name)
}

// handleMCPRequest is the shared implementation for MCP POST handlers. It
// injects the authenticated caller identity into the Go context before
// dispatching to the given MCP server so that tool handlers can scope queries
// to the caller's organization without importing Fiber or the auth package.
//
// The standard request headers are always cross-checked against the body,
// per MCP Streamable HTTP §4.5: a mismatch, or a required header missing
// while the body claims a modern protocol version, is rejected with HTTP 400
// and JSON-RPC CodeHeaderMismatch, so that intermediaries routing on these
// headers and the server executing the body never disagree about which
// version, method, or tool a request names. A genuinely legacy request
// (neither the header nor the body claims a modern revision) incurs no
// validation error here — see validateMCPHeaders.
//
// When the request carries Accept: text/event-stream, the JSON-RPC response is
// wrapped as a Server-Sent Events message per the MCP Streamable HTTP spec.
func (h *Handler) handleMCPRequest(c fiber.Ctx, server *mcp.Server) error {
	body := append([]byte{}, c.Body()...)
	if len(body) == 0 {
		return c.JSON(mcp.NewErrorResponse(nil, mcp.CodeParseError, "empty request body"))
	}

	if id, verr := validateMCPHeaders(c, body); verr != nil {
		c.Status(fiber.StatusBadRequest)
		return c.JSON(mcp.Response{JSONRPC: "2.0", ID: id, Error: verr})
	}

	ki := auth.KeyInfoFromCtx(c)
	ctx := c.Context()
	if ki != nil {
		ctx = mcp.WithKeyIdentity(ctx, mcp.KeyIdentity{
			OrgID:  ki.OrgID,
			TeamID: ki.TeamID,
			KeyID:  ki.ID,
			UserID: ki.UserID,
			Role:   ki.Role,
		})
	}

	result := server.Handle(ctx, body, fiberHeader{c})

	switch result.Hint {
	case mcp.HintNotification:
		return c.SendStatus(fiber.StatusAccepted)
	case mcp.HintBadRequest:
		// Per MCP Streamable HTTP §4.5/§4.6 (docs/mcp-v2.md §4.6): a dual-era
		// client probing a server recognizes it as modern by an HTTP 400
		// carrying a recognizable modern JSON-RPC error body.
		c.Status(fiber.StatusBadRequest)
	case mcp.HintMethodNotFound:
		// Only the modern era gets 404 here; a legacy CodeMethodNotFound
		// response deliberately keeps the default 200 — see HintMethodNotFound's
		// doc on why a 404 would break legacy clients' transport fallback.
		if result.Era == mcp.EraModern {
			c.Status(fiber.StatusNotFound)
		}
	}

	if acceptsSSE(c.Get("Accept")) {
		c.Set("Content-Type", "text/event-stream")
		c.Set("Cache-Control", "no-cache")
		c.Set("X-Accel-Buffering", "no")
		return c.SendString(formatSSEMessage(result.Body))
	}

	c.Set("Content-Type", "application/json")
	return c.Send(result.Body)
}

// formatSSEMessage renders body as a single SSE "message" event. Per the SSE
// wire format, every line of the payload needs its own "data:" prefix — a
// single "data: <body>" line does not survive a body that itself contains a
// raw newline, since everything after the first newline would arrive on the
// client as a bare, prefix-less line and be discarded, silently truncating
// the message to its first line. Used both here and by HandleMCPProxy
// (mcp_proxy.go), which forwards upstream-controlled bytes that VoidLLM does
// not control the formatting of.
func formatSSEMessage(body []byte) string {
	var b strings.Builder
	b.WriteString("event: message\n")
	for _, line := range strings.Split(string(body), "\n") {
		b.WriteString("data: ")
		b.WriteString(strings.TrimSuffix(line, "\r"))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}

// isModernProtocolVersion reports whether headerVal names a protocol version
// VoidLLM recognizes and whose era is EraModern. An empty, unrecognized, or
// legacy value returns false, in which case the request is handled with no
// header/body validation, exactly as before this revision was introduced.
func isModernProtocolVersion(headerVal string) bool {
	v := mcp.Version(headerVal)
	return v.Valid() && v.Era() == mcp.EraModern
}

// mcpHeaderProbe is the subset of a JSON-RPC request body validateMCPHeaders
// needs to cross-check against transport headers.
type mcpHeaderProbe struct {
	ID     jsonx.RawMessage `json:"id"`
	Method string           `json:"method"`
	Params struct {
		Name string                      `json:"name"`
		URI  string                      `json:"uri"`
		Meta map[string]jsonx.RawMessage `json:"_meta"`
	} `json:"params"`
}

// bodyProtocolVersion extracts params._meta["io.modelcontextprotocol/protocolVersion"]
// from probe as a string. Returns "" if the field is absent or is not a JSON
// string. A present-but-wrongly-typed value is a malformed _meta field —
// dialect2026.Decode's concern (CodeInvalidParams), not header validation's;
// here it is simply treated as "the body claims no version".
func bodyProtocolVersion(probe mcpHeaderProbe) string {
	raw, ok := probe.Params.Meta[metaProtocolVersionKey]
	if !ok {
		return ""
	}
	var s string
	if jsonx.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// validateMCPHeaders enforces MCP Streamable HTTP §4.5 for every request —
// not only ones that already carry a modern MCP-Protocol-Version header,
// since a body claiming a modern revision without that header is itself a
// violation (see the second returned-error case below). Returns the
// request's JSON-RPC ID (for use in an error response's ID field, so a
// rejection is still correlatable to the request that triggered it) and nil
// when the request is valid, or the same ID alongside a CodeHeaderMismatch
// error describing the first violation found.
//
// Violations checked, in order:
//
//  1. The header names a version and the body's
//     params._meta["io.modelcontextprotocol/protocolVersion"] names a
//     DIFFERENT one. Absence of either side is not itself a mismatch — only
//     an actual disagreement between two present values is.
//  2. The body's _meta.protocolVersion names a modern revision but the
//     MCP-Protocol-Version header — MUST for every modern request — is
//     absent. Without this check a modern-bodied request with no header
//     would be silently served as the legacy default (Negotiate's rule 4),
//     which is the exact interop bug this check exists to close.
//  3. For a request modern by either signal: Mcp-Method must be present and
//     match the body's "method". For the three methods mcp.TargetParamKey
//     names — tools/call, prompts/get (params.name) and resources/read
//     (params.uri) — Mcp-Name must additionally be present and match that
//     field. Header values are decoded per the §4.4 base64 sentinel format
//     before comparison.
//
// A request that is not modern by either signal (the common case — most
// callers are legacy or omit MCP-Protocol-Version entirely) incurs none of
// this: validateMCPHeaders returns immediately once it establishes that, and
// a body that fails to parse as JSON is left for Server.Handle's own Decode
// step to report as CodeParseError, unchanged from pre-existing behavior.
func validateMCPHeaders(c fiber.Ctx, body []byte) (jsonx.RawMessage, *mcp.Error) {
	hdrVersion := mcp.ProtocolVersionHeaderValue(fiberHeader{c})

	var probe mcpHeaderProbe
	parseErr := jsonx.Unmarshal(body, &probe)

	var bodyVersion string
	if parseErr == nil {
		bodyVersion = bodyProtocolVersion(probe)
	}

	switch {
	case hdrVersion == "" && isModernProtocolVersion(bodyVersion):
		return probe.ID, &mcp.Error{Code: mcp.CodeHeaderMismatch, Message: "missing required header: " + mcp.HeaderProtocolVersion}
	case hdrVersion != "" && bodyVersion != "" && hdrVersion != bodyVersion:
		return probe.ID, &mcp.Error{Code: mcp.CodeHeaderMismatch, Message: mcp.HeaderProtocolVersion + " header does not match request body"}
	}

	if !isModernProtocolVersion(hdrVersion) {
		return probe.ID, nil
	}

	// From here on hdrVersion is confirmed modern: Mcp-Method (and, for
	// tools/call, resources/read, and prompts/get, Mcp-Name) are required.
	if parseErr != nil {
		return probe.ID, &mcp.Error{Code: mcp.CodeHeaderMismatch, Message: "header validation: request body is not valid JSON"}
	}

	hdrMethod := c.Get(mcp.HeaderMethod)
	if hdrMethod == "" {
		return probe.ID, &mcp.Error{Code: mcp.CodeHeaderMismatch, Message: "missing required header: " + mcp.HeaderMethod}
	}
	decodedMethod, ok := decodeMCPHeaderValue(hdrMethod)
	if !ok {
		return probe.ID, &mcp.Error{Code: mcp.CodeHeaderMismatch, Message: "invalid " + mcp.HeaderMethod + " header encoding"}
	}
	if decodedMethod != probe.Method {
		return probe.ID, &mcp.Error{Code: mcp.CodeHeaderMismatch, Message: mcp.HeaderMethod + " header does not match request method"}
	}

	targetKey, requiresName := mcp.TargetParamKey(probe.Method)
	if !requiresName {
		return probe.ID, nil
	}

	// tools/call and prompts/get mirror params.name; resources/read mirrors
	// params.uri instead — see mcp.TargetParamKey.
	bodyTarget := probe.Params.Name
	if targetKey == "uri" {
		bodyTarget = probe.Params.URI
	}

	hdrName := c.Get(mcp.HeaderName)
	if hdrName == "" {
		return probe.ID, &mcp.Error{Code: mcp.CodeHeaderMismatch, Message: "missing required header: " + mcp.HeaderName}
	}
	decodedName, ok := decodeMCPHeaderValue(hdrName)
	if !ok {
		return probe.ID, &mcp.Error{Code: mcp.CodeHeaderMismatch, Message: "invalid " + mcp.HeaderName + " header encoding"}
	}
	if decodedName != bodyTarget {
		return probe.ID, &mcp.Error{Code: mcp.CodeHeaderMismatch, Message: mcp.HeaderName + " header does not match request params." + targetKey}
	}

	return probe.ID, nil
}

// decodeMCPHeaderValue reverses the base64 sentinel encoding a modern-era
// client applies to header values that are not visible ASCII (docs/mcp-v2.md
// §4.4). It is a thin wrapper around mcp.DecodeHeaderValue, which is also
// used by internal/mcp's own outbound client dialect (dialect_2026_client.go)
// when encoding these same headers — keeping both directions in the same
// place is what keeps them in lockstep.
func decodeMCPHeaderValue(v string) (decoded string, ok bool) {
	return mcp.DecodeHeaderValue(v)
}

// HandleMCPSSE opens a Server-Sent Events stream on GET /api/v1/mcp/voidllm.
func (h *Handler) HandleMCPSSE(c fiber.Ctx) error {
	return h.handleMCPSSE(c, "/api/v1/mcp/voidllm")
}

// HandleCodeModeMCPSSE opens a Server-Sent Events stream on GET /api/v1/mcp.
func (h *Handler) HandleCodeModeMCPSSE(c fiber.Ctx) error {
	return h.handleMCPSSE(c, "/api/v1/mcp")
}

// handleMCPSSE is the shared implementation for MCP SSE GET handlers. It
// sends an initial endpoint event that tells legacy SSE-only MCP clients
// which URL to POST requests to, then keeps the connection alive with
// periodic comment pings until the client disconnects or a 10-minute
// deadline expires.
//
// A request carrying a modern-era MCP-Protocol-Version is rejected with 405
// Method Not Allowed: the 2026-07-28 revision has no GET-based transport, and
// SSE-only legacy clients never send that header, so this cannot break them.
func (h *Handler) handleMCPSSE(c fiber.Ctx, endpoint string) error {
	if isModernProtocolVersion(mcp.ProtocolVersionHeaderValue(fiberHeader{c})) {
		return c.SendStatus(fiber.StatusMethodNotAllowed)
	}

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("X-Accel-Buffering", "no")

	endpointEvent := fmt.Sprintf("event: endpoint\ndata: %s\n\n", endpoint)

	return c.SendStreamWriter(func(w *bufio.Writer) {
		if _, err := w.WriteString(endpointEvent); err != nil {
			return
		}
		if err := w.Flush(); err != nil {
			return
		}

		deadline := time.After(10 * time.Minute)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if _, err := w.WriteString(": ping\n\n"); err != nil {
					return
				}
				if err := w.Flush(); err != nil {
					return
				}
			case <-deadline:
				return
			}
		}
	})
}

// acceptMediaType is one comma-separated entry of an Accept header, split
// into its media type and quality value by parseAcceptEntry.
type acceptMediaType struct {
	mediaType string
	q         float64
}

// parseAcceptEntry parses a single comma-separated Accept header entry
// (e.g. "text/event-stream ;q=0.9") into its trimmed media type and q
// parameter (RFC 9110 §12.4.2). q defaults to 1 when the entry carries no
// "q" parameter, or when one is present but not a valid number — matching
// RFC 9110's own "absent means 1" default and simply ignoring the kind of
// malformed value real clients are not expected to send, rather than
// treating it as a rejection its author never wrote. Parameter order is not
// assumed: every ";"-separated segment after the media type is scanned for
// one named "q", case-insensitively, so a real Accept entry that lists other
// parameters before "q" (RFC 9110 §12.4.2's own example puts a media-range
// parameter first) is still parsed correctly.
func parseAcceptEntry(part string) acceptMediaType {
	mt := strings.TrimSpace(part)
	q := 1.0
	if idx := strings.IndexByte(mt, ';'); idx >= 0 {
		params := mt[idx+1:]
		mt = strings.TrimSpace(mt[:idx])
		for _, p := range strings.Split(params, ";") {
			name, val, ok := strings.Cut(p, "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(name), "q") {
				continue
			}
			if parsed, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
				q = parsed
			}
		}
	}
	return acceptMediaType{mediaType: mt, q: q}
}

// acceptsSSE checks whether the Accept header contains the text/event-stream
// media type. It correctly handles comma-separated values and quality
// parameters, comparing the media type case-insensitively per RFC 9110
// §8.3.1 ("The type and subtype tokens are case-insensitive") and treating
// an explicit q=0 as "not acceptable" per RFC 9110 §12.4.2 — before this fix
// neither was honored, so "Text/Event-Stream" went unrecognized and
// "text/event-stream;q=0" (a client explicitly declining SSE) was treated as
// accepting it (docs/mcp-v2.md, review finding A3).
func acceptsSSE(accept string) bool {
	for _, part := range strings.Split(accept, ",") {
		am := parseAcceptEntry(part)
		if am.q == 0 {
			continue
		}
		if strings.EqualFold(am.mediaType, "text/event-stream") {
			return true
		}
	}
	return false
}

// acceptsOnlySSE reports whether accept names text/event-stream but does NOT
// also accept application/json (nor a wildcard that would cover it, e.g.
// "*/*" or "application/*"). The modern MCP Streamable HTTP revision requires
// every client to accept both media types (docs/mcp-v2.md §1a); a caller
// whose Accept header names text/event-stream exclusively identifies itself
// as a legacy-era client that never adopted that requirement.
//
// HandleMCPProxy (mcp_proxy.go) uses this to decide whether an upstream
// application/json response must be wrapped as a single SSE "message" event
// via formatSSEMessage — exactly as handleMCPRequest already does for the
// built-in server below — instead of being forwarded to such a caller
// unchanged, which it cannot parse (docs/mcp-v2.md, review finding A2). A
// caller that also accepts application/json (the modern, spec-compliant
// case) always gets the upstream's own choice of representation passed
// through untouched; only the true legacy caller is special-cased.
//
// Like acceptsSSE, media types are compared case-insensitively (RFC 9110
// §8.3.1) and an entry carrying an explicit q=0 is skipped entirely — for
// application/json (or a wildcard covering it) this matters in the opposite
// direction from acceptsSSE: a caller sending
// "text/event-stream, application/json;q=0" is EXPLICITLY declining JSON per
// RFC 9110 §12.4.2, so that entry must not count as "also accepts JSON" and
// suppress the legacy-wrap branch it should instead trigger (docs/mcp-v2.md,
// review finding A3).
func acceptsOnlySSE(accept string) bool {
	sawSSE := false
	for _, part := range strings.Split(accept, ",") {
		am := parseAcceptEntry(part)
		if am.q == 0 {
			continue
		}
		switch {
		case strings.EqualFold(am.mediaType, "text/event-stream"):
			sawSSE = true
		case strings.EqualFold(am.mediaType, "application/json"),
			am.mediaType == "*/*",
			strings.EqualFold(am.mediaType, "application/*"):
			return false
		}
	}
	return sawSSE
}
