package proxy

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
)

// newTransportCacheTestDB opens a real, migrated in-memory SQLite database
// for MCPTransportCache tests. Each test gets its own private DSN so tests
// can run in parallel without sharing state.
func newTransportCacheTestDB(t *testing.T, dsn string) *db.DB {
	t.Helper()

	ctx := context.Background()
	database, err := db.Open(ctx, config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             dsn,
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return database
}

// TestMCPTransportCache_LoadAll_ProtocolVersionChange_RebuildsTransport is
// the regression test for the fixed LoadAll bug: previously ProtocolVersion
// was absent from the reuse-comparison, so changing ONLY a server's pinned
// mcp_servers.protocol_version left the OLD transport — built with the OLD
// pin baked into its HTTPTransport.pinnedVersion field at construction time —
// cached and in use until the process restarted. An admin who pinned a
// server to a different era via the Admin API would see no effect at all
// until a restart.
func TestMCPTransportCache_LoadAll_ProtocolVersionChange_RebuildsTransport(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := newTransportCacheTestDB(t, "file:TestMCPTransportCache_ProtocolVersionChange?mode=memory&cache=private")

	created, err := database.CreateMCPServer(ctx, db.CreateMCPServerParams{
		Name:            "upstream-1",
		Alias:           "upstream-1",
		URL:             "https://upstream.example.com/mcp",
		AuthType:        "none",
		CreatedBy:       "test",
		ProtocolVersion: "auto",
	})
	if err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}

	c := NewMCPTransportCache(make([]byte, 32), true, 5*time.Second, 5*time.Second, slog.Default())
	t.Cleanup(c.Close)

	c.LoadAll([]db.MCPServer{*created})
	before, ok := c.Get(created.ID)
	if !ok {
		t.Fatal("Get() ok = false after first LoadAll")
	}
	beforeTransport := before.Transport
	if beforeTransport == nil {
		t.Fatal("Transport = nil after first LoadAll")
	}

	// Change ONLY protocol_version — URL, AuthType, AuthHeader, and the
	// encrypted token fields are all untouched.
	newVersion := "2025-11-25"
	updated, err := database.UpdateMCPServer(ctx, created.ID, db.UpdateMCPServerParams{
		ProtocolVersion: &newVersion,
	})
	if err != nil {
		t.Fatalf("UpdateMCPServer: %v", err)
	}
	if updated.ProtocolVersion != newVersion {
		t.Fatalf("test setup: updated.ProtocolVersion = %q, want %q", updated.ProtocolVersion, newVersion)
	}

	c.LoadAll([]db.MCPServer{*updated})
	after, ok := c.Get(created.ID)
	if !ok {
		t.Fatal("Get() ok = false after second LoadAll")
	}

	if after.Transport == beforeTransport {
		t.Error("Transport unchanged after protocol_version changed — the pinned version override would remain silently in effect until process restart")
	}
	if after.ProtocolVersion != newVersion {
		t.Errorf("cached ProtocolVersion = %q, want %q", after.ProtocolVersion, newVersion)
	}
}

// TestMCPTransportCache_LoadAll_NoChange_ReusesTransport is the reuse-side
// counterpart of the regression test above: reloading with a server row that
// changed nothing at all — including protocol_version, once it participates
// in the comparison — must keep the SAME transport instance rather than
// rebuilding on every refresh tick.
func TestMCPTransportCache_LoadAll_NoChange_ReusesTransport(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := newTransportCacheTestDB(t, "file:TestMCPTransportCache_NoChange?mode=memory&cache=private")

	created, err := database.CreateMCPServer(ctx, db.CreateMCPServerParams{
		Name:            "upstream-1",
		Alias:           "upstream-1",
		URL:             "https://upstream.example.com/mcp",
		AuthType:        "none",
		CreatedBy:       "test",
		ProtocolVersion: "2025-03-26",
	})
	if err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}

	c := NewMCPTransportCache(make([]byte, 32), true, 5*time.Second, 5*time.Second, slog.Default())
	t.Cleanup(c.Close)

	c.LoadAll([]db.MCPServer{*created})
	before, ok := c.Get(created.ID)
	if !ok {
		t.Fatal("Get() ok = false after first LoadAll")
	}

	// Reload with the exact same row (fetched fresh from the DB, not the
	// same Go value) — nothing changed.
	reloaded, err := database.GetMCPServer(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	c.LoadAll([]db.MCPServer{*reloaded})

	after, ok := c.Get(created.ID)
	if !ok {
		t.Fatal("Get() ok = false after second LoadAll")
	}
	if after.Transport != before.Transport {
		t.Error("Transport was rebuilt even though nothing changed, want the same instance reused")
	}
}
