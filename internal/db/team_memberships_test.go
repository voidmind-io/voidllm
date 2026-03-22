package db

import (
	"context"
	"errors"
	"testing"
)

// mustCreateTeamMembership creates a team membership and fatals the test on error.
func mustCreateTeamMembership(t *testing.T, d *DB, params CreateTeamMembershipParams) *TeamMembership {
	t.Helper()
	m, err := d.CreateTeamMembership(context.Background(), params)
	if err != nil {
		t.Fatalf("mustCreateTeamMembership(team=%q, user=%q): %v", params.TeamID, params.UserID, err)
	}
	return m
}

// ---- CreateTeamMembership ---------------------------------------------------

func TestCreateTeamMembership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		params    CreateTeamMembershipParams
		wantErr   error
		checkFunc func(t *testing.T, got *TeamMembership, params CreateTeamMembershipParams)
	}{
		{
			name: "correct fields and ID generated",
			params: CreateTeamMembershipParams{
				// TeamID and UserID filled in by setup below.
				Role: "member",
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *TeamMembership, params CreateTeamMembershipParams) {
				t.Helper()
				if got.ID == "" {
					t.Error("ID is empty, want non-empty UUID")
				}
				if got.TeamID != params.TeamID {
					t.Errorf("TeamID = %q, want %q", got.TeamID, params.TeamID)
				}
				if got.UserID != params.UserID {
					t.Errorf("UserID = %q, want %q", got.UserID, params.UserID)
				}
				if got.Role != params.Role {
					t.Errorf("Role = %q, want %q", got.Role, params.Role)
				}
				if got.CreatedAt == "" {
					t.Error("CreatedAt is empty, want a timestamp")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			org := mustCreateOrg(t, d, CreateOrgParams{Name: "Test Org", Slug: "create-tmem-org-" + tc.name})
			team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Test Team", Slug: "create-tmem-team-" + tc.name})
			user := mustCreateUser(t, d, CreateUserParams{Email: "create-tmem-" + tc.name + "@example.com", DisplayName: "Test User"})

			tc.params.TeamID = team.ID
			tc.params.UserID = user.ID

			got, err := d.CreateTeamMembership(context.Background(), tc.params)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateTeamMembership() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, got, tc.params)
			}
		})
	}
}

func TestCreateTeamMembership_Duplicate(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Dup TM Org", Slug: "dup-tmem-org"})
	team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Dup Team", Slug: "dup-tmem-team"})
	user := mustCreateUser(t, d, CreateUserParams{Email: "dup-tmem@example.com", DisplayName: "Dup User"})

	mustCreateTeamMembership(t, d, CreateTeamMembershipParams{TeamID: team.ID, UserID: user.ID, Role: "member"})

	_, err := d.CreateTeamMembership(ctx, CreateTeamMembershipParams{TeamID: team.ID, UserID: user.ID, Role: "member"})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("CreateTeamMembership() duplicate error = %v, want ErrConflict", err)
	}
}

// ---- GetTeamMembership ------------------------------------------------------

func TestGetTeamMembership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string // returns ID to look up
		wantErr error
	}{
		{
			name: "existing membership returns correct data",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Get TM Org", Slug: "get-tmem-org"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Get Team", Slug: "get-tmem-team"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "get-tmem@example.com", DisplayName: "Get TM User"})
				m := mustCreateTeamMembership(t, d, CreateTeamMembershipParams{TeamID: team.ID, UserID: user.ID, Role: "team_admin"})
				return m.ID
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			id := tc.setup(t, d)
			got, err := d.GetTeamMembership(context.Background(), id)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetTeamMembership() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				if got == nil {
					t.Fatal("GetTeamMembership() returned nil, want non-nil TeamMembership")
				}
				if got.ID != id {
					t.Errorf("GetTeamMembership().ID = %q, want %q", got.ID, id)
				}
				if got.Role != "team_admin" {
					t.Errorf("GetTeamMembership().Role = %q, want %q", got.Role, "team_admin")
				}
			}
		})
	}
}

// ---- ListTeamMemberships ----------------------------------------------------

func TestListTeamMemberships_FiltersByTeamID(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "List TM Org", Slug: "list-tmem-org"})
	teamA := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Team A", Slug: "list-tmem-team-a"})
	teamB := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Team B", Slug: "list-tmem-team-b"})

	userA1 := mustCreateUser(t, d, CreateUserParams{Email: "list-tmem-a1@example.com", DisplayName: "A1"})
	userA2 := mustCreateUser(t, d, CreateUserParams{Email: "list-tmem-a2@example.com", DisplayName: "A2"})
	userB1 := mustCreateUser(t, d, CreateUserParams{Email: "list-tmem-b1@example.com", DisplayName: "B1"})

	mustCreateTeamMembership(t, d, CreateTeamMembershipParams{TeamID: teamA.ID, UserID: userA1.ID, Role: "member"})
	mustCreateTeamMembership(t, d, CreateTeamMembershipParams{TeamID: teamA.ID, UserID: userA2.ID, Role: "member"})
	mustCreateTeamMembership(t, d, CreateTeamMembershipParams{TeamID: teamB.ID, UserID: userB1.ID, Role: "member"})

	memberships, err := d.ListTeamMemberships(ctx, teamA.ID, "", 100)
	if err != nil {
		t.Fatalf("ListTeamMemberships() error = %v, want nil", err)
	}
	if len(memberships) != 2 {
		t.Fatalf("ListTeamMemberships(teamA) len = %d, want 2", len(memberships))
	}
	for _, m := range memberships {
		if m.TeamID != teamA.ID {
			t.Errorf("membership TeamID = %q, want %q", m.TeamID, teamA.ID)
		}
	}
}

func TestListTeamMemberships_Pagination(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Pag TM Org", Slug: "pag-tmem-org"})
	team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Pag Team", Slug: "pag-tmem-team"})

	var users [5]*User
	for i := range 5 {
		users[i] = mustCreateUser(t, d, CreateUserParams{
			Email:       "pag-tmem-user-" + string(rune('a'+i)) + "@example.com",
			DisplayName: "User " + string(rune('a'+i)),
		})
		mustCreateTeamMembership(t, d, CreateTeamMembershipParams{TeamID: team.ID, UserID: users[i].ID, Role: "member"})
	}

	// Page 1: first 2.
	page1, err := d.ListTeamMemberships(ctx, team.ID, "", 2)
	if err != nil {
		t.Fatalf("ListTeamMemberships page1 error = %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}

	// Page 2: next 2 using last ID from page 1 as cursor.
	page2, err := d.ListTeamMemberships(ctx, team.ID, page1[len(page1)-1].ID, 2)
	if err != nil {
		t.Fatalf("ListTeamMemberships page2 error = %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2))
	}

	// Page 3: last 1.
	page3, err := d.ListTeamMemberships(ctx, team.ID, page2[len(page2)-1].ID, 2)
	if err != nil {
		t.Fatalf("ListTeamMemberships page3 error = %v", err)
	}
	if len(page3) != 1 {
		t.Fatalf("page3 len = %d, want 1", len(page3))
	}

	// All IDs must be distinct and in ascending order.
	all := append(append(page1, page2...), page3...)
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Errorf("IDs not in ascending order: all[%d].ID=%q <= all[%d].ID=%q",
				i, all[i].ID, i-1, all[i-1].ID)
		}
	}
}

// ---- UpdateTeamMembership ---------------------------------------------------

func TestUpdateTeamMembership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, d *DB) string // returns membership ID
		params    UpdateTeamMembershipParams
		wantErr   error
		checkFunc func(t *testing.T, got *TeamMembership)
	}{
		{
			name: "role change is persisted",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Upd TM Org", Slug: "upd-tmem-org-role"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Upd Team", Slug: "upd-tmem-team-role"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "upd-tmem-role@example.com", DisplayName: "U"})
				m := mustCreateTeamMembership(t, d, CreateTeamMembershipParams{TeamID: team.ID, UserID: user.ID, Role: "member"})
				return m.ID
			},
			params:  UpdateTeamMembershipParams{Role: ptr("team_admin")},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *TeamMembership) {
				t.Helper()
				if got.Role != "team_admin" {
					t.Errorf("Role = %q, want %q", got.Role, "team_admin")
				}
			},
		},
		{
			name: "nil role returns record unchanged",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Nil TM Org", Slug: "upd-tmem-org-nil"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Nil Team", Slug: "upd-tmem-team-nil"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "upd-tmem-nil@example.com", DisplayName: "U"})
				m := mustCreateTeamMembership(t, d, CreateTeamMembershipParams{TeamID: team.ID, UserID: user.ID, Role: "member"})
				return m.ID
			},
			params:  UpdateTeamMembershipParams{Role: nil},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *TeamMembership) {
				t.Helper()
				if got.Role != "member" {
					t.Errorf("Role = %q, want %q (unchanged)", got.Role, "member")
				}
			},
		},
		{
			name: "non-existent ID returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				return "00000000-0000-0000-0000-000000000000"
			},
			params:  UpdateTeamMembershipParams{Role: ptr("member")},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			id := tc.setup(t, d)
			got, err := d.UpdateTeamMembership(context.Background(), id, tc.params)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("UpdateTeamMembership() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, got)
			}
		})
	}
}

// ---- DeleteTeamMembership ---------------------------------------------------

func TestDeleteTeamMembership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string // returns membership ID to delete
		wantErr error
	}{
		{
			name: "hard delete removes record so GetTeamMembership returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Del TM Org", Slug: "del-tmem-org"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Del Team", Slug: "del-tmem-team"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "del-tmem@example.com", DisplayName: "D"})
				m := mustCreateTeamMembership(t, d, CreateTeamMembershipParams{TeamID: team.ID, UserID: user.ID, Role: "member"})
				return m.ID
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			id := tc.setup(t, d)
			err := d.DeleteTeamMembership(context.Background(), id)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("DeleteTeamMembership() error = %v, wantErr %v", err, tc.wantErr)
			}

			// On success, confirm the record is gone (hard delete).
			if tc.wantErr == nil {
				_, getErr := d.GetTeamMembership(context.Background(), id)
				if !errors.Is(getErr, ErrNotFound) {
					t.Errorf("GetTeamMembership() after DeleteTeamMembership() error = %v, want ErrNotFound", getErr)
				}
			}
		})
	}
}
