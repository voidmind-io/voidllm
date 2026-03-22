package db

import (
	"context"
	"errors"
	"testing"
)

// mustCreateTeam creates a team and fails the test if an error occurs.
func mustCreateTeam(t *testing.T, d *DB, params CreateTeamParams) *Team {
	t.Helper()
	team, err := d.CreateTeam(context.Background(), params)
	if err != nil {
		t.Fatalf("mustCreateTeam(%q): %v", params.Slug, err)
	}
	return team
}

// ---- CreateTeam -------------------------------------------------------------

func TestCreateTeam(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, d *DB) CreateTeamParams
		wantErr   error
		checkFunc func(t *testing.T, got *Team, params CreateTeamParams)
	}{
		{
			name: "valid params returns team with fields set",
			setup: func(t *testing.T, d *DB) CreateTeamParams {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org A", Slug: "org-a-ct"})
				return CreateTeamParams{
					OrgID:             org.ID,
					Name:              "Engineering",
					Slug:              "engineering",
					DailyTokenLimit:   5000,
					MonthlyTokenLimit: 100000,
					RequestsPerMinute: 60,
					RequestsPerDay:    1440,
				}
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *Team, params CreateTeamParams) {
				t.Helper()
				if got.ID == "" {
					t.Error("ID is empty, want non-empty UUID")
				}
				if got.OrgID != params.OrgID {
					t.Errorf("OrgID = %q, want %q", got.OrgID, params.OrgID)
				}
				if got.Name != params.Name {
					t.Errorf("Name = %q, want %q", got.Name, params.Name)
				}
				if got.Slug != params.Slug {
					t.Errorf("Slug = %q, want %q", got.Slug, params.Slug)
				}
				if got.DailyTokenLimit != params.DailyTokenLimit {
					t.Errorf("DailyTokenLimit = %d, want %d", got.DailyTokenLimit, params.DailyTokenLimit)
				}
				if got.MonthlyTokenLimit != params.MonthlyTokenLimit {
					t.Errorf("MonthlyTokenLimit = %d, want %d", got.MonthlyTokenLimit, params.MonthlyTokenLimit)
				}
				if got.RequestsPerMinute != params.RequestsPerMinute {
					t.Errorf("RequestsPerMinute = %d, want %d", got.RequestsPerMinute, params.RequestsPerMinute)
				}
				if got.RequestsPerDay != params.RequestsPerDay {
					t.Errorf("RequestsPerDay = %d, want %d", got.RequestsPerDay, params.RequestsPerDay)
				}
				if got.CreatedAt == "" {
					t.Error("CreatedAt is empty, want a timestamp")
				}
				if got.UpdatedAt == "" {
					t.Error("UpdatedAt is empty, want a timestamp")
				}
				if got.DeletedAt != nil {
					t.Errorf("DeletedAt = %v, want nil", got.DeletedAt)
				}
			},
		},
		{
			name: "duplicate slug in same org returns ErrConflict",
			setup: func(t *testing.T, d *DB) CreateTeamParams {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org Dup", Slug: "org-dup-ct"})
				mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "First", Slug: "shared-slug"})
				return CreateTeamParams{OrgID: org.ID, Name: "Second", Slug: "shared-slug"}
			},
			wantErr: ErrConflict,
		},
		{
			name: "same slug in different org succeeds",
			setup: func(t *testing.T, d *DB) CreateTeamParams {
				t.Helper()
				org1 := mustCreateOrg(t, d, CreateOrgParams{Name: "Org X", Slug: "org-x-ct"})
				org2 := mustCreateOrg(t, d, CreateOrgParams{Name: "Org Y", Slug: "org-y-ct"})
				mustCreateTeam(t, d, CreateTeamParams{OrgID: org1.ID, Name: "Backend", Slug: "backend"})
				return CreateTeamParams{OrgID: org2.ID, Name: "Backend", Slug: "backend"}
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *Team, params CreateTeamParams) {
				t.Helper()
				if got.ID == "" {
					t.Error("ID is empty, want non-empty UUID")
				}
				if got.OrgID != params.OrgID {
					t.Errorf("OrgID = %q, want %q", got.OrgID, params.OrgID)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			params := tc.setup(t, d)
			got, err := d.CreateTeam(context.Background(), params)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateTeam() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, got, params)
			}
		})
	}
}

// ---- GetTeam ----------------------------------------------------------------

func TestGetTeam(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string // returns the ID to look up
		wantErr error
	}{
		{
			name: "existing team returns correct data",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org Get", Slug: "org-get-t"})
				team := mustCreateTeam(t, d, CreateTeamParams{
					OrgID: org.ID,
					Name:  "Platform",
					Slug:  "platform",
				})
				return team.ID
			},
			wantErr: nil,
		},
		{
			name: "non-existent ID returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				return "00000000-0000-0000-0000-000000000000"
			},
			wantErr: ErrNotFound,
		},
		{
			name: "soft-deleted team returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org Del", Slug: "org-del-t"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Gone Team", Slug: "gone-team"})
				if err := d.DeleteTeam(context.Background(), team.ID); err != nil {
					t.Fatalf("DeleteTeam(): %v", err)
				}
				return team.ID
			},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			id := tc.setup(t, d)
			got, err := d.GetTeam(context.Background(), id)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetTeam() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				if got == nil {
					t.Fatal("GetTeam() returned nil, want non-nil Team")
				}
				if got.ID != id {
					t.Errorf("GetTeam().ID = %q, want %q", got.ID, id)
				}
				if got.Name != "Platform" {
					t.Errorf("GetTeam().Name = %q, want %q", got.Name, "Platform")
				}
			}
		})
	}
}

// ---- ListTeams --------------------------------------------------------------

func TestListTeams_OnlyReturnsTargetOrg(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org1 := mustCreateOrg(t, d, CreateOrgParams{Name: "Org List 1", Slug: "org-list-1"})
	org2 := mustCreateOrg(t, d, CreateOrgParams{Name: "Org List 2", Slug: "org-list-2"})

	mustCreateTeam(t, d, CreateTeamParams{OrgID: org1.ID, Name: "Alpha", Slug: "alpha-lt"})
	mustCreateTeam(t, d, CreateTeamParams{OrgID: org1.ID, Name: "Beta", Slug: "beta-lt"})
	mustCreateTeam(t, d, CreateTeamParams{OrgID: org2.ID, Name: "Gamma", Slug: "gamma-lt"})

	teams, err := d.ListTeams(ctx, org1.ID, "", 100, false)
	if err != nil {
		t.Fatalf("ListTeams() error = %v", err)
	}
	if len(teams) != 2 {
		t.Errorf("ListTeams() len = %d, want 2", len(teams))
	}
	for _, team := range teams {
		if team.OrgID != org1.ID {
			t.Errorf("ListTeams() returned team with OrgID = %q, want %q", team.OrgID, org1.ID)
		}
	}
}

func TestListTeams_CursorPagination(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org Pag", Slug: "org-pag-lt"})
	for i := range 3 {
		mustCreateTeam(t, d, CreateTeamParams{
			OrgID: org.ID,
			Name:  "Team",
			Slug:  "team-pag-" + string(rune('a'+i)),
		})
	}

	// First page: limit=2, expect 2 results.
	page1, err := d.ListTeams(ctx, org.ID, "", 2, false)
	if err != nil {
		t.Fatalf("ListTeams page1 error = %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}

	// Second page: cursor from last of page1, expect 1 result.
	page2, err := d.ListTeams(ctx, org.ID, page1[len(page1)-1].ID, 2, false)
	if err != nil {
		t.Fatalf("ListTeams page2 error = %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("page2 len = %d, want 1", len(page2))
	}

	// All IDs must be distinct and in ascending order.
	all := append(page1, page2...)
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Errorf("IDs not in ascending order: all[%d].ID=%q <= all[%d].ID=%q",
				i, all[i].ID, i-1, all[i-1].ID)
		}
	}
}

func TestListTeams_ExcludesSoftDeleted(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org Excl", Slug: "org-excl-lt"})
	live := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Live", Slug: "live-team"})
	gone := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Gone", Slug: "gone-team-excl"})

	if err := d.DeleteTeam(ctx, gone.ID); err != nil {
		t.Fatalf("DeleteTeam(): %v", err)
	}

	teams, err := d.ListTeams(ctx, org.ID, "", 100, false)
	if err != nil {
		t.Fatalf("ListTeams() error = %v", err)
	}
	if len(teams) != 1 {
		t.Fatalf("ListTeams(includeDeleted=false) len = %d, want 1", len(teams))
	}
	if teams[0].ID != live.ID {
		t.Errorf("ListTeams returned team ID %q, want %q", teams[0].ID, live.ID)
	}
}

func TestListTeams_IncludesSoftDeleted(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org Incl", Slug: "org-incl-lt"})
	mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Live", Slug: "live-team-incl"})
	gone := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Gone", Slug: "gone-team-incl"})

	if err := d.DeleteTeam(ctx, gone.ID); err != nil {
		t.Fatalf("DeleteTeam(): %v", err)
	}

	teams, err := d.ListTeams(ctx, org.ID, "", 100, true)
	if err != nil {
		t.Fatalf("ListTeams() error = %v", err)
	}
	if len(teams) != 2 {
		t.Fatalf("ListTeams(includeDeleted=true) len = %d, want 2", len(teams))
	}

	var foundDeleted bool
	for _, tm := range teams {
		if tm.ID == gone.ID {
			foundDeleted = true
			if tm.DeletedAt == nil {
				t.Error("deleted team has nil DeletedAt, want a timestamp")
			}
		}
	}
	if !foundDeleted {
		t.Error("deleted team not found in ListTeams(includeDeleted=true) results")
	}
}

// ---- UpdateTeam -------------------------------------------------------------

func TestUpdateTeam(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, d *DB) *Team
		params    UpdateTeamParams
		wantErr   error
		checkFunc func(t *testing.T, original, got *Team)
	}{
		{
			name: "update name only leaves slug unchanged",
			setup: func(t *testing.T, d *DB) *Team {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org UT1", Slug: "org-ut1"})
				return mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Old Name", Slug: "old-name-ut"})
			},
			params:  UpdateTeamParams{Name: ptr("New Name")},
			wantErr: nil,
			checkFunc: func(t *testing.T, original, got *Team) {
				t.Helper()
				if got.Name != "New Name" {
					t.Errorf("Name = %q, want %q", got.Name, "New Name")
				}
				if got.Slug != original.Slug {
					t.Errorf("Slug = %q, want %q (unchanged)", got.Slug, original.Slug)
				}
			},
		},
		{
			name: "update slug conflict in same org returns ErrConflict",
			setup: func(t *testing.T, d *DB) *Team {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org UT2", Slug: "org-ut2"})
				mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Taken", Slug: "taken-slug-ut"})
				return mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Target", Slug: "target-slug-ut"})
			},
			params:  UpdateTeamParams{Slug: ptr("taken-slug-ut")},
			wantErr: ErrConflict,
		},
		{
			name: "non-existent ID returns ErrNotFound",
			setup: func(t *testing.T, d *DB) *Team {
				return &Team{ID: "00000000-0000-0000-0000-000000000000"}
			},
			params:  UpdateTeamParams{Name: ptr("Ghost")},
			wantErr: ErrNotFound,
		},
		{
			name: "no fields set returns team unchanged",
			setup: func(t *testing.T, d *DB) *Team {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org UT3", Slug: "org-ut3"})
				return mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Stable Team", Slug: "stable-team-ut"})
			},
			params:  UpdateTeamParams{},
			wantErr: nil,
			checkFunc: func(t *testing.T, original, got *Team) {
				t.Helper()
				if got.Name != original.Name {
					t.Errorf("Name = %q, want %q", got.Name, original.Name)
				}
				if got.Slug != original.Slug {
					t.Errorf("Slug = %q, want %q", got.Slug, original.Slug)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			original := tc.setup(t, d)
			got, err := d.UpdateTeam(context.Background(), original.ID, tc.params)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("UpdateTeam() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, original, got)
			}
		})
	}
}

// ---- DeleteTeam -------------------------------------------------------------

func TestDeleteTeam(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string
		wantErr error
	}{
		{
			name: "delete existing team soft-deletes and GetTeam returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org DT1", Slug: "org-dt1"})
				return mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "To Delete", Slug: "to-delete-dt"}).ID
			},
			wantErr: nil,
		},
		{
			name: "delete non-existent ID returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
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
			err := d.DeleteTeam(context.Background(), id)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("DeleteTeam() error = %v, wantErr %v", err, tc.wantErr)
			}

			// On success, verify GetTeam now returns ErrNotFound.
			if tc.wantErr == nil {
				_, getErr := d.GetTeam(context.Background(), id)
				if !errors.Is(getErr, ErrNotFound) {
					t.Errorf("GetTeam() after DeleteTeam() error = %v, want ErrNotFound", getErr)
				}
			}
		})
	}
}
