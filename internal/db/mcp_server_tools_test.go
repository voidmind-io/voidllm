package db

import (
	"context"
	"testing"
)

// ---- helpers -----------------------------------------------------------------

// makeTools is a convenience factory that builds a slice of MCPServerTool
// values for use in UpsertServerTools. ServerID and ID are intentionally left
// empty — UpsertServerTools generates new IDs during insert.
func makeTools(names ...string) []MCPServerTool {
	tools := make([]MCPServerTool, len(names))
	for i, n := range names {
		tools[i] = MCPServerTool{
			Name:        n,
			Description: "description for " + n,
			InputSchema: `{"type":"object"}`,
		}
	}
	return tools
}

// ---- UpsertServerTools -------------------------------------------------------

func TestUpsertServerTools(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("tools-upsert-basic"))

	tools := makeTools("read_file", "write_file", "exec_shell")
	if err := d.UpsertServerTools(context.Background(), server.ID, tools); err != nil {
		t.Fatalf("UpsertServerTools() error = %v, want nil", err)
	}

	stored, err := d.ListServerTools(context.Background(), server.ID)
	if err != nil {
		t.Fatalf("ListServerTools() error = %v", err)
	}
	if len(stored) != 3 {
		t.Fatalf("ListServerTools() len = %d, want 3", len(stored))
	}

	// Each stored tool must have a non-empty ID, the correct ServerID, and
	// a non-empty FetchedAt timestamp set by CURRENT_TIMESTAMP in the INSERT.
	for _, st := range stored {
		if st.ID == "" {
			t.Errorf("tool %q: ID is empty", st.Name)
		}
		if st.ServerID != server.ID {
			t.Errorf("tool %q: ServerID = %q, want %q", st.Name, st.ServerID, server.ID)
		}
		if st.FetchedAt == "" {
			t.Errorf("tool %q: FetchedAt is empty", st.Name)
		}
	}

	// Verify Name, Description, and InputSchema are preserved.
	for i, want := range []MCPServerTool{
		{Name: "exec_shell", Description: "description for exec_shell", InputSchema: `{"type":"object"}`},
		{Name: "read_file", Description: "description for read_file", InputSchema: `{"type":"object"}`},
		{Name: "write_file", Description: "description for write_file", InputSchema: `{"type":"object"}`},
	} {
		if stored[i].Name != want.Name {
			t.Errorf("stored[%d].Name = %q, want %q", i, stored[i].Name, want.Name)
		}
		if stored[i].Description != want.Description {
			t.Errorf("stored[%d].Description = %q, want %q", i, stored[i].Description, want.Description)
		}
		if stored[i].InputSchema != want.InputSchema {
			t.Errorf("stored[%d].InputSchema = %q, want %q", i, stored[i].InputSchema, want.InputSchema)
		}
	}
}

func TestUpsertServerTools_Replace(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("tools-upsert-replace"))

	// First upsert: three tools.
	if err := d.UpsertServerTools(context.Background(), server.ID, makeTools("alpha", "beta", "gamma")); err != nil {
		t.Fatalf("UpsertServerTools() first call error = %v", err)
	}

	// Second upsert: different tools — old ones must be replaced entirely.
	if err := d.UpsertServerTools(context.Background(), server.ID, makeTools("delta", "epsilon")); err != nil {
		t.Fatalf("UpsertServerTools() second call error = %v", err)
	}

	stored, err := d.ListServerTools(context.Background(), server.ID)
	if err != nil {
		t.Fatalf("ListServerTools() error = %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("ListServerTools() len = %d, want 2 after replace", len(stored))
	}

	// Results must contain only the new tools, ordered by name ASC.
	if stored[0].Name != "delta" {
		t.Errorf("stored[0].Name = %q, want %q", stored[0].Name, "delta")
	}
	if stored[1].Name != "epsilon" {
		t.Errorf("stored[1].Name = %q, want %q", stored[1].Name, "epsilon")
	}
}

func TestUpsertServerTools_Empty(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("tools-upsert-empty"))

	// Seed some tools first.
	if err := d.UpsertServerTools(context.Background(), server.ID, makeTools("tool_a", "tool_b")); err != nil {
		t.Fatalf("UpsertServerTools() seed error = %v", err)
	}

	// Upsert with empty slice must delete all existing tools.
	if err := d.UpsertServerTools(context.Background(), server.ID, []MCPServerTool{}); err != nil {
		t.Fatalf("UpsertServerTools() empty slice error = %v", err)
	}

	stored, err := d.ListServerTools(context.Background(), server.ID)
	if err != nil {
		t.Fatalf("ListServerTools() error = %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("ListServerTools() len = %d, want 0 after empty upsert", len(stored))
	}
}

func TestUpsertServerTools_NilSlice(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("tools-upsert-nil"))

	if err := d.UpsertServerTools(context.Background(), server.ID, makeTools("tool_x")); err != nil {
		t.Fatalf("UpsertServerTools() seed error = %v", err)
	}

	// nil slice behaves identically to empty slice — deletes all rows.
	if err := d.UpsertServerTools(context.Background(), server.ID, nil); err != nil {
		t.Fatalf("UpsertServerTools() nil slice error = %v", err)
	}

	stored, err := d.ListServerTools(context.Background(), server.ID)
	if err != nil {
		t.Fatalf("ListServerTools() error = %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("ListServerTools() len = %d, want 0 after nil upsert", len(stored))
	}
}

// ---- ListServerTools ---------------------------------------------------------

func TestListServerTools(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("tools-list-order"))

	// Insert in non-alphabetical order to confirm ORDER BY name ASC is enforced.
	if err := d.UpsertServerTools(context.Background(), server.ID, makeTools("zulu", "alpha", "mike")); err != nil {
		t.Fatalf("UpsertServerTools() error = %v", err)
	}

	stored, err := d.ListServerTools(context.Background(), server.ID)
	if err != nil {
		t.Fatalf("ListServerTools() error = %v, want nil", err)
	}
	if len(stored) != 3 {
		t.Fatalf("ListServerTools() len = %d, want 3", len(stored))
	}

	wantOrder := []string{"alpha", "mike", "zulu"}
	for i, want := range wantOrder {
		if stored[i].Name != want {
			t.Errorf("stored[%d].Name = %q, want %q (ORDER BY name ASC violated)", i, stored[i].Name, want)
		}
	}
}

func TestListServerTools_Empty(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("tools-list-empty"))

	stored, err := d.ListServerTools(context.Background(), server.ID)
	if err != nil {
		t.Fatalf("ListServerTools() error = %v, want nil", err)
	}
	if len(stored) != 0 {
		t.Errorf("ListServerTools() len = %d, want 0 for server with no tools", len(stored))
	}
}

func TestListServerTools_IsolatedByServer(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	serverA := mustCreateMCPServer(t, d, defaultMCPParams("tools-list-isolated-a"))
	serverB := mustCreateMCPServer(t, d, defaultMCPParams("tools-list-isolated-b"))

	if err := d.UpsertServerTools(context.Background(), serverA.ID, makeTools("tool_a1", "tool_a2")); err != nil {
		t.Fatalf("UpsertServerTools(serverA) error = %v", err)
	}
	if err := d.UpsertServerTools(context.Background(), serverB.ID, makeTools("tool_b1")); err != nil {
		t.Fatalf("UpsertServerTools(serverB) error = %v", err)
	}

	storedA, err := d.ListServerTools(context.Background(), serverA.ID)
	if err != nil {
		t.Fatalf("ListServerTools(serverA) error = %v", err)
	}
	if len(storedA) != 2 {
		t.Errorf("ListServerTools(serverA) len = %d, want 2", len(storedA))
	}

	storedB, err := d.ListServerTools(context.Background(), serverB.ID)
	if err != nil {
		t.Fatalf("ListServerTools(serverB) error = %v", err)
	}
	if len(storedB) != 1 {
		t.Errorf("ListServerTools(serverB) len = %d, want 1", len(storedB))
	}
}

// ---- ListAllServerTools ------------------------------------------------------

func TestListAllServerTools(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	serverA := mustCreateMCPServer(t, d, defaultMCPParams("tools-all-alpha"))
	serverB := mustCreateMCPServer(t, d, defaultMCPParams("tools-all-beta"))

	if err := d.UpsertServerTools(context.Background(), serverA.ID, makeTools("read_file", "write_file")); err != nil {
		t.Fatalf("UpsertServerTools(serverA) error = %v", err)
	}
	if err := d.UpsertServerTools(context.Background(), serverB.ID, makeTools("exec_shell")); err != nil {
		t.Fatalf("UpsertServerTools(serverB) error = %v", err)
	}

	result, err := d.ListAllServerTools(context.Background())
	if err != nil {
		t.Fatalf("ListAllServerTools() error = %v, want nil", err)
	}

	// Map must be keyed by server ID.
	toolsA, ok := result[serverA.ID]
	if !ok {
		t.Fatalf("ListAllServerTools() missing key for serverA ID %q", serverA.ID)
	}
	if len(toolsA) != 2 {
		t.Errorf("result[serverA.ID] len = %d, want 2", len(toolsA))
	}
	if toolsA[0].Name != "read_file" || toolsA[1].Name != "write_file" {
		t.Errorf("result[serverA.ID] names = [%q, %q], want [read_file, write_file]",
			toolsA[0].Name, toolsA[1].Name)
	}

	toolsB, ok := result[serverB.ID]
	if !ok {
		t.Fatalf("ListAllServerTools() missing key for serverB ID %q", serverB.ID)
	}
	if len(toolsB) != 1 || toolsB[0].Name != "exec_shell" {
		t.Errorf("result[serverB.ID] = %v, want [{exec_shell}]", toolsB)
	}
}

func TestListAllServerTools_OnlyActiveServers(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	active := mustCreateMCPServer(t, d, defaultMCPParams("tools-active-server"))
	inactive := mustCreateMCPServer(t, d, defaultMCPParams("tools-inactive-server"))

	if err := d.UpsertServerTools(context.Background(), active.ID, makeTools("visible_tool")); err != nil {
		t.Fatalf("UpsertServerTools(active) error = %v", err)
	}
	if err := d.UpsertServerTools(context.Background(), inactive.ID, makeTools("hidden_tool")); err != nil {
		t.Fatalf("UpsertServerTools(inactive) error = %v", err)
	}

	// Deactivate the second server.
	falseVal := false
	if _, err := d.UpdateMCPServer(context.Background(), inactive.ID, UpdateMCPServerParams{IsActive: &falseVal}); err != nil {
		t.Fatalf("UpdateMCPServer(deactivate) error = %v", err)
	}

	result, err := d.ListAllServerTools(context.Background())
	if err != nil {
		t.Fatalf("ListAllServerTools() error = %v", err)
	}

	if _, found := result[active.ID]; !found {
		t.Error("ListAllServerTools() missing active server ID")
	}
	if _, found := result[inactive.ID]; found {
		t.Error("ListAllServerTools() returned tools for deactivated server, want excluded")
	}
}

func TestListAllServerTools_DeletedServerExcluded(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	alive := mustCreateMCPServer(t, d, defaultMCPParams("tools-alive-server"))
	deleted := mustCreateMCPServer(t, d, defaultMCPParams("tools-deleted-server"))

	if err := d.UpsertServerTools(context.Background(), alive.ID, makeTools("tool_alive")); err != nil {
		t.Fatalf("UpsertServerTools(alive) error = %v", err)
	}
	if err := d.UpsertServerTools(context.Background(), deleted.ID, makeTools("tool_deleted")); err != nil {
		t.Fatalf("UpsertServerTools(deleted) error = %v", err)
	}

	if err := d.DeleteMCPServer(context.Background(), deleted.ID); err != nil {
		t.Fatalf("DeleteMCPServer() error = %v", err)
	}

	result, err := d.ListAllServerTools(context.Background())
	if err != nil {
		t.Fatalf("ListAllServerTools() error = %v", err)
	}

	if _, found := result[alive.ID]; !found {
		t.Error("ListAllServerTools() missing alive server ID")
	}
	if _, found := result[deleted.ID]; found {
		t.Error("ListAllServerTools() returned tools for soft-deleted server, want excluded")
	}
}

func TestListAllServerTools_Empty(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)

	result, err := d.ListAllServerTools(context.Background())
	if err != nil {
		t.Fatalf("ListAllServerTools() error = %v, want nil", err)
	}
	if len(result) != 0 {
		t.Errorf("ListAllServerTools() len = %d, want 0 when no tools exist", len(result))
	}
}

// ---- DeleteServerTools -------------------------------------------------------

func TestDeleteServerTools(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("tools-delete"))

	if err := d.UpsertServerTools(context.Background(), server.ID, makeTools("tool_x", "tool_y")); err != nil {
		t.Fatalf("UpsertServerTools() error = %v", err)
	}

	if err := d.DeleteServerTools(context.Background(), server.ID); err != nil {
		t.Fatalf("DeleteServerTools() error = %v, want nil", err)
	}

	stored, err := d.ListServerTools(context.Background(), server.ID)
	if err != nil {
		t.Fatalf("ListServerTools() after delete error = %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("ListServerTools() len = %d, want 0 after DeleteServerTools", len(stored))
	}
}

func TestDeleteServerTools_NoOp(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("tools-delete-noop"))

	// Deleting tools for a server with no cached tools must not error.
	if err := d.DeleteServerTools(context.Background(), server.ID); err != nil {
		t.Errorf("DeleteServerTools() on empty set error = %v, want nil", err)
	}
}

func TestDeleteServerTools_IsolatedByServer(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	serverA := mustCreateMCPServer(t, d, defaultMCPParams("tools-del-isolated-a"))
	serverB := mustCreateMCPServer(t, d, defaultMCPParams("tools-del-isolated-b"))

	if err := d.UpsertServerTools(context.Background(), serverA.ID, makeTools("keep_me")); err != nil {
		t.Fatalf("UpsertServerTools(serverA) error = %v", err)
	}
	if err := d.UpsertServerTools(context.Background(), serverB.ID, makeTools("delete_me")); err != nil {
		t.Fatalf("UpsertServerTools(serverB) error = %v", err)
	}

	if err := d.DeleteServerTools(context.Background(), serverB.ID); err != nil {
		t.Fatalf("DeleteServerTools(serverB) error = %v", err)
	}

	// serverA tools must remain untouched.
	storedA, err := d.ListServerTools(context.Background(), serverA.ID)
	if err != nil {
		t.Fatalf("ListServerTools(serverA) error = %v", err)
	}
	if len(storedA) != 1 || storedA[0].Name != "keep_me" {
		t.Errorf("ListServerTools(serverA) = %v, want [{keep_me}] after deleting serverB tools", storedA)
	}

	// serverB tools must be gone.
	storedB, err := d.ListServerTools(context.Background(), serverB.ID)
	if err != nil {
		t.Fatalf("ListServerTools(serverB) error = %v", err)
	}
	if len(storedB) != 0 {
		t.Errorf("ListServerTools(serverB) len = %d, want 0", len(storedB))
	}
}
