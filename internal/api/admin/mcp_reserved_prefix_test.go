package admin_test

import (
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// TestReservedMCPParamHeaderPrefix_MatchesHeaderParamPrefix guards the one
// remaining duplicated definition between internal/mcp and
// internal/api/admin: mcp.HeaderParamPrefix ("Mcp-Param-", the prefix a
// server-side x-mcp-header binding is rendered onto, and the prefix
// collectMCPParamHeaders forwards on the transparent proxy path) and
// admin's own reservedMCPParamHeaderPrefix (the lowercased prefix
// isReservedMCPHeader uses to keep a registered server's own auth_header
// from ever colliding with the Mcp-Param-* family). If these two constants
// are ever edited independently — e.g. mcp.HeaderParamPrefix renamed to
// "Mcp-Header-" without the matching admin-side edit — auth_header
// registration would stop rejecting the very header family it exists to
// protect, and TestCollectMCPParamHeaders_AuthHeaderCollision_CredentialPreserved's
// scenario (mcp_proxy_param_headers_test.go) could no longer be relied upon
// for any server registered going forward.
func TestReservedMCPParamHeaderPrefix_MatchesHeaderParamPrefix(t *testing.T) {
	t.Parallel()

	want := strings.ToLower(mcp.HeaderParamPrefix)
	if admin.ReservedMCPParamHeaderPrefix != want {
		t.Errorf("admin.ReservedMCPParamHeaderPrefix = %q, want %q (strings.ToLower(mcp.HeaderParamPrefix))",
			admin.ReservedMCPParamHeaderPrefix, want)
	}
}
