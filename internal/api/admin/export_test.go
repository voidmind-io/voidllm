package admin

// SanitizeOriginForLog exposes the internal sanitizeOriginForLog function for
// white-box testing from the admin_test package.
var SanitizeOriginForLog = sanitizeOriginForLog

// MaxLoggedOriginLen exposes the internal maxLoggedOriginLen constant for
// white-box testing from the admin_test package.
const MaxLoggedOriginLen = maxLoggedOriginLen

// AcceptsSSE exposes the internal acceptsSSE function for white-box testing
// from the admin_test package — the Accept-header parser that decides
// whether the BUILT-IN MCP server's own response (mcp_handler.go's
// handleMCPRequest) gets wrapped via formatSSEMessage. See its own doc for
// why HandleMCPProxy's external-server path no longer has an equivalent
// branch at all.
var AcceptsSSE = acceptsSSE

// AcceptsOnlySSE exposes the internal acceptsOnlySSE function for white-box
// testing from the admin_test package — the Accept-header parser
// HandleMCPProxy (mcp_proxy.go) uses to decide whether an upstream
// application/json response must be wrapped as a legacy SSE "message" event
// via sendLegacySSEWrapped. See its own doc in mcp_handler.go.
var AcceptsOnlySSE = acceptsOnlySSE

// MCPServerAAD exposes the internal mcpServerAAD function for white-box
// testing from the admin_test package — tests that simulate a pre-existing
// MCP server registration by writing directly to the database (bypassing the
// Admin API's own auth_header reservation check, see isReservedMCPHeader)
// need this to compute the same AES-256-GCM additional authenticated data
// buildAdHocTransport later decrypts AuthTokenEnc against, without
// duplicating the "mcp_server:" + serverID format here.
var MCPServerAAD = mcpServerAAD

// ReservedMCPParamHeaderPrefix exposes the internal reservedMCPParamHeaderPrefix
// constant for white-box testing from the admin_test package — the one
// remaining place mcp.HeaderParamPrefix's value is duplicated (lowercased),
// so a test can assert the two never drift apart independently of either
// package's own internal tests.
const ReservedMCPParamHeaderPrefix = reservedMCPParamHeaderPrefix

// FormatSSEMessage exposes the internal formatSSEMessage function for
// white-box testing from the admin_test package — the function
// mcp_handler.go's handleMCPRequest calls to wrap the BUILT-IN MCP server's
// own JSON-RPC response as a Server-Sent Events message. mcp_proxy.go's
// HandleMCPProxy no longer calls this: the streaming rewrite made the
// external-server proxy path a byte-identical pass-through that never
// reformats an upstream's response (see mcp.HTTPTransport.Forward's doc).
var FormatSSEMessage = formatSSEMessage
