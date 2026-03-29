package app

// Tests for dbToolStore — the mcp.ToolStore implementation backed by the DB.
//
// Because dbToolStore is unexported, these tests live in package app (white-box).
// Each test creates an isolated in-memory SQLite database via openToolStoreDB,
// constructs a dbToolStore, and exercises its public-surface methods through the
// mcp.ToolStore interface.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// openToolStoreDB opens an isolated in-memory SQLite database, runs all
// migrations, and registers cleanup. The test name is embedded in the DSN to
// prevent cross-test database sharing.
func openToolStoreDB(t *testing.T) *db.DB {
	t.Helper()

	// Replace characters that are invalid in SQLite URI filenames.
	safeName := strings.NewReplacer("/", "_", " ", "_", "#", "_").Replace(t.Name())

	ctx := context.Background()
	d, err := db.Open(ctx, config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             "file:" + safeName + "?mode=memory&cache=private",
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if err := db.RunMigrations(ctx, d.SQL(), db.SQLiteDialect{}); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	return d
}

// mustCreateToolStoreServer creates an MCP server record for tool store tests.
// The alias is used as both the name suffix and alias.
func mustCreateToolStoreServer(t *testing.T, d *db.DB, alias string) *db.MCPServer {
	t.Helper()
	s, err := d.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "ToolStore " + alias,
		Alias:    alias,
		URL:      "https://mcp.example.com/" + alias,
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("CreateMCPServer %q: %v", alias, err)
	}
	return s
}

// sampleTools returns a slice of mcp.Tool values for seeding tests.
func sampleTools(names ...string) []mcp.Tool {
	tools := make([]mcp.Tool, len(names))
	for i, n := range names {
		tools[i] = mcp.Tool{
			Name:        n,
			Description: "does " + n,
			InputSchema: mcp.InputSchema{Type: "object"},
		}
	}
	return tools
}

// ---- LoadAll -----------------------------------------------------------------

func TestDBToolStore_LoadAll(t *testing.T) {
	t.Parallel()

	d := openToolStoreDB(t)
	store := &dbToolStore{db: d}

	serverA := mustCreateToolStoreServer(t, d, "ts-load-all-a")
	serverB := mustCreateToolStoreServer(t, d, "ts-load-all-b")

	if err := store.Save(context.Background(), serverA.ID, sampleTools("read_file", "write_file")); err != nil {
		t.Fatalf("Save(serverA): %v", err)
	}
	if err := store.Save(context.Background(), serverB.ID, sampleTools("exec_cmd")); err != nil {
		t.Fatalf("Save(serverB): %v", err)
	}

	got, err := store.LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll() error = %v, want nil", err)
	}

	toolsA, ok := got[serverA.ID]
	if !ok {
		t.Fatalf("LoadAll() missing key for serverA ID %q", serverA.ID)
	}
	if len(toolsA) != 2 {
		t.Errorf("LoadAll()[serverA.ID] len = %d, want 2", len(toolsA))
	}

	toolsB, ok := got[serverB.ID]
	if !ok {
		t.Fatalf("LoadAll() missing key for serverB ID %q", serverB.ID)
	}
	if len(toolsB) != 1 || toolsB[0].Name != "exec_cmd" {
		t.Errorf("LoadAll()[serverB.ID] = %v, want [{exec_cmd}]", toolsB)
	}
}

func TestDBToolStore_LoadAll_Empty(t *testing.T) {
	t.Parallel()

	d := openToolStoreDB(t)
	store := &dbToolStore{db: d}

	got, err := store.LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll() error = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("LoadAll() len = %d, want 0 when no tools exist", len(got))
	}
}

// ---- Save --------------------------------------------------------------------

func TestDBToolStore_Save(t *testing.T) {
	t.Parallel()

	d := openToolStoreDB(t)
	store := &dbToolStore{db: d}

	s := mustCreateToolStoreServer(t, d, "ts-save")

	tools := sampleTools("tool_alpha", "tool_beta")
	if err := store.Save(context.Background(), s.ID, tools); err != nil {
		t.Fatalf("Save() error = %v, want nil", err)
	}

	// Verify via ListServerTools that both tools were persisted.
	stored, err := d.ListServerTools(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("ListServerTools(): %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("ListServerTools() len = %d, want 2", len(stored))
	}

	// Results are ordered by name ASC.
	if stored[0].Name != "tool_alpha" {
		t.Errorf("stored[0].Name = %q, want %q", stored[0].Name, "tool_alpha")
	}
	if stored[1].Name != "tool_beta" {
		t.Errorf("stored[1].Name = %q, want %q", stored[1].Name, "tool_beta")
	}
}

func TestDBToolStore_Save_Replace(t *testing.T) {
	t.Parallel()

	d := openToolStoreDB(t)
	store := &dbToolStore{db: d}

	s := mustCreateToolStoreServer(t, d, "ts-save-replace")

	if err := store.Save(context.Background(), s.ID, sampleTools("old_tool_one", "old_tool_two")); err != nil {
		t.Fatalf("Save() first call error = %v", err)
	}

	// Second save with different tools — previous entries must be replaced.
	if err := store.Save(context.Background(), s.ID, sampleTools("new_tool")); err != nil {
		t.Fatalf("Save() second call error = %v", err)
	}

	stored, err := d.ListServerTools(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("ListServerTools(): %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("ListServerTools() len = %d, want 1 after replace", len(stored))
	}
	if stored[0].Name != "new_tool" {
		t.Errorf("stored[0].Name = %q, want %q", stored[0].Name, "new_tool")
	}
}

func TestDBToolStore_Save_UnknownServerID(t *testing.T) {
	t.Parallel()

	d := openToolStoreDB(t)
	store := &dbToolStore{db: d}

	// A non-existent server ID triggers a FK constraint failure in the DB.
	err := store.Save(context.Background(), "00000000-0000-0000-0000-000000000000", sampleTools("tool_x"))
	if err == nil {
		t.Fatal("Save() with unknown server ID error = nil, want error")
	}
}

// ---- Delete ------------------------------------------------------------------

func TestDBToolStore_Delete(t *testing.T) {
	t.Parallel()

	d := openToolStoreDB(t)
	store := &dbToolStore{db: d}

	s := mustCreateToolStoreServer(t, d, "ts-delete")

	if err := store.Save(context.Background(), s.ID, sampleTools("tool_to_remove")); err != nil {
		t.Fatalf("Save(): %v", err)
	}

	if err := store.Delete(context.Background(), s.ID); err != nil {
		t.Fatalf("Delete() error = %v, want nil", err)
	}

	stored, err := d.ListServerTools(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("ListServerTools() after Delete: %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("ListServerTools() len = %d, want 0 after Delete", len(stored))
	}
}

func TestDBToolStore_Delete_NoOp(t *testing.T) {
	t.Parallel()

	d := openToolStoreDB(t)
	store := &dbToolStore{db: d}

	s := mustCreateToolStoreServer(t, d, "ts-delete-noop")

	// Delete when no tools have been saved must not return an error.
	if err := store.Delete(context.Background(), s.ID); err != nil {
		t.Errorf("Delete() on server with no tools error = %v, want nil", err)
	}
}

// ---- LoadAll — corrupt JSON schema skipped ----------------------------------

func TestDBToolStore_LoadAll_SkipsCorruptJSON(t *testing.T) {
	t.Parallel()

	d := openToolStoreDB(t)
	store := &dbToolStore{db: d}

	s := mustCreateToolStoreServer(t, d, "ts-corrupt-schema")

	// Insert one valid tool and one tool with a corrupt input_schema directly
	// via UpsertServerTools so we bypass the Save helper's json.Marshal path.
	rawTools := []db.MCPServerTool{
		{ServerID: s.ID, Name: "valid_tool", Description: "ok", InputSchema: `{"type":"object"}`},
		{ServerID: s.ID, Name: "corrupt_tool", Description: "bad schema", InputSchema: `not-valid-json`},
	}
	if err := d.UpsertServerTools(context.Background(), s.ID, rawTools); err != nil {
		t.Fatalf("UpsertServerTools: %v", err)
	}

	got, err := store.LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll() error = %v, want nil", err)
	}

	tools, ok := got[s.ID]
	if !ok {
		t.Fatalf("LoadAll() missing key for server ID %q", s.ID)
	}
	// Only the valid tool must be present; the corrupt one must be silently skipped.
	if len(tools) != 1 {
		t.Fatalf("LoadAll()[s.ID] len = %d, want 1 (corrupt entry skipped)", len(tools))
	}
	if tools[0].Name != "valid_tool" {
		t.Errorf("tools[0].Name = %q, want %q", tools[0].Name, "valid_tool")
	}
}

// ---- InputSchema round-trip -------------------------------------------------

// TestDBToolStore_Save_SchemaPreserved verifies that the InputSchema fields
// (type, properties) survive the Save → LoadAll round-trip without loss.
func TestDBToolStore_Save_SchemaPreserved(t *testing.T) {
	t.Parallel()

	d := openToolStoreDB(t)
	store := &dbToolStore{db: d}

	s := mustCreateToolStoreServer(t, d, "ts-schema-roundtrip")

	tools := []mcp.Tool{
		{
			Name:        "search",
			Description: "search the web",
			InputSchema: mcp.InputSchema{
				Type: "object",
				Properties: map[string]mcp.Property{
					"query": {Type: "string", Description: "search query"},
				},
			},
		},
	}
	if err := store.Save(context.Background(), s.ID, tools); err != nil {
		t.Fatalf("Save(): %v", err)
	}

	got, err := store.LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll(): %v", err)
	}

	loaded, ok := got[s.ID]
	if !ok || len(loaded) != 1 {
		t.Fatalf("LoadAll()[s.ID] len = %d, want 1", len(got[s.ID]))
	}

	tool := loaded[0]
	if tool.Name != "search" {
		t.Errorf("tool.Name = %q, want %q", tool.Name, "search")
	}
	if tool.InputSchema.Type != "object" {
		t.Errorf("InputSchema.Type = %q, want %q", tool.InputSchema.Type, "object")
	}
	if _, ok := tool.InputSchema.Properties["query"]; !ok {
		t.Error("InputSchema.Properties missing 'query' after round-trip")
	}
}
