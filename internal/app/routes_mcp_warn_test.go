package app

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers warnIfMCPWriteTimeoutFinite (routes.go): a startup WARN,
// emitted at most once per call, when the MCP gateway is active and
// write_timeout is finite — fasthttp's WriteTimeout is a single absolute
// per-socket write deadline never refreshed by a successful SSE flush, so a
// long-lived subscriptions/listen stream is killed once it elapses no matter
// how healthy the traffic still flowing is (see the function's own doc).

// newMCPWarnTestApp builds a minimal *Application with just the fields
// warnIfMCPWriteTimeoutFinite reads: log, and adminHandler.MCPServer (the
// same gate admin.RegisterRoutes uses to decide whether the MCP proxy routes
// are mounted at all). mcpActive controls whether adminHandler.MCPServer is
// non-nil.
func newMCPWarnTestApp(t *testing.T, mcpActive bool) (*Application, *syncBuf) {
	t.Helper()
	buf := &syncBuf{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	handler := &admin.Handler{}
	if mcpActive {
		handler.MCPServer = mcp.NewServer("voidllm", "test")
	}

	return &Application{
		log:          logger,
		adminHandler: handler,
	}, buf
}

// TestWarnIfMCPWriteTimeoutFinite_FiniteAndMCPActive_WarnsOnceWithValue
// verifies the WARN fires exactly once and includes the concrete
// write_timeout value when MCP is active and write_timeout is finite.
func TestWarnIfMCPWriteTimeoutFinite_FiniteAndMCPActive_WarnsOnceWithValue(t *testing.T) {
	t.Parallel()

	const writeTimeout = 90 * time.Second
	a, buf := newMCPWarnTestApp(t, true)

	a.warnIfMCPWriteTimeoutFinite(writeTimeout)

	got := buf.String()
	if !strings.Contains(got, "MCP gateway is active with a finite write_timeout") {
		t.Fatalf("expected warn log, got: %q", got)
	}
	if !strings.Contains(got, "1m30s") && !strings.Contains(got, writeTimeout.String()) {
		t.Errorf("expected the log to include the concrete write_timeout value %v, got: %q", writeTimeout, got)
	}

	// Exactly once: a second call is a distinct invocation (as
	// setupRoutes would only ever make once per hosting app in
	// production), but within a single call the log line must appear
	// exactly once.
	if n := strings.Count(got, "MCP gateway is active with a finite write_timeout"); n != 1 {
		t.Errorf("warn line appears %d times, want exactly 1; log: %q", n, got)
	}
}

// TestWarnIfMCPWriteTimeoutFinite_ZeroWriteTimeout_NoWarn verifies that an
// explicitly unlimited write_timeout (0, or negative) never warns, regardless
// of whether MCP is active.
func TestWarnIfMCPWriteTimeoutFinite_ZeroWriteTimeout_NoWarn(t *testing.T) {
	t.Parallel()

	a, buf := newMCPWarnTestApp(t, true)

	a.warnIfMCPWriteTimeoutFinite(0)

	if got := buf.String(); got != "" {
		t.Errorf("expected no log for write_timeout=0, got: %q", got)
	}
}

// TestWarnIfMCPWriteTimeoutFinite_MCPInactive_NoWarn verifies that a finite
// write_timeout never warns when the MCP gateway itself is not active
// (adminHandler.MCPServer == nil) — the same gate admin.RegisterRoutes uses
// to decide whether to mount the MCP proxy routes at all.
func TestWarnIfMCPWriteTimeoutFinite_MCPInactive_NoWarn(t *testing.T) {
	t.Parallel()

	a, buf := newMCPWarnTestApp(t, false)

	a.warnIfMCPWriteTimeoutFinite(90 * time.Second)

	if got := buf.String(); got != "" {
		t.Errorf("expected no log when MCP is inactive, got: %q", got)
	}
}
