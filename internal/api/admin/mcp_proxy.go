package admin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	apihealth "github.com/voidmind-io/voidllm/internal/api/health"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/jsonx"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/internal/metrics"
	"github.com/voidmind-io/voidllm/internal/usage"
	"github.com/voidmind-io/voidllm/pkg/crypto"
)

// mcpClientInfo self-identifies VoidLLM to upstream MCP servers in both
// protocol eras: the legacy initialize handshake's clientInfo field and the
// modern era's params._meta["io.modelcontextprotocol/clientInfo"]. Used only
// by ListTools and CallMCPTool's transport.Call — HandleMCPProxy's
// transport.Forward never sends VoidLLM's own identity anywhere (see
// forwardHeaders and Forward's doc).
var mcpClientInfo = mcp.ClientInfo{Name: "voidllm", Version: apihealth.Version}

// forwardHeaders extracts the MCP standard request headers (MCP Streamable
// HTTP §4.2 — MCP-Protocol-Version, Mcp-Method, Mcp-Name — plus the legacy
// mcp.HeaderSessionID) from the inbound request c, then adds every validly
// named and valued Mcp-Param-{Name} header the caller sent (§4.3) via
// collectMCPParamHeaders, for mcp.HTTPTransport.Forward to send upstream.
// HandleMCPProxy is a transparent intermediary (see Forward's doc): these
// are the only headers ever mirrored — never Connection, Host, or any other
// hop-by-hop or unrelated header the caller happened to send — following
// this repo's allowlist-only convention for header forwarding. Unlike the
// four MCP standard headers, which are a fixed, known-in-advance set, the
// Mcp-Param-* family is open-ended by design: §4.3 requires an intermediary
// that does not itself recognize a given Mcp-Param-{Name} header to forward
// it uninterpreted anyway, rather than silently drop it, which is what this
// function did before collectMCPParamHeaders existed — see that function's
// own doc for the validation and dedup rules governing which of them
// actually get mirrored, and for the invariant governing what a mirrored
// value may and may not be used for.
//
// Returns a non-nil *mcp.Error, with a nil map, exactly when
// collectMCPParamHeaders does (see that function's own doc for why): the
// caller presented more valid, non-duplicate Mcp-Param-* headers than
// mcp.MaxParamHeaders, and neither this function nor collectMCPParamHeaders
// has any principled subset of them to drop on its own authority. The
// HandleMCPProxy caller must reject the whole request in that case instead
// of calling Forward with a header set it knows to be incomplete.
//
// mcp.HeaderSessionID is the one FIX 1/FIX 3 depend on: Forward mints and
// re-initializes no session of its own, so a legacy caller's own session, if
// any, must reach the upstream this way or not at all — but "reach the
// upstream this way" is no longer unconditional: HandleMCPProxy itself may
// still delete this entry from the map forwardHeaders returns, before ever
// calling Forward, if h.MCPSessionRegistry never saw the upstream issue it to
// this caller (see HandleMCPProxy's own comments at the Forward call site).
// The other three let a genuinely modern caller's own standard request
// headers reach the upstream exactly as sent, instead of being silently
// dropped by an intermediary that no longer runs the request through a
// dialect's Prepare.
//
// authHeaderName is server.AuthHeader for the upstream server this request
// targets — the header name rawPost/Forward set to carry this server's own
// credential when AuthType is "header" — passed through unchanged to
// collectMCPParamHeaders so it can refuse to forward a caller-supplied
// Mcp-Param-* header of that exact name (see that function's own doc for
// why).
func forwardHeaders(c fiber.Ctx, authHeaderName string) (mcp.MapHeader, *mcp.Error) {
	hdr := mcp.MapHeader{}
	for _, name := range []string{mcp.HeaderProtocolVersion, mcp.HeaderMethod, mcp.HeaderName, mcp.HeaderSessionID} {
		if v := c.Get(name); v != "" {
			hdr[name] = v
		}
	}
	if paramErr := collectMCPParamHeaders(c, authHeaderName, hdr); paramErr != nil {
		return nil, paramErr
	}
	return hdr, nil
}

// mcpParamHeaderCandidate is one Mcp-Param-{Name} header collectMCPParamHeaders
// has provisionally accepted from the inbound request — name and value
// already copied out of fasthttp's request buffer (see that function's own
// doc, rule 1) — pending the case-insensitive-duplicate check that can still
// drop it before it ever reaches the header map forwardHeaders returns.
// lower is name lowercased once, up front, so neither the sort nor the
// duplicate scan recomputes it per comparison.
type mcpParamHeaderCandidate struct {
	name  string
	lower string
	value string
}

// collectMCPParamHeaders scans the inbound request c for Mcp-Param-{Name}
// headers (MCP Streamable HTTP §4.3) and, when every one of them can be
// forwarded in full, adds each survivor into hdr so mcp.HTTPTransport.Forward
// mirrors it upstream exactly as received — closing the gap forwardHeaders
// otherwise left in §4.3's obligation for an intermediary to forward,
// uninterpreted, any Mcp-Param-{Name} header it does not itself recognize.
//
// Returns a non-nil *mcp.Error, and leaves hdr untouched, when more than
// mcp.MaxParamHeaders headers survive validation and deduplication (rule 6
// below). §4.3 obligates forwarding the WHOLE set of Mcp-Param-{Name}
// headers this proxy does not itself recognize, and there is no principled
// way to choose a subset once that set is too large to forward in full: any
// subset dropped silently would make the headers Forward sends upstream
// describe a different call than the JSON-RPC body they travel alongside,
// and the upstream's own §4.5 header-against-body validation would then
// fail with a HeaderMismatch whose real cause — too many headers at
// VoidLLM's own edge — is invisible to whoever ends up debugging it, one hop
// away from where the discrepancy actually originated. Rejecting the whole
// request here instead, with a cause the caller can act on directly, is
// strictly more diagnosable than silently forwarding an incomplete header
// set and letting the upstream discover the mismatch on VoidLLM's behalf.
// This is also why the previous behavior — sorting survivors by lowercased
// name and truncating to mcp.MaxParamHeaders — is gone: it made the choice
// of which headers to drop deterministic, but a deterministic wrong answer
// is still wrong, and rule 6 below no longer needs sort.Slice's ordering to
// decide anything; only rule 4's dedup does (see that step's own inline
// comment).
//
// A Mcp-Param-{Name} value is a tool argument. It is forwarded uninterpreted
// and MUST NEVER be read, here or anywhere downstream of hdr, to make a
// routing, rate-limiting, metrics-label, or usage-logging decision, unless
// that call site first adds the check §4.5 requires before any mirrored
// header may be trusted for such a decision: that the request's
// MCP-Protocol-Version names a revision whose spec mandates header-against-
// body validation. Nor does this function validate a Mcp-Param-{Name} value
// against the request body itself, the way a server that actually executes
// the tool call is required to (§4.5): doing so would require resolving the
// addressed tool's inputSchema to learn which property, if any, the header
// is supposed to mirror — an upstream fetch this transparent proxy path has
// no business making on the hot path for a tool it may never have fetched
// at all, and the entire reason HandleMCPProxy exists as a pass-through
// rather than a second implementation of the built-in server's own request
// handling.
//
// Rules, applied in this order:
//
//  1. c's headers are iterated via the raw fasthttp API, and each header's
//     name and value are copied into an owned string via string(...)
//     immediately: fasthttp's header iterator returns slices into the
//     request's read buffer, which does not outlive HandleMCPProxy the way
//     a value derived from it might — see alias's identical copy at
//     HandleMCPProxy's own top for the full trap this avoids.
//  2. A name that mcp.ValidParamHeaderName rejects is silently skipped: it
//     is either not in the Mcp-Param-* family at all (true of nearly every
//     header on an ordinary request, which is why no candidate slice is
//     even allocated until the first one passes this check) or not validly
//     shaped.
//  3. A value that mcp.ValidParamHeaderValue rejects is silently skipped,
//     never rejects the request outright: §4.3 obligates forwarding a
//     HEADER, not forwarding something that does not even parse as one,
//     and this proxy has no body-derived schema to fall back on validating
//     against (see above).
//  4. A name that recurs, case-insensitively, anywhere in the request is
//     dropped entirely — neither occurrence is forwarded. hdr is a plain
//     map, so keeping whichever occurrence was visited last would depend on
//     fasthttp's iteration order, and two different values sent for the
//     same parameter name are unresolvable from this vantage point anyway:
//     only the addressed tool's inputSchema, which this function
//     deliberately never fetches (see above), could ever say which one the
//     body agrees with.
//  5. A name that collides, case-insensitively, with authHeaderName — the
//     server's own configured auth_header — is dropped. rawPost and Forward
//     apply this server's authentication BEFORE merging hdr's entries in,
//     specifically so hdr always wins a name collision against a
//     misconfigured auth_header (see that ordering's own doc in
//     http_transport.go). That was a safe assumption while hdr only ever
//     held VoidLLM-selected or already-validated values; it stops being one
//     the moment hdr can carry an open-ended, caller-chosen header set.
//     Server registration rejects a NEW auth_header equal to any reserved
//     MCP header name, this prefix included (isReservedMCPHeader,
//     mcp_servers.go) — but a caller does not control auth_header, and a
//     server registered before that validation existed could still have one
//     configured. Without this check, any caller could silently overwrite
//     that server's own credential on the outbound request merely by
//     sending a Mcp-Param-{Name} header with the same name.
//  6. Survivors are counted: more than mcp.MaxParamHeaders of them and the
//     WHOLE request is rejected (see the doc above for why) rather than any
//     subset of them forwarded. Exactly mcp.MaxParamHeaders is still
//     accepted in full — this is a ceiling on what gets forwarded, not a
//     trigger at or below it.
func collectMCPParamHeaders(c fiber.Ctx, authHeaderName string, hdr mcp.MapHeader) *mcp.Error {
	authLower := strings.ToLower(authHeaderName)

	var candidates []mcpParamHeaderCandidate
	for k, v := range c.Request().Header.All() {
		name := string(k)
		if !mcp.ValidParamHeaderName(name) {
			continue
		}
		value := string(v)
		if !mcp.ValidParamHeaderValue(value) {
			continue
		}
		lower := strings.ToLower(name)
		if authHeaderName != "" && lower == authLower {
			continue
		}
		candidates = append(candidates, mcpParamHeaderCandidate{name: name, lower: lower, value: value})
	}
	if len(candidates) == 0 {
		return nil
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].lower < candidates[j].lower })

	// Drop every name that recurs case-insensitively (rule 4 above). After
	// sorting by lower, all occurrences of the same name are adjacent, so a
	// single forward scan finds each run and keeps it only when it has
	// exactly one member. deduped reuses candidates' own backing array: its
	// write position never exceeds the read position i, so no unread
	// element is overwritten before it is read (the standard in-place
	// filter idiom).
	deduped := candidates[:0]
	for i := 0; i < len(candidates); {
		j := i + 1
		for j < len(candidates) && candidates[j].lower == candidates[i].lower {
			j++
		}
		if j-i == 1 {
			deduped = append(deduped, candidates[i])
		}
		i = j
	}

	// Rule 6: reject the whole request rather than forward a subset — see the
	// function's own doc for why. The error message carries only the count
	// and the configured limit, both operator-controlled or trivially
	// re-derivable, never a header name or value: a Mcp-Param-{Name} value is
	// a tool argument (see the type doc above) and header NAMES here are
	// caller-chosen strings mirroring tool-schema property names, which this
	// repo's zero-knowledge logging contract treats with the same caution.
	if len(deduped) > mcp.MaxParamHeaders {
		return &mcp.Error{
			Code: mcp.CodeTooManyParamHeaders,
			Message: fmt.Sprintf(
				"too many Mcp-Param-* headers: %d exceeds the limit of %d",
				len(deduped), mcp.MaxParamHeaders),
		}
	}
	for _, cand := range deduped {
		hdr[cand.name] = cand.value
	}
	return nil
}

// buildAdHocTransport creates a one-off HTTPTransport for server s when the
// persistent transport cache has no entry (cold start or cache miss). It decrypts
// both the bearer auth token and the OAuth client secret as needed.
// The caller is responsible for calling Close on the returned transport.
func (h *Handler) buildAdHocTransport(server *db.MCPServer, timeout time.Duration) (*mcp.HTTPTransport, error) {
	var authToken string
	if server.AuthTokenEnc != nil && *server.AuthTokenEnc != "" {
		decrypted, decErr := crypto.DecryptString(*server.AuthTokenEnc, h.EncryptionKey, mcpServerAAD(server.ID))
		if decErr != nil {
			return nil, fmt.Errorf("decrypt auth token: %w", decErr)
		}
		authToken = decrypted
	}

	var oauthMgr *mcp.OAuthTokenManager
	var oauthCfg *mcp.OAuthConfig
	if server.AuthType == "oauth" && server.OAuthClientSecretEnc != nil && *server.OAuthClientSecretEnc != "" {
		plainSecret, decErr := crypto.DecryptString(*server.OAuthClientSecretEnc, h.EncryptionKey, mcpServerAAD(server.ID))
		if decErr != nil {
			return nil, fmt.Errorf("decrypt oauth client secret: %w", decErr)
		}
		// Use the shared manager from the transport cache when available so token
		// grants are reused across ad-hoc transports for the same server.
		if h.MCPTransportCache != nil {
			oauthMgr = h.MCPTransportCache.OAuthManager()
		} else {
			oauthMgr = mcp.NewOAuthTokenManager(nil)
		}
		oauthCfg = &mcp.OAuthConfig{
			TokenURL:     server.OAuthTokenURL,
			ServerURL:    server.URL,
			ClientID:     server.OAuthClientID,
			ClientSecret: plainSecret,
			Scopes:       server.OAuthScopes,
		}
	}

	streamIdleTimeout := h.MCPStreamIdleTimeout
	if streamIdleTimeout == 0 {
		streamIdleTimeout = 120 * time.Second
	}
	return mcp.NewHTTPTransport(server.URL, server.AuthType, server.AuthHeader, authToken, timeout, h.MCPAllowPrivateURLs,
		server.ID, oauthMgr, oauthCfg, mcpClientInfo, mcp.ResolvePinnedVersion(server.ProtocolVersion), streamIdleTimeout), nil
}

// MCPToolCallLogger logs MCP tool call events asynchronously.
// Implementations must be safe for concurrent use. Log must never block.
// The concrete implementation is usage.MCPLogger.
type MCPToolCallLogger interface {
	Log(event usage.MCPToolCallEvent)
}

// HandleMCPProxy routes a POST MCP request to either the built-in VoidLLM MCP
// server (alias "voidllm") or an external registered MCP server identified by
// the :alias path parameter.
func (h *Handler) HandleMCPProxy(c fiber.Ctx) error {
	// alias is cloned into an owned copy immediately: c.Params returns a
	// value backed by fasthttp's per-request buffer — see Fiber's own
	// "Returned value is only valid within the handler; do not store any
	// references" doc — and the genuine transparent pass-through branch
	// below uses alias, as a Prometheus label and in a usage event, from
	// inside a SendStreamWriter goroutine that outlives HandleMCPProxy's own
	// return, by which point fasthttp has already recycled that buffer (see
	// internal/proxy/handler.go's documented trap, and this function's own
	// pass-through section for the rest of what is captured the same way).
	alias := strings.Clone(c.Params("alias"))

	if alias == "voidllm" {
		return h.HandleMCP(c)
	}

	ki := auth.KeyInfoFromCtx(c)
	if ki == nil {
		return c.Status(fiber.StatusUnauthorized).JSON(
			mcp.NewErrorResponse(nil, mcp.CodeInvalidRequest, "missing authentication"))
	}

	var server *db.MCPServer
	if h.MCPServerCache != nil {
		server, _ = h.MCPServerCache.Get(alias, ki.OrgID, ki.TeamID)
	}
	if server == nil {
		var err error
		server, err = h.DB.GetMCPServerByAliasScoped(c.Context(), alias, ki.OrgID, ki.TeamID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return c.Status(fiber.StatusNotFound).JSON(
					mcp.NewErrorResponse(nil, mcp.CodeInvalidRequest, "unknown MCP server"))
			}
			h.Log.ErrorContext(c.Context(), "mcp proxy: lookup server",
				slog.String("alias", alias),
				slog.String("error", err.Error()))
			return c.Status(fiber.StatusInternalServerError).JSON(
				mcp.NewErrorResponse(nil, mcp.CodeInternalError, "internal error"))
		}
	}

	if !server.IsActive {
		return c.Status(fiber.StatusServiceUnavailable).JSON(
			mcp.NewErrorResponse(nil, mcp.CodeInternalError, "MCP server is disabled"))
	}

	// Global servers (org_id IS NULL, team_id IS NULL) require explicit access
	// control via the org/team/key MCP access tables. Org- and team-scoped
	// servers are implicitly accessible to members of that scope — their
	// visibility is already enforced by GetMCPServerByAliasScoped.
	if server.OrgID == nil && server.TeamID == nil && !auth.HasRole(ki.Role, auth.RoleSystemAdmin) {
		var allowed bool
		if h.MCPAccessCache != nil {
			allowed = h.MCPAccessCache.Check(ki.OrgID, ki.TeamID, ki.ID, server.ID)
		} else {
			var accessErr error
			allowed, accessErr = h.DB.CheckMCPAccess(c.Context(), ki.OrgID, ki.TeamID, ki.ID, server.ID)
			if accessErr != nil {
				h.Log.ErrorContext(c.Context(), "mcp proxy: check access",
					slog.String("error", accessErr.Error()))
				return c.Status(fiber.StatusInternalServerError).JSON(
					mcp.NewErrorResponse(nil, mcp.CodeInternalError, "internal error"))
			}
		}
		if !allowed {
			return c.Status(fiber.StatusForbidden).JSON(
				mcp.NewErrorResponse(nil, mcp.CodeInvalidRequest, "access denied to MCP server"))
		}
	}

	// Resolve transport from cache (avoids per-request transport creation and
	// AES-256-GCM decryption). Fall back to an ad-hoc transport on cache miss
	// (cold start or server not yet loaded into the cache).
	var transport *mcp.HTTPTransport
	var adHoc bool
	if h.MCPTransportCache != nil {
		if resolved, ok := h.MCPTransportCache.Get(server.ID); ok {
			transport = resolved.Transport
		}
	}
	if transport == nil {
		adHoc = true
		timeout := h.MCPCallTimeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		var buildErr error
		transport, buildErr = h.buildAdHocTransport(server, timeout)
		if buildErr != nil {
			h.Log.ErrorContext(c.Context(), "mcp proxy: build ad-hoc transport",
				slog.String("server", alias),
				slog.String("error", buildErr.Error()))
			return c.Status(fiber.StatusInternalServerError).JSON(
				mcp.NewErrorResponse(nil, mcp.CodeInternalError, "internal error"))
		}
	}
	// transportClosedAsync tracks whether the ad-hoc transport (if any) has
	// been handed off to the SendStreamWriter goroutine on the genuine
	// pass-through path below, which closes it itself after io.Copy
	// completes (docs/mcp-v2.md, review finding B1). When true, this defer
	// must NOT also close it: SendStreamWriter registers the writer and
	// returns immediately without waiting for the goroutine to run, so
	// HandleMCPProxy's own return — and this defer firing — can happen while
	// Forward's response body is still being read on that goroutine; closing
	// transport out from under an in-flight read would be a use-after-close
	// race. Every other return path below (early errors, 202, a redirect,
	// and the legacy-SSE-wrap path, which is fully synchronous) is
	// unaffected and closes transport, if ad-hoc, exactly as before.
	var transportClosedAsync bool
	if adHoc {
		defer func() {
			if !transportClosedAsync {
				transport.Close() //nolint:errcheck
			}
		}()
	}

	body := append([]byte{}, c.Body()...)
	if len(body) == 0 {
		return c.JSON(mcp.NewErrorResponse(nil, mcp.CodeParseError, "empty request body"))
	}

	// Cross-check the standard request headers against the body, exactly as
	// handleMCPRequest does for the built-in server (see validateMCPHeaders):
	// without this, a modern-era request with a Mcp-Method/Mcp-Name header
	// that disagrees with the body would be rejected by /mcp/voidllm but
	// silently executed by /mcp/:alias — the split-brain MCP Streamable HTTP
	// §4.5 warns intermediaries against (FIX 4).
	//
	// id is declared here, not scoped to this if, because forwardHeaders'
	// too-many-Mcp-Param-headers rejection below reuses it: both are
	// input-validation failures discovered before transport.Forward is ever
	// called, and both answer with the caller's own JSON-RPC request ID
	// (or null, per validateMCPHeaders/mcpHeaderProbe, when the body could
	// not even be parsed that far) rather than inventing a second parse of
	// body for the same field.
	id, verr := validateMCPHeaders(c, body)
	if verr != nil {
		c.Status(fiber.StatusBadRequest)
		return c.JSON(mcp.Response{JSONRPC: "2.0", ID: id, Error: verr})
	}

	meta := parseMCPRequestMeta(body)

	// downstreamAccept is consumed synchronously below (acceptsOnlySSE), well
	// before any goroutine that outlives HandleMCPProxy's own return could
	// see it, so — unlike alias — it needs no defensive clone.
	downstreamAccept := c.Get("Accept")

	start := time.Now()
	// HandleMCPProxy is a transparent intermediary, not an MCP client of its
	// own — the CALLER owns the protocol handshake, and the caller's own
	// request body is that handshake. transport.Forward reflects that: no
	// warmup of VoidLLM's own, no response interpretation — forwardHeaders
	// mirrors the caller's own Mcp-Session-Id upstream, and the upstream's
	// response Mcp-Session-Id is mirrored back to the caller below, so the
	// caller's own initialize remains the only handshake on the wire
	// (docs/mcp-v2.md, FIX 1/FIX 3).
	//
	// sessionScope is (ki.OrgID, ki.ID), not ki.OrgID alone: a caller-supplied
	// session, unlike Call's own VoidLLM-established one (see
	// mcp.SessionScope's doc), can be guessed or reused by ANY key in the same
	// organization, not only the one that actually received it — scoping
	// SessionRegistry lookups per key as well as per org means one key
	// issuing many sessionless legacy "initialize" requests can only ever
	// evict its OWN prior sessions from SessionRegistry's bounded per-scope
	// set, never a sibling key's (see mcp.NewClientSessionScope's doc).
	//
	// h.MCPSessionRegistry, not transport.Forward itself, is what checks and
	// records the caller's inbound Mcp-Session-Id these days: a store
	// anchored to this one *mcp.HTTPTransport would not survive an ad-hoc
	// transport built for a single cache-miss request (buildAdHocTransport
	// closes it right after this function returns), and gating the check on
	// the resolved binding's era rather than on whether the header is even
	// present would miss a dual-era upstream resolved as modern that still
	// accepts headerless legacy traffic at the same endpoint — see
	// mcp.SessionRegistry's own doc for the full history of both problems.
	// The check below instead gates on the one thing that is actually true
	// regardless of era: a modern request has no reason to carry
	// Mcp-Session-Id at all (docs/mcp-v2.md §2, §3.1), so a caller that does
	// deserves scrutiny no matter what this server's binding resolved to.
	//
	// known fails CLOSED, not open, when h.MCPSessionRegistry is nil: a
	// missing registry is a configuration error (production wiring in
	// internal/app always sets it — see that field's doc), and a
	// configuration error on a tenant-isolation control must break loudly,
	// not quietly relay every caller-supplied session unchecked. Treating a
	// nil registry as "known" would silently disable exactly the isolation
	// this whole mechanism exists for the moment someone refactors Handler
	// construction and forgets to wire it — tests would stay green (nothing
	// downstream panics), and the cross-tenant guessing/reuse attack this
	// check exists to stop would work again without a single failing
	// assertion anywhere. Failing closed instead means a forgotten wiring
	// breaks legacy session continuity immediately and visibly (every
	// caller-supplied session is treated as unrecognized and dropped) rather
	// than as a silent security regression.
	//
	// paramErr, when non-nil, means the caller sent more valid,
	// non-duplicate Mcp-Param-{Name} headers than mcp.MaxParamHeaders (see
	// collectMCPParamHeaders' own doc for why this is a whole-request
	// rejection, never a partial forward). HTTP 400 plus a JSON-RPC error
	// body is the same shape validateMCPHeaders' rejections above already
	// use for every other input the caller controls and this proxy cannot
	// safely relay as sent — deliberately not the spec's own
	// CodeHeaderMismatch (-32020, reserved for a header disagreeing with the
	// body), since nothing here disagrees with anything: the body is never
	// even inspected for this check. mcp.CodeTooManyParamHeaders is
	// allocated from the -32000..-32019 range the spec leaves
	// implementation-defined for exactly this kind of VoidLLM-specific
	// condition (see that constant's own doc in internal/mcp/protocol.go).
	hdr, paramErr := forwardHeaders(c, server.AuthHeader)
	if paramErr != nil {
		c.Status(fiber.StatusBadRequest)
		return c.JSON(mcp.Response{JSONRPC: "2.0", ID: id, Error: paramErr})
	}
	sessionScope := mcp.NewClientSessionScope(ki.OrgID, ki.ID)
	if sid := hdr.Get(mcp.HeaderSessionID); sid != "" {
		known := h.MCPSessionRegistry != nil && h.MCPSessionRegistry.Known(server.ID, sessionScope, sid)
		if !known {
			// Verwerfen, nicht ablehnen: drop the header rather than answer
			// with an error, which would itself leak whether sid exists for
			// some OTHER caller — turning the check into an oracle for
			// exactly the enumeration attack it exists to prevent.
			delete(hdr, mcp.HeaderSessionID)
		}
	}

	result, callErr := transport.Forward(c.Context(), body, hdr)

	if callErr != nil {
		h.recordMCPForwardOutcome(ki, alias, meta, time.Since(start), "transport_error", "call")
		h.Log.ErrorContext(c.Context(), "mcp proxy: transport error",
			slog.String("server", alias),
			slog.String("error", callErr.Error()))
		return c.Status(fiber.StatusBadGateway).JSON(
			mcp.NewErrorResponse(nil, mcp.CodeInternalError, "upstream MCP server unavailable"))
	}

	// An upstream Mcp-Session-Id longer than mcp.MaxSessionIDLength is
	// stripped from result.Header here, before anything below ever reads it
	// again — neither mirrored back to the caller nor recorded into
	// h.MCPSessionRegistry, which would silently refuse to remember it
	// anyway (see that constant's doc). Without this, the caller would
	// receive a session ID this proxy itself can never recognize, and only
	// discover the mismatch a request later, once the inbound check below
	// drops it as unrecognized and the upstream 404s — an extra, confusing
	// round trip. Deleting it here instead means the caller sees no
	// Mcp-Session-Id at all on this response, a defined state that leads it
	// to reinitialize immediately.
	if sid := result.Header.Get(mcp.HeaderSessionID); sid != "" && len(sid) > mcp.MaxSessionIDLength {
		result.Header.Del(mcp.HeaderSessionID)
	}

	// The upstream's Mcp-Session-Id, if any, is mirrored back to the caller
	// exactly as received, and — only once it has actually been mirrored —
	// recorded into h.MCPSessionRegistry. Both happen together, in
	// h.mirrorMCPResponseHeaders, below: the 202 Accepted branch and the
	// 3xx-from-upstream branch immediately below both return before ever
	// reaching that call, so neither one mirrors OR records a session — see
	// mirrorMCPResponseHeaders' own doc for why recording anywhere earlier
	// than the point that determines what the caller actually receives is
	// exactly the bug this shape avoids. No session ID is ever logged on
	// this path: a session ID is a bearer credential (docs/mcp-v2.md
	// §11.5), and this repo's zero-knowledge logging contract applies to it
	// exactly as it does to prompt content.

	// Notification — upstream returned 202 Accepted with no response body
	// expected. There is nothing to stream to the caller; result.Body, if
	// non-nil, is closed unread rather than passed to SendStreamWriter. The
	// round trip is already fully complete at this point, so the outcome is
	// recorded immediately rather than deferred — see the genuine
	// pass-through branch below for why that path defers instead
	// (docs/mcp-v2.md, review finding A1).
	if result.Status == fiber.StatusAccepted {
		if result.Body != nil {
			result.Body.Close() //nolint:errcheck // best-effort close of a body intentionally never read
		}
		h.recordMCPForwardOutcome(ki, alias, meta, time.Since(start), "success", "")
		return c.SendStatus(fiber.StatusAccepted)
	}

	// A 3xx from the upstream is never a sensible state for an MCP gateway to
	// hand its own caller: mcp.NewHTTPTransport's CheckRedirect already
	// refuses to follow one itself, so surfacing it here would only leak an
	// upstream implementation detail — a Location the caller cannot usefully
	// act on through what is supposed to be a JSON-RPC channel — instead of
	// hiding it (docs/mcp-v2.md, review finding C5). Treated exactly like any
	// other upstream/transport problem: close the body, record the failure,
	// answer 502.
	if result.Status >= 300 && result.Status < 400 {
		if result.Body != nil {
			result.Body.Close() //nolint:errcheck // best-effort close of a body intentionally never read
		}
		h.recordMCPForwardOutcome(ki, alias, meta, time.Since(start), "transport_error", "call")
		h.Log.LogAttrs(c.Context(), slog.LevelWarn, "mcp proxy: upstream returned a redirect",
			slog.String("server", alias),
			slog.Int("status", result.Status))
		return c.Status(fiber.StatusBadGateway).JSON(
			mcp.NewErrorResponse(nil, mcp.CodeInternalError, "upstream MCP server unavailable"))
	}

	// Legacy caller path (docs/mcp-v2.md, review finding A2): a caller whose
	// Accept header names text/event-stream exclusively — never also
	// application/json — never adopted the modern requirement to accept both
	// (docs/mcp-v2.md §1a, acceptsOnlySSE's doc). If the upstream answered
	// with application/json, wrap it as a single SSE "message" event exactly
	// as handleMCPRequest already does for the built-in server, instead of
	// handing such a caller a media type it cannot parse. A genuine upstream
	// SSE response (Content-Type: text/event-stream) is never touched by
	// this branch — it already IS the format such a caller expects, and is
	// passed through untouched by the genuine pass-through path below like
	// everything else.
	if acceptsOnlySSE(downstreamAccept) && strings.HasPrefix(result.Header.Get("Content-Type"), "application/json") {
		return h.sendLegacySSEWrapped(c, result, alias, meta, ki, start, server.ID, sessionScope)
	}

	// From here on this is a genuine transparent pass-through: VoidLLM does
	// not interpret the upstream's status, headers, or body — all three are
	// forwarded unchanged. This includes a modern-era
	// resultType:"input_required" (the real MCP client on the other side of
	// this proxy is the one the spec obligates to retry, docs/mcp-v2.md
	// §3.7) and any other non-2xx status the upstream chose to answer with.
	// h.mirrorMCPResponseHeaders (below) mirrors only the allowlisted
	// response headers — never Server, X-Powered-By, or any hop-by-hop
	// header — and the upstream alone decides Content-Type: there is no
	// Accept-based branch and no self-set content type on this path, unlike
	// the built-in server's handleMCPRequest (mcp_handler.go), which
	// produces its own response and must format it for callers that only
	// accept text/event-stream. It also records the caller's
	// Mcp-Session-Id, if any, into h.MCPSessionRegistry — see its own doc
	// for why that recording belongs exactly here.
	h.mirrorMCPResponseHeaders(c, result.Header, server.ID, sessionScope)
	upstreamContentType := result.Header.Get("Content-Type")
	if strings.HasPrefix(upstreamContentType, "text/event-stream") {
		// Set once, by VoidLLM, for the response VoidLLM itself sends to its
		// own caller — deliberately never mirrored from the upstream's own
		// copy of this header name (see allowedMCPResponseHeaders' doc,
		// docs/mcp-v2.md review finding C1): a reverse proxy sitting in front
		// of VoidLLM must be told not to buffer this response, or this
		// entire streaming rewrite is defeated at that hop.
		c.Set("X-Accel-Buffering", "no")
	}
	c.Status(result.Status)

	// Everything reachable through c must be captured into local variables
	// before SendStreamWriter's goroutine runs: fasthttp recycles the Fiber
	// context once Handle returns, and that goroutine outlives this return.
	// respBody is the resource the closure reads and closes (Body.Close, see
	// ForwardResult's doc, stops Forward's idle timer and cancels its
	// request context). ki, alias, and meta were already made safe above;
	// maxBytes is a plain int64 copy, safe by value. transport is handed off
	// via transportClosedAsync — see that field's doc — only when adHoc, so
	// the closure, not this function's own defer, becomes responsible for
	// closing it once io.Copy completes.
	respBody := result.Body
	logger := h.Log
	maxBytes := h.MCPStreamMaxBytes
	if adHoc {
		transportClosedAsync = true
	}

	return c.SendStreamWriter(func(w *bufio.Writer) {
		defer respBody.Close() //nolint:errcheck // Close's own error carries no actionable information here
		if adHoc {
			defer transport.Close() //nolint:errcheck
		}

		_, copyErr := io.Copy(&flushingWriter{w: w, maxBytes: maxBytes}, respBody)
		duration := time.Since(start)

		if copyErr != nil {
			// The stream ended before completion: a client disconnect, the
			// idle timeout (settings.mcp.stream_idle_timeout) firing, the
			// configured byte ceiling (settings.mcp.stream_max_bytes) being
			// exceeded, or the upstream tearing down the connection. This is
			// spec-sanctioned, not a bug to paper over — MCP Streamable HTTP
			// §3.9 (docs/mcp-v2.md) requires the caller to retry as an
			// entirely new request with a new ID, so nothing synthetic is
			// written here, unlike the LLM proxy's abort event. copyErr is a
			// transport-level Go error (network, context, or
			// errStreamMaxBytesExceeded), never upstream response content, so
			// logging it — and, for the byte-limit case, the numeric limit
			// itself — does not violate the zero-knowledge proxy contract
			// (docs/mcp-v2.md §11.5): no streamed bytes, prompt content, tool
			// arguments, or upstream error text are ever logged here.
			//
			// This is also the fix for review finding A1: before this, the
			// call-level metrics and usage event below were recorded
			// eagerly, right after Forward returned response headers — long
			// before io.Copy, and therefore the actual outcome of the
			// stream, was known. A stream that ran for minutes and then
			// broke was recorded as an instant success. Recording here,
			// after io.Copy returns, uses the real duration and the real
			// outcome instead.
			if errors.Is(copyErr, errStreamMaxBytesExceeded) {
				logger.LogAttrs(context.Background(), slog.LevelDebug, "mcp proxy: stream ended before completion",
					slog.String("server", alias),
					slog.String("error", copyErr.Error()),
					slog.Int64("max_bytes", maxBytes))
			} else {
				logger.LogAttrs(context.Background(), slog.LevelDebug, "mcp proxy: stream ended before completion",
					slog.String("server", alias),
					slog.String("error", copyErr.Error()))
			}
			h.recordMCPForwardOutcome(ki, alias, meta, duration, "transport_error", "stream")
			return
		}
		h.recordMCPForwardOutcome(ki, alias, meta, duration, "success", "")
	})
}

// mirrorMCPResponseHeaders mirrors header's allowlisted fields onto c's
// outgoing response via copyMCPResponseHeaders, then — only for whatever
// Mcp-Session-Id value c's own response now actually carries, read back off
// c itself rather than re-derived a second time from header — records it
// into h.MCPSessionRegistry.
//
// This is deliberately the ONLY place HandleMCPProxy (directly, or via
// sendLegacySSEWrapped) ever records a session. Recording anywhere upstream
// of this call — straight off result.Header, before this function decides
// what actually gets mirrored — used to let a session be entered into the
// registry on the 202 Accepted and 3xx-from-upstream branches, even though
// neither of those branches ever mirrors a response header to the caller at
// all: the caller received no Mcp-Session-Id, yet the registry remembered
// one anyway, sitting there unreachable by any legitimate follow-up request
// until it aged out or evicted a real session (SessionRegistry's own doc).
// Deriving what to record from c's own outgoing response, instead of
// re-checking header's hop-by-hop Connection options a second time, is what
// keeps this guarantee true regardless of how copyMCPResponseHeaders' own
// filtering evolves: whatever this method records is, by construction,
// exactly what the caller is about to see on the wire — never more.
//
// No session ID is ever logged: a session ID is a bearer credential
// (docs/mcp-v2.md §11.5), and this repo's zero-knowledge logging contract
// applies to it exactly as it does to prompt content.
func (h *Handler) mirrorMCPResponseHeaders(c fiber.Ctx, header http.Header, serverID string, sessionScope mcp.SessionScope) {
	copyMCPResponseHeaders(c, header)
	if h.MCPSessionRegistry == nil {
		return
	}
	if sid := string(c.Response().Header.Peek(mcp.HeaderSessionID)); sid != "" {
		h.MCPSessionRegistry.Record(serverID, sessionScope, sid)
	}
}

// legacySSEWrapReadCap is the hard ceiling sendLegacySSEWrapped enforces on
// its upstream body read, regardless of settings.mcp.stream_max_bytes
// (h.MCPStreamMaxBytes) — including when that setting is explicitly 0,
// which means "unbounded" everywhere else on this proxy
// (config.MCPConfig.StreamMaxBytes' doc). This branch needs its own,
// independent ceiling because it cannot inherit "unbounded" the way the
// genuine transparent pass-through path can: that path's flushingWriter
// bounds a STREAM as bytes flow through, byte-for-byte, without ever holding
// the whole body in memory at once, so "0" genuinely costs nothing extra
// there. This branch exists specifically because its caller cannot consume a
// stream (see acceptsOnlySSE) — it must read result.Body to completion
// before it can wrap it as a single SSE "message" event, so an operator who
// sets stream_max_bytes to 0 for the streaming path would otherwise turn
// this one into an unbounded io.ReadAll, and therefore an unbounded
// allocation, for every legacy caller that happens to hit it (docs/mcp-v2.md,
// Punkt C). 10 MiB matches the buffered rawPost path's own hard cap
// (http_transport.go) for the same underlying reason — both are
// fully-buffered reads of a response no caller can meaningfully cap via
// configuration alone. When stream_max_bytes IS set below this cap, the
// smaller of the two still wins (see effectiveMaxBytes below): this constant
// is a ceiling on top of the configured value, never a floor that raises it.
const legacySSEWrapReadCap int64 = 10 << 20 // 10 MiB

// sendLegacySSEWrapped reads result's upstream application/json body — up to
// the smaller of h.MCPStreamMaxBytes (settings.mcp.stream_max_bytes, the same
// ceiling the genuine transparent pass-through path enforces via
// flushingWriter) and legacySSEWrapReadCap, which additionally applies
// unconditionally on this branch even when stream_max_bytes is explicitly 0
// ("unbounded" everywhere else — see legacySSEWrapReadCap's doc for why this
// branch cannot honor that) — and sends it to the caller as a single SSE
// "message" event via formatSSEMessage, exactly as handleMCPRequest already
// does for the built-in server. It is reached only for a legacy caller (see
// acceptsOnlySSE) whose Accept header cannot parse the upstream's
// application/json response as sent (docs/mcp-v2.md, review finding A2).
// Before the fix that introduced h.MCPStreamMaxBytes here, the read was
// bounded by a fixed, unconfigurable 10 MiB constant instead: an operator
// who set stream_max_bytes lower — 1 KiB, say — had that ceiling silently
// bypassed on exactly this branch (docs/mcp-v2.md, review finding A1).
//
// Unlike the genuine transparent pass-through path in HandleMCPProxy, this is
// fully synchronous: the whole (bounded) body is read and the response sent
// before this function returns, so it needs none of that path's
// goroutine-lifetime handling — result.Body, and, for an ad-hoc transport,
// transport itself, are both still safe to close via HandleMCPProxy's own
// ordinary, synchronous defer (transportClosedAsync is never set on this
// path).
//
// The outcome recorded via recordMCPForwardOutcome is only ever "success" or
// the upstream-read failure handled above ("transport_error"/"call"); there
// is no separate write-failure outcome here. Fiber's SendString sets the
// response body on the in-memory fasthttp.Response and always returns nil —
// the actual socket write happens only after this handler returns, so a
// client disconnect mid-write is not observable on this synchronous path.
// That is acceptable specifically because this branch, unlike the genuine
// pass-through path (whose io.Copy inside SendStreamWriter does see real
// socket errors, per review finding A1), only ever hands Fiber a single,
// already fully read body bounded by effectiveMaxBytes: "success" here means
// VoidLLM handed a complete body to Fiber, not that the caller received it.
//
// serverID and sessionScope are passed through unchanged from HandleMCPProxy
// so this function's own call to h.mirrorMCPResponseHeaders below can record
// the caller's Mcp-Session-Id (if the upstream sent one and it is actually
// mirrored) into h.MCPSessionRegistry — see that method's doc for why
// recording happens there and nowhere earlier.
func (h *Handler) sendLegacySSEWrapped(c fiber.Ctx, result *mcp.ForwardResult, alias string, meta mcpRequestMeta, ki *auth.KeyInfo, start time.Time, serverID string, sessionScope mcp.SessionScope) error {
	defer result.Body.Close() //nolint:errcheck // best-effort close; the body has already been fully consumed or the read failed

	// effectiveMaxBytes is the smaller of the two ceilings this branch
	// enforces (see legacySSEWrapReadCap's doc): the operator-configured
	// h.MCPStreamMaxBytes, when set below the hard cap, still wins — this
	// cap only ever tightens an unbounded or overly generous configuration,
	// it never loosens a stricter one.
	effectiveMaxBytes := legacySSEWrapReadCap
	if maxBytes := h.MCPStreamMaxBytes; maxBytes > 0 && maxBytes < legacySSEWrapReadCap {
		effectiveMaxBytes = maxBytes
	}
	// The limit reader is deliberately given effectiveMaxBytes+1, one byte
	// more than the ceiling this branch actually enforces: reading exactly
	// effectiveMaxBytes would make a response that fits EXACTLY at the limit
	// indistinguishable from one that was truncated at it — both would come
	// back as a len(respBody) == effectiveMaxBytes read with no error. The
	// extra byte lets the length check below tell the two apart: len ==
	// effectiveMaxBytes+1 only happens when the upstream body was actually
	// longer than the limit. Do not "simplify" this back down to
	// effectiveMaxBytes; that reintroduces exactly this ambiguity.
	respBody, readErr := io.ReadAll(io.LimitReader(result.Body, effectiveMaxBytes+1))
	if readErr != nil || int64(len(respBody)) > effectiveMaxBytes {
		h.recordMCPForwardOutcome(ki, alias, meta, time.Since(start), "transport_error", "call")
		h.Log.LogAttrs(c.Context(), slog.LevelWarn, "mcp proxy: legacy SSE wrap read failed",
			slog.String("server", alias))
		return c.Status(fiber.StatusBadGateway).JSON(
			mcp.NewErrorResponse(nil, mcp.CodeInternalError, "upstream MCP server unavailable"))
	}

	// h.mirrorMCPResponseHeaders no longer ever mirrors Content-Encoding (it
	// was dropped from allowedMCPResponseHeaders as part of docs/mcp-v2.md
	// review finding B), so — unlike before this fix — there is nothing here
	// left to strip: formatSSEMessage's plain-text "data:" re-framing below
	// can never be mislabeled with a stale encoding the upstream's original
	// bytes may have carried. It also records the caller's Mcp-Session-Id,
	// if any, into h.MCPSessionRegistry — see its own doc.
	h.mirrorMCPResponseHeaders(c, result.Header, serverID, sessionScope)
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("X-Accel-Buffering", "no")
	c.Status(result.Status)

	// c.SendString's error return is checked on principle, not because it is
	// reachable today: Fiber's SendString only sets the in-memory response
	// body and always returns nil (see the doc comment above), so this err
	// is never actually non-nil. There is deliberately no dedicated
	// "transport_error"/"stream" outcome for it, unlike the genuine
	// pass-through path — that classification exists there because io.Copy
	// can see a real, distinguishable socket failure; here it never applies.
	if err := c.SendString(formatSSEMessage(respBody)); err != nil {
		return fmt.Errorf("mcp proxy: send legacy SSE-wrapped response: %w", err)
	}
	h.recordMCPForwardOutcome(ki, alias, meta, time.Since(start), "success", "")
	return nil
}

// recordMCPForwardOutcome finalises the metrics and usage-event bookkeeping
// for a single HandleMCPProxy call to an external MCP server: the
// server/tool/status Prometheus counters, the call-duration histogram, and
// (when MCPLogger is configured) one usage.MCPToolCallEvent.
//
// errorType, when non-empty, additionally increments MCPTransportErrorsTotal
// under that error_type label — "call" for a failure discovered before any
// response reached the caller (Forward itself returned an error, or the
// upstream answered with a redirect this gateway refuses to follow), and
// "stream" for one discovered only after the response had already started
// streaming to the caller: a mid-stream break, the idle timeout, or the
// configured byte ceiling being exceeded (docs/mcp-v2.md, review finding A1).
//
// ki, alias, and meta must already be values safely captured out of the
// Fiber request: this is called both synchronously, while HandleMCPProxy or
// sendLegacySSEWrapped is still executing, and from the SendStreamWriter
// goroutine HandleMCPProxy launches on the genuine pass-through path, which
// outlives HandleMCPProxy's own return — see that call site's own comments
// for how each value is made safe to use there.
func (h *Handler) recordMCPForwardOutcome(ki *auth.KeyInfo, alias string, meta mcpRequestMeta, duration time.Duration, status, errorType string) {
	if errorType != "" {
		metrics.MCPTransportErrorsTotal.WithLabelValues(alias, errorType).Inc()
	}
	metricsMethod := meta.MetricsMethod()
	metrics.MCPToolCallsTotal.WithLabelValues(alias, metricsMethod, status).Inc()
	metrics.MCPToolCallDurationSeconds.WithLabelValues(alias, metricsMethod).Observe(duration.Seconds())

	if h.MCPLogger != nil {
		h.MCPLogger.Log(usage.MCPToolCallEvent{
			KeyID:            ki.ID,
			KeyType:          ki.KeyType,
			OrgID:            ki.OrgID,
			TeamID:           ki.TeamID,
			UserID:           ki.UserID,
			ServiceAccountID: ki.ServiceAccountID,
			ServerAlias:      alias,
			ToolName:         meta.ToolName,
			DurationMS:       int(duration.Milliseconds()),
			Status:           status,
		})
	}
}

// errStreamMaxBytesExceeded is returned by flushingWriter.Write once
// settings.mcp.stream_max_bytes (h.MCPStreamMaxBytes) is exceeded on the
// genuine pass-through path, stopping io.Copy exactly as any other
// transport-level error does there (see HandleMCPProxy's SendStreamWriter
// closure): a malicious or misbehaving upstream must not be able to push
// unbounded data through VoidLLM, nor hold the stream open indefinitely by
// trickling bytes just under settings.mcp.stream_idle_timeout
// (docs/mcp-v2.md, review finding D). It carries no upstream content — only
// the byte-count classification the zero-knowledge contract
// (docs/mcp-v2.md §11.5) allows is ever logged alongside it.
var errStreamMaxBytesExceeded = errors.New("mcp proxy: stream exceeded configured byte limit")

// flushingWriter wraps a *bufio.Writer so that io.Copy flushes after every
// chunk it writes, instead of letting w's own internal buffer decide when to
// flush. Without this, io.Copy's default copy buffer would let SSE events —
// including progress notifications and subscriptions/listen notifications,
// the entire point of this streaming rewrite — sit in w unflushed until
// enough of them accumulated to fill it, stalling delivery indefinitely on a
// server that sends data slowly.
//
// maxBytes, when non-zero, is the settings.mcp.stream_max_bytes ceiling
// (h.MCPStreamMaxBytes): once the running total of bytes written would
// exceed it, Write returns errStreamMaxBytesExceeded instead of writing
// anything more. Zero means unbounded. A pointer receiver is required — the
// written counter must survive across the many Write calls a single io.Copy
// makes into the same flushingWriter.
type flushingWriter struct {
	w        *bufio.Writer
	maxBytes int64
	written  int64
}

// Write implements io.Writer.
func (f *flushingWriter) Write(p []byte) (int, error) {
	if f.maxBytes > 0 && f.written+int64(len(p)) > f.maxBytes {
		return 0, errStreamMaxBytesExceeded
	}
	n, err := f.w.Write(p)
	f.written += int64(n)
	if err != nil {
		return n, err
	}
	if err := f.w.Flush(); err != nil {
		return n, err
	}
	return n, nil
}

// HandleMCPProxySSE handles GET requests for the MCP SSE transport.
// For the built-in "voidllm" alias it opens a persistent SSE stream.
// For external MCP servers, SSE streaming requires persistent connections
// that are not yet supported and returns 501 Not Implemented.
func (h *Handler) HandleMCPProxySSE(c fiber.Ctx) error {
	alias := c.Params("alias")

	if alias == "voidllm" {
		return h.HandleMCPSSE(c)
	}

	return c.Status(fiber.StatusNotImplemented).JSON(
		mcp.NewErrorResponse(nil, mcp.CodeInternalError,
			"SSE streaming is not supported for external MCP servers"))
}

// validMCPMethods is the set of standard MCP JSON-RPC method names. Only these
// values are allowed as Prometheus label values to prevent cardinality explosion
// from arbitrary user-controlled method strings.
var validMCPMethods = map[string]bool{
	"initialize":                true,
	"notifications/initialized": true,
	"ping":                      true,
	"tools/list":                true,
	"tools/call":                true,
	"resources/list":            true,
	"resources/read":            true,
	"prompts/list":              true,
	"prompts/get":               true,
	// Modern-era (2026-07-28) methods — see docs/mcp-v2.md §3.3, §3.4.
	"server/discover":      true,
	"subscriptions/listen": true,
}

// mcpRequestMeta holds the parsed metadata from a single MCP JSON-RPC request
// body. It is produced once per request by parseMCPRequestMeta and consumed by
// logging, metrics, and session logic, avoiding three separate body parses.
type mcpRequestMeta struct {
	// Method is the raw JSON-RPC method string (e.g. "tools/call").
	Method string
	// ToolName is a usage-log-safe identifier: the tools/call tool name
	// (truncated to 64 bytes) for tools/call requests, the method name for
	// other known MCP methods, or "unknown" otherwise. May contain
	// user-controlled data — must NOT be used as a Prometheus label.
	ToolName string
}

// MetricsMethod returns a Prometheus-safe label value derived from the parsed
// method. Only values present in validMCPMethods are returned as-is; everything
// else (including unknown methods from user input) maps to "unknown", preventing
// cardinality explosion in Prometheus metrics.
func (m mcpRequestMeta) MetricsMethod() string {
	if validMCPMethods[m.Method] {
		return m.Method
	}
	return "unknown"
}

// parseMCPRequestMeta parses the JSON-RPC method and params.name fields from
// body in a single pass and returns an mcpRequestMeta. It is the sole body-
// parse for all metadata needs in HandleMCPProxy.
func parseMCPRequestMeta(body []byte) mcpRequestMeta {
	var req struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if jsonx.Unmarshal(body, &req) != nil {
		return mcpRequestMeta{Method: "unknown", ToolName: "unknown"}
	}

	meta := mcpRequestMeta{
		Method: req.Method,
	}

	if req.Method == "tools/call" && req.Params.Name != "" {
		name := req.Params.Name
		if len(name) > 64 {
			name = name[:64]
		}
		meta.ToolName = name
	} else if validMCPMethods[req.Method] {
		meta.ToolName = req.Method
	} else {
		meta.ToolName = "unknown"
	}

	return meta
}

// mcpServerAAD returns the additional authenticated data used when
// encrypting and decrypting MCP server auth tokens. Binding the AAD to the
// server ID prevents a ciphertext from one server being replayed for another.
func mcpServerAAD(serverID string) []byte {
	return []byte("mcp_server:" + serverID)
}

// buildToolCallRequest serialises a JSON-RPC tools/call request body for the
// given tool name and argument object. The error path of jsonx.Marshal is
// unreachable for the static structure used here; the result is always valid.
func buildToolCallRequest(toolName string, args jsonx.RawMessage) []byte {
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	}
	b, _ := jsonx.Marshal(req)
	return b
}

// CallMCPTool executes a single MCP tool call against an upstream server on
// behalf of the given caller identity. It performs the same server lookup,
// access control, credential decryption, session management, metrics recording,
// and usage logging as HandleMCPProxy. codeMode should be true when the call
// originates from a Code Mode execution. executionID is the UUIDv7 that groups
// all tool calls from a single execute_code invocation; pass an empty string
// for non-Code-Mode calls.
func (h *Handler) CallMCPTool(ctx context.Context, ki *auth.KeyInfo, serverAlias, toolName string, args jsonx.RawMessage, codeMode bool, executionID string) (jsonx.RawMessage, error) {
	// Built-in VoidLLM management server — dispatch in-process instead of HTTP.
	if serverAlias == "voidllm" && h.MCPServer != nil {
		return h.callBuiltinTool(ctx, ki, toolName, args)
	}

	var server *db.MCPServer
	if h.MCPServerCache != nil {
		server, _ = h.MCPServerCache.Get(serverAlias, ki.OrgID, ki.TeamID)
	}
	if server == nil {
		var err error
		server, err = h.DB.GetMCPServerByAliasScoped(ctx, serverAlias, ki.OrgID, ki.TeamID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, fmt.Errorf("CallMCPTool %s: unknown MCP server", serverAlias)
			}
			return nil, fmt.Errorf("CallMCPTool %s: lookup: %w", serverAlias, err)
		}
	}

	if !server.IsActive {
		return nil, fmt.Errorf("CallMCPTool %s: server is disabled", serverAlias)
	}

	// Global servers require explicit access control via the access tables.
	// System admins bypass this check — they have unrestricted access.
	if server.OrgID == nil && server.TeamID == nil && !auth.HasRole(ki.Role, auth.RoleSystemAdmin) {
		var allowed bool
		if h.MCPAccessCache != nil {
			allowed = h.MCPAccessCache.Check(ki.OrgID, ki.TeamID, ki.ID, server.ID)
		} else {
			var accessErr error
			allowed, accessErr = h.DB.CheckMCPAccess(ctx, ki.OrgID, ki.TeamID, ki.ID, server.ID)
			if accessErr != nil {
				return nil, fmt.Errorf("CallMCPTool %s: check access: %w", serverAlias, accessErr)
			}
		}
		if !allowed {
			return nil, fmt.Errorf("CallMCPTool %s: access denied", serverAlias)
		}
	}

	var transport *mcp.HTTPTransport
	var adHocTool bool
	if h.MCPTransportCache != nil {
		if resolved, ok := h.MCPTransportCache.Get(server.ID); ok {
			transport = resolved.Transport
		}
	}
	if transport == nil {
		adHocTool = true
		timeout := h.MCPCallTimeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		var buildErr error
		transport, buildErr = h.buildAdHocTransport(server, timeout)
		if buildErr != nil {
			return nil, fmt.Errorf("CallMCPTool %s: build transport: %w", serverAlias, buildErr)
		}
	}
	if adHocTool {
		defer transport.Close() //nolint:errcheck
	}

	body := buildToolCallRequest(toolName, args)

	// Mirror the tool's validated x-mcp-header bindings (MCP 2026-07-28 §4.3)
	// onto the outbound request. This is set unconditionally, regardless of
	// which era transport resolves to — see CallRequest.HeaderParams' own doc
	// for why there is deliberately no `if era == modern` branch here: only
	// dialect2026Client.Prepare actually renders these as Mcp-Param-{Name}
	// headers, legacyClientDialect.Prepare ignores the field entirely, and
	// which of those two applies is resolved inside transport.Call, never
	// here.
	//
	// h.ToolCache may be nil (Code Mode, and therefore the tool cache, is
	// optional) — guarded the same way every other h.ToolCache access in this
	// package is. On the Code Mode path this lookup is warm almost every
	// time: the same request has typically already called GetTools moments
	// earlier for this serverID (see ToolCache.HeaderParams' own doc).
	var headerParams []mcp.HeaderParam
	if h.ToolCache != nil {
		hp, hpErr := h.ToolCache.HeaderParams(ctx, server.ID, toolName)
		if hpErr != nil {
			// Mirroring is additive: the JSON-RPC body remains authoritative
			// for the call regardless of whether headers were mirrored onto
			// it, so a failed schema fetch must never fail an otherwise valid
			// tools/call — only warn and proceed without header mirroring.
			// Never log hpErr's text here: HeaderParams' only error source is
			// ListTools, which has already reduced any upstream JSON-RPC
			// error down to its numeric code (see ListTools' doc) before this
			// function could see it, but even that reduced detail is not
			// logged — server alias and tool name are enough to diagnose
			// from VoidLLM's own side.
			h.Log.LogAttrs(ctx, slog.LevelWarn, "mcp: header param lookup failed, calling without header mirroring",
				slog.String("server_alias", serverAlias),
				slog.String("tool_name", toolName),
			)
		} else {
			headerParams = hp
		}
	}

	start := time.Now()
	// Session/handshake management, if the resolved protocol era needs any,
	// happens transparently inside Call — see internal/mcp/http_transport.go.
	// The caller's org ID scopes any legacy session this call establishes so
	// it is never shared with another org that has access to the same global
	// server (see mcp.SessionScope).
	callResult, callErr := transport.Call(ctx, &mcp.CallRequest{Raw: body, HeaderParams: headerParams}, mcp.SessionScope(ki.OrgID))

	duration := time.Since(start)

	status := "success"
	if callErr != nil {
		status = "transport_error"
		metrics.MCPTransportErrorsTotal.WithLabelValues(serverAlias, "call").Inc()
	}
	metrics.MCPToolCallsTotal.WithLabelValues(serverAlias, "tools/call", status).Inc()
	metrics.MCPToolCallDurationSeconds.WithLabelValues(serverAlias, "tools/call").Observe(duration.Seconds())

	if h.MCPLogger != nil {
		h.MCPLogger.Log(usage.MCPToolCallEvent{
			KeyID:               ki.ID,
			KeyType:             ki.KeyType,
			OrgID:               ki.OrgID,
			TeamID:              ki.TeamID,
			UserID:              ki.UserID,
			ServiceAccountID:    ki.ServiceAccountID,
			ServerAlias:         serverAlias,
			ToolName:            toolName,
			DurationMS:          int(duration.Milliseconds()),
			Status:              status,
			CodeMode:            codeMode,
			CodeModeExecutionID: executionID,
		})
	}

	if callErr != nil {
		return nil, fmt.Errorf("CallMCPTool %s/%s: transport: %w", serverAlias, toolName, callErr)
	}

	return callResult.Body, nil
}

// callBuiltinTool dispatches a tool call to the built-in VoidLLM management
// MCP server in-process, without HTTP. The caller's identity is injected into
// the MCP context so tool handlers can enforce RBAC.
func (h *Handler) callBuiltinTool(ctx context.Context, ki *auth.KeyInfo, toolName string, args jsonx.RawMessage) (jsonx.RawMessage, error) {
	mcpCtx := mcp.WithKeyIdentity(ctx, mcp.KeyIdentity{
		OrgID:  ki.OrgID,
		TeamID: ki.TeamID,
		KeyID:  ki.ID,
		UserID: ki.UserID,
		Role:   ki.Role,
	})
	body := buildToolCallRequest(toolName, args)
	// No real transport headers exist for an in-process call: mcp.MapHeader
	// is empty, so Negotiate falls back to the oldest legacy version. That is
	// harmless here — buildToolCallRequest always emits a plain "tools/call"
	// request, whose handling never branches on protocol version.
	result := h.MCPServer.Handle(mcpCtx, body, mcp.MapHeader{})
	if result.Body == nil {
		return nil, fmt.Errorf("builtin tool %s: no response", toolName)
	}
	return result.Body, nil
}

// MakeToolFetcher returns a ToolFetcher that retrieves the tool listing from
// the upstream MCP server identified by serverID. It creates a fresh
// HTTPTransport, sends tools/list (or, for a legacy upstream, initialize +
// tools/list), and returns the parsed result unchanged — HTTPTransport.ListTools
// already applies FilterHeaderParamTools, so there is nothing further for this
// fetcher to do to the listing itself. The lookup uses GetMCPServer (by
// database ID) so that org-scoped, team-scoped, and global servers are all
// resolved without ambiguity. Access control is enforced separately at the
// call layer; the fetcher only reads URL and auth config.
func (h *Handler) MakeToolFetcher() mcp.ToolFetcher {
	return func(ctx context.Context, serverID string) (*mcp.ToolListing, error) {
		server, err := h.DB.GetMCPServer(ctx, serverID)
		if err != nil {
			return nil, fmt.Errorf("tool fetcher %s: lookup: %w", serverID, err)
		}

		var transport *mcp.HTTPTransport
		var adHocFetch bool
		if h.MCPTransportCache != nil {
			if resolved, ok := h.MCPTransportCache.Get(server.ID); ok {
				transport = resolved.Transport
			}
		}
		if transport == nil {
			adHocFetch = true
			timeout := h.MCPCallTimeout
			if timeout == 0 {
				timeout = 30 * time.Second
			}
			var buildErr error
			transport, buildErr = h.buildAdHocTransport(server, timeout)
			if buildErr != nil {
				return nil, fmt.Errorf("tool fetcher %s: build transport: %w", serverID, buildErr)
			}
		}
		if adHocFetch {
			defer transport.Close() //nolint:errcheck
		}

		listing, err := transport.ListTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("tool fetcher %s: list tools: %w", serverID, err)
		}
		return listing, nil
	}
}
