package db

import (
	"context"
	"errors"
	"testing"
)

// mustCreateMCPServer creates an MCP server and fails the test on error.
func mustCreateMCPServer(t *testing.T, d *DB, params CreateMCPServerParams) *MCPServer {
	t.Helper()
	s, err := d.CreateMCPServer(context.Background(), params)
	if err != nil {
		t.Fatalf("mustCreateMCPServer(%q): %v", params.Alias, err)
	}
	return s
}

// defaultMCPParams returns a minimal valid CreateMCPServerParams for the given alias.
// CreatedBy is intentionally left empty (maps to NULL) so that no users row is
// required — the FK is nullable.
func defaultMCPParams(alias string) CreateMCPServerParams {
	return CreateMCPServerParams{
		Name:     "Test MCP " + alias,
		Alias:    alias,
		URL:      "https://mcp.example.com/" + alias,
		AuthType: "none",
	}
}

// ---- CreateMCPServer --------------------------------------------------------

func TestCreateMCPServer(t *testing.T) {
	t.Parallel()

	enc := "encrypted-token-value"

	tests := []struct {
		name      string
		params    CreateMCPServerParams
		wantErr   error
		checkFunc func(t *testing.T, got *MCPServer, params CreateMCPServerParams)
	}{
		{
			name: "all fields set returns populated server",
			params: CreateMCPServerParams{
				// CreatedBy is empty so we do not need a users row — the FK is nullable.
				Name:         "GitHub MCP",
				Alias:        "github",
				URL:          "https://mcp.github.com/v1",
				AuthType:     "bearer",
				AuthHeader:   "",
				AuthTokenEnc: &enc,
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *MCPServer, params CreateMCPServerParams) {
				t.Helper()
				if got.ID == "" {
					t.Error("ID is empty, want non-empty UUID")
				}
				if got.Name != params.Name {
					t.Errorf("Name = %q, want %q", got.Name, params.Name)
				}
				if got.Alias != params.Alias {
					t.Errorf("Alias = %q, want %q", got.Alias, params.Alias)
				}
				if got.URL != params.URL {
					t.Errorf("URL = %q, want %q", got.URL, params.URL)
				}
				if got.AuthType != params.AuthType {
					t.Errorf("AuthType = %q, want %q", got.AuthType, params.AuthType)
				}
				if got.AuthTokenEnc == nil || *got.AuthTokenEnc != enc {
					t.Errorf("AuthTokenEnc = %v, want %q", got.AuthTokenEnc, enc)
				}
				// created_by is NULL because CreatedBy was empty.
				if got.CreatedBy != nil {
					t.Errorf("CreatedBy = %v, want nil", got.CreatedBy)
				}
				if !got.IsActive {
					t.Error("IsActive = false, want true for new server")
				}
				if got.CreatedAt == "" {
					t.Error("CreatedAt is empty")
				}
				if got.UpdatedAt == "" {
					t.Error("UpdatedAt is empty")
				}
				if got.DeletedAt != nil {
					t.Errorf("DeletedAt = %v, want nil", got.DeletedAt)
				}
			},
		},
		{
			name: "header auth type stores header name",
			params: CreateMCPServerParams{
				Name:       "Notion MCP",
				Alias:      "notion",
				URL:        "https://mcp.notion.so/v1",
				AuthType:   "header",
				AuthHeader: "X-Notion-Token",
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *MCPServer, params CreateMCPServerParams) {
				t.Helper()
				if got.AuthHeader != params.AuthHeader {
					t.Errorf("AuthHeader = %q, want %q", got.AuthHeader, params.AuthHeader)
				}
			},
		},
		{
			name: "no auth token leaves AuthTokenEnc nil",
			params: CreateMCPServerParams{
				Name:     "Public MCP",
				Alias:    "public",
				URL:      "https://mcp.public.example.com",
				AuthType: "none",
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *MCPServer, _ CreateMCPServerParams) {
				t.Helper()
				if got.AuthTokenEnc != nil {
					t.Errorf("AuthTokenEnc = %v, want nil", got.AuthTokenEnc)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := openMigratedDB(t)
			got, err := d.CreateMCPServer(context.Background(), tc.params)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateMCPServer() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, got, tc.params)
			}
		})
	}
}

func TestCreateMCPServer_DuplicateAlias(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	mustCreateMCPServer(t, d, defaultMCPParams("dup-alias"))

	_, err := d.CreateMCPServer(context.Background(), defaultMCPParams("dup-alias"))
	if !errors.Is(err, ErrConflict) {
		t.Errorf("CreateMCPServer() error = %v, want ErrConflict", err)
	}
}

// ---- GetMCPServer -----------------------------------------------------------

func TestGetMCPServer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string // returns id to look up
		wantErr error
	}{
		{
			name: "existing server returns server",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				s := mustCreateMCPServer(t, d, defaultMCPParams("get-found"))
				return s.ID
			},
			wantErr: nil,
		},
		{
			name: "non-existent id returns ErrNotFound",
			setup: func(_ *testing.T, _ *DB) string {
				return "00000000-0000-0000-0000-000000000000"
			},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := openMigratedDB(t)
			id := tc.setup(t, d)

			got, err := d.GetMCPServer(context.Background(), id)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetMCPServer() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				if got == nil {
					t.Fatal("GetMCPServer() = nil, want non-nil")
				}
				if got.ID != id {
					t.Errorf("ID = %q, want %q", got.ID, id)
				}
			}
		})
	}
}

// ---- GetMCPServerByAlias ----------------------------------------------------

func TestGetMCPServerByAlias(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string // returns alias to look up
		wantErr error
	}{
		{
			name: "existing alias returns server",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				mustCreateMCPServer(t, d, defaultMCPParams("by-alias-ok"))
				return "by-alias-ok"
			},
			wantErr: nil,
		},
		{
			name: "unknown alias returns ErrNotFound",
			setup: func(_ *testing.T, _ *DB) string {
				return "does-not-exist"
			},
			wantErr: ErrNotFound,
		},
		{
			name: "soft-deleted server returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				s := mustCreateMCPServer(t, d, defaultMCPParams("by-alias-deleted"))
				if err := d.DeleteMCPServer(context.Background(), s.ID); err != nil {
					t.Fatalf("DeleteMCPServer: %v", err)
				}
				return "by-alias-deleted"
			},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := openMigratedDB(t)
			alias := tc.setup(t, d)

			got, err := d.GetMCPServerByAlias(context.Background(), alias)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetMCPServerByAlias() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				if got == nil {
					t.Fatal("GetMCPServerByAlias() = nil, want non-nil")
				}
				if got.Alias != alias {
					t.Errorf("Alias = %q, want %q", got.Alias, alias)
				}
			}
		})
	}
}

// ---- ListMCPServers ---------------------------------------------------------

func TestListMCPServers_OrderedByAlias(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)

	// Insert in reverse alphabetical order to verify ORDER BY alias ASC.
	for _, alias := range []string{"zebra", "alpha", "mango"} {
		mustCreateMCPServer(t, d, defaultMCPParams(alias))
	}

	servers, err := d.ListMCPServers(context.Background())
	if err != nil {
		t.Fatalf("ListMCPServers() error = %v", err)
	}
	if len(servers) != 3 {
		t.Fatalf("ListMCPServers() count = %d, want 3", len(servers))
	}

	want := []string{"alpha", "mango", "zebra"}
	for i, s := range servers {
		if s.Alias != want[i] {
			t.Errorf("servers[%d].Alias = %q, want %q", i, s.Alias, want[i])
		}
	}
}

func TestListMCPServers_ExcludesInactive(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)

	// Create one active and one that we will deactivate by directly updating
	// the is_active flag (the DB layer's UpdateMCPServer does not expose is_active,
	// so we use raw SQL to simulate a deactivated record).
	mustCreateMCPServer(t, d, defaultMCPParams("list-active"))
	inactive := mustCreateMCPServer(t, d, defaultMCPParams("list-inactive"))

	_, err := d.SQL().ExecContext(context.Background(),
		"UPDATE mcp_servers SET is_active = 0 WHERE id = ?", inactive.ID)
	if err != nil {
		t.Fatalf("deactivate server: %v", err)
	}

	servers, err := d.ListMCPServers(context.Background())
	if err != nil {
		t.Fatalf("ListMCPServers() error = %v", err)
	}

	for _, s := range servers {
		if s.ID == inactive.ID {
			t.Errorf("ListMCPServers() returned inactive server %q, want it excluded", s.Alias)
		}
	}

	found := false
	for _, s := range servers {
		if s.Alias == "list-active" {
			found = true
		}
	}
	if !found {
		t.Error("ListMCPServers() did not return the active server")
	}
}

func TestListMCPServers_ExcludesSoftDeleted(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)

	active := mustCreateMCPServer(t, d, defaultMCPParams("list-nodelete"))
	deleted := mustCreateMCPServer(t, d, defaultMCPParams("list-willdelete"))

	if err := d.DeleteMCPServer(context.Background(), deleted.ID); err != nil {
		t.Fatalf("DeleteMCPServer: %v", err)
	}

	servers, err := d.ListMCPServers(context.Background())
	if err != nil {
		t.Fatalf("ListMCPServers() error = %v", err)
	}

	for _, s := range servers {
		if s.ID == deleted.ID {
			t.Errorf("ListMCPServers() returned soft-deleted server %q", s.Alias)
		}
	}
	_ = active
}

// ---- UpdateMCPServer --------------------------------------------------------

func TestUpdateMCPServer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		params    UpdateMCPServerParams
		wantErr   error
		checkFunc func(t *testing.T, original, got *MCPServer)
	}{
		{
			name: "partial update changes only supplied fields",
			params: UpdateMCPServerParams{
				Name: ptr("Updated Name"),
				URL:  ptr("https://new.example.com/mcp"),
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, original, got *MCPServer) {
				t.Helper()
				if got.Name != "Updated Name" {
					t.Errorf("Name = %q, want %q", got.Name, "Updated Name")
				}
				if got.URL != "https://new.example.com/mcp" {
					t.Errorf("URL = %q, want %q", got.URL, "https://new.example.com/mcp")
				}
				// Alias must be unchanged.
				if got.Alias != original.Alias {
					t.Errorf("Alias = %q, want original %q", got.Alias, original.Alias)
				}
			},
		},
		{
			name:   "empty params returns server unchanged",
			params: UpdateMCPServerParams{
				// All nil — no-op update.
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, original, got *MCPServer) {
				t.Helper()
				if got.Name != original.Name {
					t.Errorf("Name = %q, want original %q", got.Name, original.Name)
				}
				if got.URL != original.URL {
					t.Errorf("URL = %q, want original %q", got.URL, original.URL)
				}
			},
		},
		{
			name: "update auth token sets auth_token_enc",
			params: UpdateMCPServerParams{
				AuthTokenEnc: ptr("new-encrypted-token"),
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, _ *MCPServer, got *MCPServer) {
				t.Helper()
				if got.AuthTokenEnc == nil || *got.AuthTokenEnc != "new-encrypted-token" {
					t.Errorf("AuthTokenEnc = %v, want %q", got.AuthTokenEnc, "new-encrypted-token")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := openMigratedDB(t)
			original := mustCreateMCPServer(t, d, defaultMCPParams("update-test-"+tc.name))

			got, err := d.UpdateMCPServer(context.Background(), original.ID, tc.params)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("UpdateMCPServer() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, original, got)
			}
		})
	}
}

func TestUpdateMCPServer_NotFound(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)

	_, err := d.UpdateMCPServer(context.Background(), "00000000-0000-0000-0000-000000000000",
		UpdateMCPServerParams{Name: ptr("ghost")})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateMCPServer() error = %v, want ErrNotFound", err)
	}
}

func TestUpdateMCPServer_DuplicateAlias(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	mustCreateMCPServer(t, d, defaultMCPParams("alias-taken"))
	second := mustCreateMCPServer(t, d, defaultMCPParams("alias-free"))

	_, err := d.UpdateMCPServer(context.Background(), second.ID,
		UpdateMCPServerParams{Alias: ptr("alias-taken")})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("UpdateMCPServer() error = %v, want ErrConflict", err)
	}
}

// ---- DeleteMCPServer --------------------------------------------------------

func TestDeleteMCPServer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string // returns id to delete
		wantErr error
	}{
		{
			name: "existing server is soft-deleted and deleted_at is set",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				s := mustCreateMCPServer(t, d, defaultMCPParams("delete-ok"))
				return s.ID
			},
			wantErr: nil,
		},
		{
			name: "non-existent id returns ErrNotFound",
			setup: func(_ *testing.T, _ *DB) string {
				return "00000000-0000-0000-0000-000000000001"
			},
			wantErr: ErrNotFound,
		},
		{
			name: "already-deleted server returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				s := mustCreateMCPServer(t, d, defaultMCPParams("delete-twice"))
				if err := d.DeleteMCPServer(context.Background(), s.ID); err != nil {
					t.Fatalf("first DeleteMCPServer: %v", err)
				}
				return s.ID
			},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := openMigratedDB(t)
			id := tc.setup(t, d)

			err := d.DeleteMCPServer(context.Background(), id)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("DeleteMCPServer() error = %v, wantErr %v", err, tc.wantErr)
			}

			if tc.wantErr == nil {
				// Verify soft-delete: GetMCPServer must return ErrNotFound.
				_, getErr := d.GetMCPServer(context.Background(), id)
				if !errors.Is(getErr, ErrNotFound) {
					t.Errorf("GetMCPServer() after delete error = %v, want ErrNotFound", getErr)
				}

				// Verify deleted_at is actually set in the underlying row.
				var deletedAt *string
				row := d.SQL().QueryRowContext(context.Background(),
					"SELECT deleted_at FROM mcp_servers WHERE id = ?", id)
				if scanErr := row.Scan(&deletedAt); scanErr != nil {
					t.Fatalf("scan deleted_at: %v", scanErr)
				}
				if deletedAt == nil {
					t.Error("deleted_at is NULL after delete, want non-NULL")
				}
			}
		})
	}
}
