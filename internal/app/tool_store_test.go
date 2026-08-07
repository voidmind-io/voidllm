package app

// Tests for dbToolStore — the mcp.ToolStore implementation backed by the DB.
//
// Because dbToolStore is unexported, these tests live in package app (white-box).
// Each test creates an isolated in-memory SQLite database via openToolStoreDB,
// constructs a dbToolStore, and exercises its public-surface methods through the
// mcp.ToolStore interface.

import (
	"context"
	"encoding/json"
	"log/slog"
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

	if err := db.RunMigrations(ctx, d.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
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
			InputSchema: mcp.ObjectSchema(nil),
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

// TestDBToolStore_Save_MarshalFailure_WritesNothing verifies FIX 9: when any
// tool's InputSchema fails to marshal, Save returns an error and writes
// NOTHING to the database — not even the other, well-formed tools in the same
// batch. Silently writing an empty schema for the broken tool instead would
// make it vanish invisibly after the next restart (LoadAll's corrupt-schema
// skip in dbToolStore.LoadAll would then also silently drop it); returning
// early with no partial write is what makes the failure visible and
// recoverable instead.
func TestDBToolStore_Save_MarshalFailure_WritesNothing(t *testing.T) {
	t.Parallel()

	d := openToolStoreDB(t)
	store := &dbToolStore{db: d}

	s := mustCreateToolStoreServer(t, d, "ts-marshal-failure")

	tools := []mcp.Tool{
		{Name: "well_formed_tool", InputSchema: mcp.ObjectSchema(nil)},
		// JSONSchema.MarshalJSON returns these bytes verbatim; encoding/json's
		// own compaction step then rejects them as malformed JSON at marshal
		// time — see the identical reasoning in internal/mcp/server_test.go's
		// brokenSchemaTool.
		{Name: "broken_schema_tool", InputSchema: mcp.JSONSchema("not-valid-json")},
	}

	err := store.Save(context.Background(), s.ID, tools)
	if err == nil {
		t.Fatal("Save() error = nil, want a marshal error for the broken tool's InputSchema")
	}
	if !strings.Contains(err.Error(), "broken_schema_tool") {
		t.Errorf("Save() error = %q, want it to name the offending tool %q", err.Error(), "broken_schema_tool")
	}

	// Nothing must have been written — not even well_formed_tool, which by
	// itself would have marshaled fine.
	stored, listErr := d.ListServerTools(context.Background(), s.ID)
	if listErr != nil {
		t.Fatalf("ListServerTools(): %v", listErr)
	}
	if len(stored) != 0 {
		t.Errorf("ListServerTools() len = %d, want 0 — Save() must write nothing on a marshal failure, got: %+v",
			len(stored), stored)
	}

	// LoadAll must likewise report no tools for this server.
	loaded, loadErr := store.LoadAll(context.Background())
	if loadErr != nil {
		t.Fatalf("LoadAll(): %v", loadErr)
	}
	if len(loaded[s.ID]) != 0 {
		t.Errorf("LoadAll()[s.ID] len = %d, want 0 after a failed Save()", len(loaded[s.ID]))
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
			InputSchema: mcp.ObjectSchema(map[string]mcp.SchemaProp{
				"query": {Type: "string", Description: "search query"},
			}),
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

	// InputSchema is now raw JSON (mcp.JSONSchema); decode it to verify the
	// type and properties keywords survived the Save → LoadAll round-trip.
	var schema map[string]any
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		t.Fatalf("unmarshal InputSchema: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("InputSchema.type = %v, want %q", schema["type"], "object")
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["query"]; !ok {
		t.Error("InputSchema.properties missing 'query' after round-trip")
	}
}
