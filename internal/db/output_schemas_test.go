package db

import (
	"context"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

const sqliteTimestampFmt = "2006-01-02 15:04:05"

// mustSaveOutputSchema saves an output schema and fails the test on error.
func mustSaveOutputSchema(t *testing.T, d *DB, serverID, toolName string, schema jsonx.RawMessage) {
	t.Helper()
	if err := d.SaveOutputSchema(context.Background(), serverID, toolName, schema); err != nil {
		t.Fatalf("mustSaveOutputSchema(server=%q, tool=%q): %v", serverID, toolName, err)
	}
}

// ageOutputSchema sets the inferred_at column for a (server, tool) row to a
// specific time, allowing staleness tests to work without sleeping.
func ageOutputSchema(t *testing.T, d *DB, serverID, toolName string, age time.Time) {
	t.Helper()
	ts := age.UTC().Format(sqliteTimestampFmt)
	_, err := d.sql.ExecContext(
		context.Background(),
		"UPDATE output_schemas SET inferred_at = ? WHERE server_id = ? AND tool_name = ?",
		ts, serverID, toolName,
	)
	if err != nil {
		t.Fatalf("ageOutputSchema(%q, %q, %q): %v", serverID, toolName, ts, err)
	}
}

// ---- SaveOutputSchema --------------------------------------------------------

func TestSaveOutputSchema_Inserts(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("schema-insert"))

	schema := jsonx.RawMessage(`{"type":"object","properties":{"id":{"type":"number"}}}`)
	mustSaveOutputSchema(t, d, server.ID, "list_repos", schema)

	schemas, err := d.GetAllOutputSchemas(context.Background(), server.ID, 0)
	if err != nil {
		t.Fatalf("GetAllOutputSchemas() error = %v, want nil", err)
	}
	if len(schemas) != 1 {
		t.Fatalf("GetAllOutputSchemas() len = %d, want 1", len(schemas))
	}
	got, ok := schemas["list_repos"]
	if !ok {
		t.Fatalf("GetAllOutputSchemas(): key %q missing from result %v", "list_repos", schemas)
	}

	// Compare parsed representations to be key-order agnostic.
	var gotMap, wantMap map[string]any
	if err := jsonx.Unmarshal(got, &gotMap); err != nil {
		t.Fatalf("unmarshal got schema: %v", err)
	}
	if err := jsonx.Unmarshal(schema, &wantMap); err != nil {
		t.Fatalf("unmarshal want schema: %v", err)
	}
	if gotMap["type"] != wantMap["type"] {
		t.Errorf("schema type = %q, want %q", gotMap["type"], wantMap["type"])
	}
}

func TestSaveOutputSchema_Upserts(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("schema-upsert"))

	schemaA := jsonx.RawMessage(`{"type":"string"}`)
	schemaB := jsonx.RawMessage(`{"type":"number"}`)

	mustSaveOutputSchema(t, d, server.ID, "my_tool", schemaA)
	mustSaveOutputSchema(t, d, server.ID, "my_tool", schemaB)

	schemas, err := d.GetAllOutputSchemas(context.Background(), server.ID, 0)
	if err != nil {
		t.Fatalf("GetAllOutputSchemas() error = %v, want nil", err)
	}
	if len(schemas) != 1 {
		t.Fatalf("GetAllOutputSchemas() len = %d, want 1 after upsert", len(schemas))
	}

	// Verify the stored value is schemaB.
	got := schemas["my_tool"]
	var gotMap map[string]any
	if err := jsonx.Unmarshal(got, &gotMap); err != nil {
		t.Fatalf("unmarshal stored schema: %v", err)
	}
	if gotMap["type"] != "number" {
		t.Errorf("upserted schema type = %q, want %q", gotMap["type"], "number")
	}

	// Confirm exactly one row in the table.
	var rowCount int
	if err := d.sql.QueryRowContext(
		context.Background(),
		"SELECT COUNT(*) FROM output_schemas WHERE server_id = ? AND tool_name = ?",
		server.ID, "my_tool",
	).Scan(&rowCount); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("output_schemas row count = %d, want 1 after upsert", rowCount)
	}
}

// ---- GetAllOutputSchemas -----------------------------------------------------

func TestGetAllOutputSchemas_FiltersStale(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("schema-stale-filter"))

	mustSaveOutputSchema(t, d, server.ID, "old_tool", jsonx.RawMessage(`{"type":"string"}`))
	// Age the row to 10 minutes ago.
	ageOutputSchema(t, d, server.ID, "old_tool", time.Now().Add(-10*time.Minute))

	// maxAge = 5 minutes → row is 10 min old → filtered out.
	schemas, err := d.GetAllOutputSchemas(context.Background(), server.ID, 5*time.Minute)
	if err != nil {
		t.Fatalf("GetAllOutputSchemas(maxAge=5m) error = %v, want nil", err)
	}
	if len(schemas) != 0 {
		t.Errorf("GetAllOutputSchemas(maxAge=5m) len = %d, want 0 (stale row filtered)", len(schemas))
	}

	// maxAge = 0 → all rows returned regardless of age.
	schemas, err = d.GetAllOutputSchemas(context.Background(), server.ID, 0)
	if err != nil {
		t.Fatalf("GetAllOutputSchemas(maxAge=0) error = %v, want nil", err)
	}
	if len(schemas) != 1 {
		t.Errorf("GetAllOutputSchemas(maxAge=0) len = %d, want 1 (zero TTL returns all)", len(schemas))
	}
}

func TestGetAllOutputSchemas_ZeroMaxAgeReturnsAll(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("schema-zero-ttl"))

	mustSaveOutputSchema(t, d, server.ID, "tool_a", jsonx.RawMessage(`{"type":"string"}`))
	mustSaveOutputSchema(t, d, server.ID, "tool_b", jsonx.RawMessage(`{"type":"number"}`))
	// Age both rows to well beyond any reasonable maxAge.
	ageOutputSchema(t, d, server.ID, "tool_a", time.Now().Add(-24*time.Hour))
	ageOutputSchema(t, d, server.ID, "tool_b", time.Now().Add(-48*time.Hour))

	schemas, err := d.GetAllOutputSchemas(context.Background(), server.ID, 0)
	if err != nil {
		t.Fatalf("GetAllOutputSchemas(maxAge=0) error = %v, want nil", err)
	}
	if len(schemas) != 2 {
		t.Errorf("GetAllOutputSchemas(maxAge=0) len = %d, want 2 (zero TTL returns all)", len(schemas))
	}
}

// ---- IsOutputSchemaStale -----------------------------------------------------

func TestIsOutputSchemaStale_MissingIsStale(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("schema-missing-stale"))

	stale, err := d.IsOutputSchemaStale(context.Background(), server.ID, "nonexistent_tool", time.Hour)
	if err != nil {
		t.Fatalf("IsOutputSchemaStale() error = %v, want nil", err)
	}
	if !stale {
		t.Error("IsOutputSchemaStale() = false, want true for missing row")
	}
}

func TestIsOutputSchemaStale_FreshIsNotStale(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("schema-fresh"))

	mustSaveOutputSchema(t, d, server.ID, "fresh_tool", jsonx.RawMessage(`{"type":"string"}`))

	stale, err := d.IsOutputSchemaStale(context.Background(), server.ID, "fresh_tool", time.Hour)
	if err != nil {
		t.Fatalf("IsOutputSchemaStale() error = %v, want nil", err)
	}
	if stale {
		t.Error("IsOutputSchemaStale() = true, want false for freshly inserted row")
	}
}

func TestIsOutputSchemaStale_ExpiredIsStale(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("schema-expired"))

	mustSaveOutputSchema(t, d, server.ID, "old_tool", jsonx.RawMessage(`{"type":"boolean"}`))
	ageOutputSchema(t, d, server.ID, "old_tool", time.Now().Add(-10*time.Minute))

	stale, err := d.IsOutputSchemaStale(context.Background(), server.ID, "old_tool", 5*time.Minute)
	if err != nil {
		t.Fatalf("IsOutputSchemaStale() error = %v, want nil", err)
	}
	if !stale {
		t.Error("IsOutputSchemaStale() = false, want true for row aged beyond maxAge")
	}
}

func TestIsOutputSchemaStale_ZeroMaxAgeNeverStale(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("schema-zero-ttl-stale"))

	mustSaveOutputSchema(t, d, server.ID, "any_tool", jsonx.RawMessage(`{"type":"string"}`))
	ageOutputSchema(t, d, server.ID, "any_tool", time.Now().Add(-365*24*time.Hour))

	stale, err := d.IsOutputSchemaStale(context.Background(), server.ID, "any_tool", 0)
	if err != nil {
		t.Fatalf("IsOutputSchemaStale(maxAge=0) error = %v, want nil", err)
	}
	if stale {
		t.Error("IsOutputSchemaStale(maxAge=0) = true, want false (zero TTL disables staleness check)")
	}
}

// ---- Cascade on server delete ------------------------------------------------

func TestOutputSchemas_CascadeOnServerDelete(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("schema-cascade"))

	mustSaveOutputSchema(t, d, server.ID, "tool_one", jsonx.RawMessage(`{"type":"string"}`))
	mustSaveOutputSchema(t, d, server.ID, "tool_two", jsonx.RawMessage(`{"type":"number"}`))

	// Verify the rows exist before deletion.
	var before int
	if err := d.sql.QueryRowContext(
		context.Background(),
		"SELECT COUNT(*) FROM output_schemas WHERE server_id = ?", server.ID,
	).Scan(&before); err != nil {
		t.Fatalf("pre-delete count query: %v", err)
	}
	if before != 2 {
		t.Fatalf("pre-delete output_schemas count = %d, want 2", before)
	}

	// Delete the server row directly (hard delete) to trigger ON DELETE CASCADE.
	// DeleteMCPServer performs a soft-delete (sets deleted_at) and does not remove
	// the row, so it would not fire the FK cascade. A hard DELETE is required.
	if _, err := d.sql.ExecContext(
		context.Background(),
		"DELETE FROM mcp_servers WHERE id = ?", server.ID,
	); err != nil {
		t.Fatalf("hard DELETE mcp_servers: %v", err)
	}

	var after int
	if err := d.sql.QueryRowContext(
		context.Background(),
		"SELECT COUNT(*) FROM output_schemas WHERE server_id = ?", server.ID,
	).Scan(&after); err != nil {
		t.Fatalf("post-delete count query: %v", err)
	}
	if after != 0 {
		t.Errorf("post-delete output_schemas count = %d, want 0 (ON DELETE CASCADE)", after)
	}
}
