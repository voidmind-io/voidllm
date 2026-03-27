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
// required — the FK is nullable. OrgID and TeamID are nil (global scope).
func defaultMCPParams(alias string) CreateMCPServerParams {
	return CreateMCPServerParams{
		Name:     "Test MCP " + alias,
		Alias:    alias,
		URL:      "https://mcp.example.com/" + alias,
		AuthType: "none",
	}
}

// mustCreateOrgForMCP creates an org for use in MCP server scope tests.
// It uses a unique slug derived from the provided suffix to avoid conflicts.
func mustCreateOrgForMCP(t *testing.T, d *DB, suffix string) *Org {
	t.Helper()
	return mustCreateOrg(t, d, CreateOrgParams{Name: "MCP Org " + suffix, Slug: "mcp-org-" + suffix})
}

// mustCreateTeamForMCP creates a team in the given org for use in MCP server scope tests.
func mustCreateTeamForMCP(t *testing.T, d *DB, orgID, suffix string) *Team {
	t.Helper()
	return mustCreateTeam(t, d, CreateTeamParams{OrgID: orgID, Name: "MCP Team " + suffix, Slug: "mcp-team-" + suffix})
}

// orgScopedMCPParams returns params for an org-scoped MCP server.
func orgScopedMCPParams(alias, orgID string) CreateMCPServerParams {
	p := defaultMCPParams(alias)
	p.OrgID = &orgID
	return p
}

// teamScopedMCPParams returns params for a team-scoped MCP server.
func teamScopedMCPParams(alias, orgID, teamID string) CreateMCPServerParams {
	p := defaultMCPParams(alias)
	p.OrgID = &orgID
	p.TeamID = &teamID
	return p
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
				// OrgID and TeamID are nil for global servers.
				if got.OrgID != nil {
					t.Errorf("OrgID = %v, want nil for global server", got.OrgID)
				}
				if got.TeamID != nil {
					t.Errorf("TeamID = %v, want nil for global server", got.TeamID)
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

func TestCreateMCPServer_OrgScoped(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	org := mustCreateOrgForMCP(t, d, "orgscoped")

	params := CreateMCPServerParams{
		Name:     "Org MCP",
		Alias:    "org-scoped",
		URL:      "https://mcp.org.example.com",
		AuthType: "none",
		OrgID:    &org.ID,
	}

	got, err := d.CreateMCPServer(context.Background(), params)
	if err != nil {
		t.Fatalf("CreateMCPServer() error = %v", err)
	}
	if got.OrgID == nil || *got.OrgID != org.ID {
		t.Errorf("OrgID = %v, want %q", got.OrgID, org.ID)
	}
	if got.TeamID != nil {
		t.Errorf("TeamID = %v, want nil for org-scoped server", got.TeamID)
	}
}

func TestCreateMCPServer_TeamScoped(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	org := mustCreateOrgForMCP(t, d, "teamscoped")
	team := mustCreateTeamForMCP(t, d, org.ID, "ts")

	params := CreateMCPServerParams{
		Name:     "Team MCP",
		Alias:    "team-scoped",
		URL:      "https://mcp.team.example.com",
		AuthType: "none",
		OrgID:    &org.ID,
		TeamID:   &team.ID,
	}

	got, err := d.CreateMCPServer(context.Background(), params)
	if err != nil {
		t.Fatalf("CreateMCPServer() error = %v", err)
	}
	if got.OrgID == nil || *got.OrgID != org.ID {
		t.Errorf("OrgID = %v, want %q", got.OrgID, org.ID)
	}
	if got.TeamID == nil || *got.TeamID != team.ID {
		t.Errorf("TeamID = %v, want %q", got.TeamID, team.ID)
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

// Same alias is allowed across different scopes because the unique index is on
// (ifnull(org_id,”), ifnull(team_id,”), alias) covering non-deleted rows.
func TestCreateMCPServer_SameAliasAcrossScopes(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	org := mustCreateOrgForMCP(t, d, "crossscope")
	team := mustCreateTeamForMCP(t, d, org.ID, "cs")

	// Global server with alias "shared".
	mustCreateMCPServer(t, d, defaultMCPParams("shared"))

	// Org-scoped server with the same alias — should succeed.
	_, err := d.CreateMCPServer(context.Background(), orgScopedMCPParams("shared", org.ID))
	if err != nil {
		t.Errorf("CreateMCPServer() org-scoped with same alias error = %v, want nil", err)
	}

	// Team-scoped server with the same alias — should succeed.
	_, err = d.CreateMCPServer(context.Background(), teamScopedMCPParams("shared", org.ID, team.ID))
	if err != nil {
		t.Errorf("CreateMCPServer() team-scoped with same alias error = %v, want nil", err)
	}

	// Duplicate org-scoped server for same org — should fail.
	_, err = d.CreateMCPServer(context.Background(), orgScopedMCPParams("shared", org.ID))
	if !errors.Is(err, ErrConflict) {
		t.Errorf("CreateMCPServer() duplicate org-scoped error = %v, want ErrConflict", err)
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
			name: "existing global alias returns server",
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
		{
			name: "org-scoped server not returned by global lookup",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrgForMCP(t, d, "byalias-orgonly")
				mustCreateMCPServer(t, d, orgScopedMCPParams("org-only", org.ID))
				return "org-only"
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

// ---- GetMCPServerByAliasScoped ----------------------------------------------

func TestGetMCPServerByAliasScoped(t *testing.T) {
	t.Parallel()

	t.Run("returns team-scoped server when team matches", func(t *testing.T) {
		t.Parallel()
		d := openMigratedDB(t)
		org := mustCreateOrgForMCP(t, d, "scoped-team-wins")
		team := mustCreateTeamForMCP(t, d, org.ID, "tw")

		mustCreateMCPServer(t, d, defaultMCPParams("tool"))
		mustCreateMCPServer(t, d, orgScopedMCPParams("tool", org.ID))
		mustCreateMCPServer(t, d, teamScopedMCPParams("tool", org.ID, team.ID))

		got, err := d.GetMCPServerByAliasScoped(context.Background(), "tool", org.ID, team.ID)
		if err != nil {
			t.Fatalf("GetMCPServerByAliasScoped() error = %v", err)
		}
		if got.TeamID == nil || *got.TeamID != team.ID {
			t.Errorf("TeamID = %v, want %q (team-scoped wins)", got.TeamID, team.ID)
		}
	})

	t.Run("falls back to org-scoped when no team match", func(t *testing.T) {
		t.Parallel()
		d := openMigratedDB(t)
		org := mustCreateOrgForMCP(t, d, "scoped-org-wins")

		mustCreateMCPServer(t, d, defaultMCPParams("tool2"))
		mustCreateMCPServer(t, d, orgScopedMCPParams("tool2", org.ID))

		got, err := d.GetMCPServerByAliasScoped(context.Background(), "tool2", org.ID, "nonexistent-team")
		if err != nil {
			t.Fatalf("GetMCPServerByAliasScoped() error = %v", err)
		}
		if got.OrgID == nil || *got.OrgID != org.ID {
			t.Errorf("OrgID = %v, want %q (org-scoped wins over global)", got.OrgID, org.ID)
		}
		if got.TeamID != nil {
			t.Errorf("TeamID = %v, want nil", got.TeamID)
		}
	})

	t.Run("falls back to global when no org or team match", func(t *testing.T) {
		t.Parallel()
		d := openMigratedDB(t)

		mustCreateMCPServer(t, d, defaultMCPParams("tool3"))

		got, err := d.GetMCPServerByAliasScoped(context.Background(), "tool3", "org-c", "team-c")
		if err != nil {
			t.Fatalf("GetMCPServerByAliasScoped() error = %v", err)
		}
		if got.OrgID != nil {
			t.Errorf("OrgID = %v, want nil (global server)", got.OrgID)
		}
		if got.TeamID != nil {
			t.Errorf("TeamID = %v, want nil (global server)", got.TeamID)
		}
	})

	t.Run("returns ErrNotFound when no matching server exists", func(t *testing.T) {
		t.Parallel()
		d := openMigratedDB(t)

		_, err := d.GetMCPServerByAliasScoped(context.Background(), "unknown", "org-z", "team-z")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("GetMCPServerByAliasScoped() error = %v, want ErrNotFound", err)
		}
	})

	t.Run("empty orgID and teamID still returns global server", func(t *testing.T) {
		t.Parallel()
		d := openMigratedDB(t)

		mustCreateMCPServer(t, d, defaultMCPParams("global-only"))

		got, err := d.GetMCPServerByAliasScoped(context.Background(), "global-only", "", "")
		if err != nil {
			t.Fatalf("GetMCPServerByAliasScoped() error = %v", err)
		}
		if got.OrgID != nil || got.TeamID != nil {
			t.Errorf("expected global server, got OrgID=%v TeamID=%v", got.OrgID, got.TeamID)
		}
	})
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

func TestListMCPServers_GlobalOnly(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	org := mustCreateOrgForMCP(t, d, "listglobal")

	// Insert one global and one org-scoped server.
	mustCreateMCPServer(t, d, defaultMCPParams("global-vis"))
	mustCreateMCPServer(t, d, orgScopedMCPParams("org-vis", org.ID))

	servers, err := d.ListMCPServers(context.Background())
	if err != nil {
		t.Fatalf("ListMCPServers() error = %v", err)
	}

	for _, s := range servers {
		if s.OrgID != nil || s.TeamID != nil {
			t.Errorf("ListMCPServers() returned non-global server %q (org=%v, team=%v)",
				s.Alias, s.OrgID, s.TeamID)
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

// ---- ListMCPServersByOrg ----------------------------------------------------

func TestListMCPServersByOrg(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	org1 := mustCreateOrgForMCP(t, d, "listorg1")
	org2 := mustCreateOrgForMCP(t, d, "listorg2")
	team1 := mustCreateTeamForMCP(t, d, org1.ID, "lo1")

	// Global servers visible to everyone.
	mustCreateMCPServer(t, d, defaultMCPParams("global-a"))
	mustCreateMCPServer(t, d, defaultMCPParams("global-b"))

	// Org-scoped for org1.
	mustCreateMCPServer(t, d, orgScopedMCPParams("org1-server", org1.ID))

	// Org-scoped for org2 — must not appear in org1 results.
	mustCreateMCPServer(t, d, orgScopedMCPParams("org2-server", org2.ID))

	// Team-scoped for org1 — must not appear (ListMCPServersByOrg excludes team-scoped).
	mustCreateMCPServer(t, d, teamScopedMCPParams("team-server", org1.ID, team1.ID))

	servers, err := d.ListMCPServersByOrg(context.Background(), org1.ID)
	if err != nil {
		t.Fatalf("ListMCPServersByOrg() error = %v", err)
	}

	aliases := make(map[string]bool, len(servers))
	for _, s := range servers {
		aliases[s.Alias] = true
	}

	for _, want := range []string{"global-a", "global-b", "org1-server"} {
		if !aliases[want] {
			t.Errorf("ListMCPServersByOrg() missing %q", want)
		}
	}
	for _, unwanted := range []string{"org2-server", "team-server"} {
		if aliases[unwanted] {
			t.Errorf("ListMCPServersByOrg() unexpectedly contains %q", unwanted)
		}
	}
}

// ---- ListMCPServersByTeam ---------------------------------------------------

func TestListMCPServersByTeam(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	org1 := mustCreateOrgForMCP(t, d, "listteam1")
	org2 := mustCreateOrgForMCP(t, d, "listteam2")
	teamA := mustCreateTeamForMCP(t, d, org1.ID, "lt-a")
	teamB := mustCreateTeamForMCP(t, d, org1.ID, "lt-b")

	// Global servers.
	mustCreateMCPServer(t, d, defaultMCPParams("global-t"))

	// Org-scoped for org1.
	mustCreateMCPServer(t, d, orgScopedMCPParams("org1-t", org1.ID))

	// Org-scoped for org2 — must not appear.
	mustCreateMCPServer(t, d, orgScopedMCPParams("org2-t", org2.ID))

	// Team-scoped for (org1, teamA).
	mustCreateMCPServer(t, d, teamScopedMCPParams("team-a-server", org1.ID, teamA.ID))

	// Team-scoped for (org1, teamB) — must not appear.
	mustCreateMCPServer(t, d, teamScopedMCPParams("team-b-server", org1.ID, teamB.ID))

	servers, err := d.ListMCPServersByTeam(context.Background(), teamA.ID, org1.ID)
	if err != nil {
		t.Fatalf("ListMCPServersByTeam() error = %v", err)
	}

	aliases := make(map[string]bool, len(servers))
	for _, s := range servers {
		aliases[s.Alias] = true
	}

	for _, want := range []string{"global-t", "org1-t", "team-a-server"} {
		if !aliases[want] {
			t.Errorf("ListMCPServersByTeam() missing %q", want)
		}
	}
	for _, unwanted := range []string{"org2-t", "team-b-server"} {
		if aliases[unwanted] {
			t.Errorf("ListMCPServersByTeam() unexpectedly contains %q", unwanted)
		}
	}
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
