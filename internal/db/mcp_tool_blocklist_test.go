package db

import (
	"context"
	"errors"
	"testing"
)

// ---- helpers -----------------------------------------------------------------

// mustCreateBlocklistEntry creates a ToolBlocklistEntry and fails the test on error.
func mustCreateBlocklistEntry(t *testing.T, d *DB, serverID, toolName, reason, createdBy string) *ToolBlocklistEntry {
	t.Helper()
	entry, err := d.CreateToolBlocklistEntry(context.Background(), serverID, toolName, reason, createdBy)
	if err != nil {
		t.Fatalf("mustCreateBlocklistEntry(server=%q, tool=%q): %v", serverID, toolName, err)
	}
	return entry
}

// ---- CreateToolBlocklistEntry ------------------------------------------------

func TestCreateToolBlocklistEntry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		toolName  string
		reason    string
		createdBy string
		wantErr   error
		check     func(t *testing.T, got *ToolBlocklistEntry, serverID string)
	}{
		{
			name:      "all fields set returns populated entry",
			toolName:  "exec_shell",
			reason:    "dangerous tool",
			createdBy: "user-abc",
			wantErr:   nil,
			check: func(t *testing.T, got *ToolBlocklistEntry, serverID string) {
				t.Helper()
				if got.ID == "" {
					t.Error("ID is empty, want non-empty UUID")
				}
				if got.ServerID != serverID {
					t.Errorf("ServerID = %q, want %q", got.ServerID, serverID)
				}
				if got.ToolName != "exec_shell" {
					t.Errorf("ToolName = %q, want %q", got.ToolName, "exec_shell")
				}
				if got.Reason != "dangerous tool" {
					t.Errorf("Reason = %q, want %q", got.Reason, "dangerous tool")
				}
				if got.CreatedBy == nil || *got.CreatedBy != "user-abc" {
					t.Errorf("CreatedBy = %v, want %q", got.CreatedBy, "user-abc")
				}
				if got.CreatedAt == "" {
					t.Error("CreatedAt is empty")
				}
			},
		},
		{
			name:      "empty createdBy stores NULL",
			toolName:  "read_file",
			reason:    "restricted",
			createdBy: "",
			wantErr:   nil,
			check: func(t *testing.T, got *ToolBlocklistEntry, _ string) {
				t.Helper()
				if got.CreatedBy != nil {
					t.Errorf("CreatedBy = %v, want nil when createdBy is empty", got.CreatedBy)
				}
			},
		},
		{
			name:      "empty reason stores empty string",
			toolName:  "write_file",
			reason:    "",
			createdBy: "",
			wantErr:   nil,
			check: func(t *testing.T, got *ToolBlocklistEntry, _ string) {
				t.Helper()
				if got.Reason != "" {
					t.Errorf("Reason = %q, want empty string", got.Reason)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := openMigratedDB(t)
			server := mustCreateMCPServer(t, d, defaultMCPParams("blocklist-create-"+tc.toolName))

			got, err := d.CreateToolBlocklistEntry(context.Background(), server.ID, tc.toolName, tc.reason, tc.createdBy)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateToolBlocklistEntry() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.check != nil {
				tc.check(t, got, server.ID)
			}
		})
	}
}

func TestCreateToolBlocklistEntry_Duplicate(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("blocklist-dup"))

	mustCreateBlocklistEntry(t, d, server.ID, "exec_shell", "first insert", "")

	_, err := d.CreateToolBlocklistEntry(context.Background(), server.ID, "exec_shell", "duplicate", "")
	if !errors.Is(err, ErrConflict) {
		t.Errorf("CreateToolBlocklistEntry() duplicate error = %v, want ErrConflict", err)
	}
}

// ---- DeleteToolBlocklistEntry ------------------------------------------------

func TestDeleteToolBlocklistEntry(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("blocklist-delete"))

	mustCreateBlocklistEntry(t, d, server.ID, "exec_shell", "test", "")

	err := d.DeleteToolBlocklistEntry(context.Background(), server.ID, "exec_shell")
	if err != nil {
		t.Fatalf("DeleteToolBlocklistEntry() error = %v, want nil", err)
	}

	// Confirm the entry is gone.
	entries, err := d.ListToolBlocklist(context.Background(), server.ID)
	if err != nil {
		t.Fatalf("ListToolBlocklist() after delete error = %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ListToolBlocklist() after delete len = %d, want 0", len(entries))
	}
}

func TestDeleteToolBlocklistEntry_NotFound(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("blocklist-delete-notfound"))

	err := d.DeleteToolBlocklistEntry(context.Background(), server.ID, "nonexistent_tool")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteToolBlocklistEntry() error = %v, want ErrNotFound", err)
	}
}

// ---- ListToolBlocklist -------------------------------------------------------

func TestListToolBlocklist(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("blocklist-list"))

	mustCreateBlocklistEntry(t, d, server.ID, "write_file", "risky", "user-1")
	mustCreateBlocklistEntry(t, d, server.ID, "exec_shell", "dangerous", "user-2")
	mustCreateBlocklistEntry(t, d, server.ID, "delete_record", "irreversible", "")

	entries, err := d.ListToolBlocklist(context.Background(), server.ID)
	if err != nil {
		t.Fatalf("ListToolBlocklist() error = %v, want nil", err)
	}

	if len(entries) != 3 {
		t.Fatalf("ListToolBlocklist() len = %d, want 3", len(entries))
	}

	// Results must be ordered by tool_name ASC.
	wantOrder := []string{"delete_record", "exec_shell", "write_file"}
	for i, want := range wantOrder {
		if entries[i].ToolName != want {
			t.Errorf("entries[%d].ToolName = %q, want %q", i, entries[i].ToolName, want)
		}
	}

	// Verify each entry references the correct server.
	for _, e := range entries {
		if e.ServerID != server.ID {
			t.Errorf("entry ServerID = %q, want %q", e.ServerID, server.ID)
		}
		if e.ID == "" {
			t.Error("entry ID is empty")
		}
		if e.CreatedAt == "" {
			t.Error("entry CreatedAt is empty")
		}
	}
}

func TestListToolBlocklist_Empty(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("blocklist-list-empty"))

	entries, err := d.ListToolBlocklist(context.Background(), server.ID)
	if err != nil {
		t.Fatalf("ListToolBlocklist() error = %v, want nil", err)
	}
	if len(entries) != 0 {
		t.Errorf("ListToolBlocklist() len = %d, want 0 for server with no entries", len(entries))
	}
}

func TestListToolBlocklist_IsolatedByServer(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	serverA := mustCreateMCPServer(t, d, defaultMCPParams("blocklist-isolated-a"))
	serverB := mustCreateMCPServer(t, d, defaultMCPParams("blocklist-isolated-b"))

	mustCreateBlocklistEntry(t, d, serverA.ID, "exec_shell", "a only", "")
	mustCreateBlocklistEntry(t, d, serverB.ID, "read_file", "b only", "")

	entriesA, err := d.ListToolBlocklist(context.Background(), serverA.ID)
	if err != nil {
		t.Fatalf("ListToolBlocklist(serverA) error = %v", err)
	}
	if len(entriesA) != 1 || entriesA[0].ToolName != "exec_shell" {
		t.Errorf("ListToolBlocklist(serverA) = %v, want [{exec_shell}]", entriesA)
	}

	entriesB, err := d.ListToolBlocklist(context.Background(), serverB.ID)
	if err != nil {
		t.Fatalf("ListToolBlocklist(serverB) error = %v", err)
	}
	if len(entriesB) != 1 || entriesB[0].ToolName != "read_file" {
		t.Errorf("ListToolBlocklist(serverB) = %v, want [{read_file}]", entriesB)
	}
}

// ---- ListBlockedToolNames ----------------------------------------------------

func TestListBlockedToolNames(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("blocklist-names"))

	mustCreateBlocklistEntry(t, d, server.ID, "write_file", "", "")
	mustCreateBlocklistEntry(t, d, server.ID, "exec_shell", "", "")
	mustCreateBlocklistEntry(t, d, server.ID, "delete_record", "", "")

	names, err := d.ListBlockedToolNames(context.Background(), server.ID)
	if err != nil {
		t.Fatalf("ListBlockedToolNames() error = %v, want nil", err)
	}

	wantNames := []string{"delete_record", "exec_shell", "write_file"}
	if len(names) != len(wantNames) {
		t.Fatalf("ListBlockedToolNames() len = %d, want %d", len(names), len(wantNames))
	}
	for i, want := range wantNames {
		if names[i] != want {
			t.Errorf("names[%d] = %q, want %q", i, names[i], want)
		}
	}
}

func TestListBlockedToolNames_Empty(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	server := mustCreateMCPServer(t, d, defaultMCPParams("blocklist-names-empty"))

	names, err := d.ListBlockedToolNames(context.Background(), server.ID)
	if err != nil {
		t.Fatalf("ListBlockedToolNames() error = %v, want nil", err)
	}
	if len(names) != 0 {
		t.Errorf("ListBlockedToolNames() len = %d, want 0", len(names))
	}
}

func TestListBlockedToolNames_IsolatedByServer(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	serverA := mustCreateMCPServer(t, d, defaultMCPParams("blocklist-names-isolated-a"))
	serverB := mustCreateMCPServer(t, d, defaultMCPParams("blocklist-names-isolated-b"))

	mustCreateBlocklistEntry(t, d, serverA.ID, "exec_shell", "", "")
	mustCreateBlocklistEntry(t, d, serverB.ID, "read_file", "", "")
	mustCreateBlocklistEntry(t, d, serverB.ID, "write_file", "", "")

	namesA, err := d.ListBlockedToolNames(context.Background(), serverA.ID)
	if err != nil {
		t.Fatalf("ListBlockedToolNames(serverA) error = %v", err)
	}
	if len(namesA) != 1 || namesA[0] != "exec_shell" {
		t.Errorf("ListBlockedToolNames(serverA) = %v, want [exec_shell]", namesA)
	}

	namesB, err := d.ListBlockedToolNames(context.Background(), serverB.ID)
	if err != nil {
		t.Fatalf("ListBlockedToolNames(serverB) error = %v", err)
	}
	if len(namesB) != 2 {
		t.Fatalf("ListBlockedToolNames(serverB) len = %d, want 2", len(namesB))
	}
	if namesB[0] != "read_file" || namesB[1] != "write_file" {
		t.Errorf("ListBlockedToolNames(serverB) = %v, want [read_file write_file]", namesB)
	}
}
