package db

import (
	"context"
	"errors"
	"testing"
)

// mustCreateMembership creates an org membership and fatals the test on error.
func mustCreateMembership(t *testing.T, d *DB, params CreateOrgMembershipParams) *OrgMembership {
	t.Helper()
	m, err := d.CreateOrgMembership(context.Background(), params)
	if err != nil {
		t.Fatalf("mustCreateMembership(org=%q, user=%q): %v", params.OrgID, params.UserID, err)
	}
	return m
}

// ---- CreateOrgMembership -----------------------------------------------------

func TestCreateOrgMembership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		params    CreateOrgMembershipParams
		wantErr   error
		checkFunc func(t *testing.T, got *OrgMembership, params CreateOrgMembershipParams)
	}{
		{
			name: "correct fields and ID generated",
			params: CreateOrgMembershipParams{
				// OrgID and UserID filled in by setup below.
				Role: "member",
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *OrgMembership, params CreateOrgMembershipParams) {
				t.Helper()
				if got.ID == "" {
					t.Error("ID is empty, want non-empty UUID")
				}
				if got.OrgID != params.OrgID {
					t.Errorf("OrgID = %q, want %q", got.OrgID, params.OrgID)
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

			org := mustCreateOrg(t, d, CreateOrgParams{Name: "Test Org", Slug: "create-mem-org-" + tc.name})
			user := mustCreateUser(t, d, CreateUserParams{Email: "create-mem-" + tc.name + "@example.com", DisplayName: "Test User"})

			tc.params.OrgID = org.ID
			tc.params.UserID = user.ID

			got, err := d.CreateOrgMembership(context.Background(), tc.params)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateOrgMembership() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, got, tc.params)
			}
		})
	}
}

func TestCreateOrgMembership_Duplicate(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Dup Org", Slug: "dup-mem-org"})
	user := mustCreateUser(t, d, CreateUserParams{Email: "dup-mem@example.com", DisplayName: "Dup User"})

	mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: user.ID, Role: "member"})

	_, err := d.CreateOrgMembership(ctx, CreateOrgMembershipParams{OrgID: org.ID, UserID: user.ID, Role: "member"})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("CreateOrgMembership() duplicate error = %v, want ErrConflict", err)
	}
}

// ---- GetOrgMembership --------------------------------------------------------

func TestGetOrgMembership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string // returns the ID to look up
		wantErr error
	}{
		{
			name: "existing membership returns correct data",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Get Mem Org", Slug: "get-mem-org"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "get-mem@example.com", DisplayName: "Get Mem User"})
				m := mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: user.ID, Role: "org_admin"})
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
			got, err := d.GetOrgMembership(context.Background(), id)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetOrgMembership() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				if got == nil {
					t.Fatal("GetOrgMembership() returned nil, want non-nil OrgMembership")
				}
				if got.ID != id {
					t.Errorf("GetOrgMembership().ID = %q, want %q", got.ID, id)
				}
				if got.Role != "org_admin" {
					t.Errorf("GetOrgMembership().Role = %q, want %q", got.Role, "org_admin")
				}
			}
		})
	}
}

// ---- ListOrgMemberships ------------------------------------------------------

func TestListOrgMemberships_FiltersByOrgID(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	orgA := mustCreateOrg(t, d, CreateOrgParams{Name: "Org A", Slug: "list-mem-org-a"})
	orgB := mustCreateOrg(t, d, CreateOrgParams{Name: "Org B", Slug: "list-mem-org-b"})

	userA1 := mustCreateUser(t, d, CreateUserParams{Email: "list-mem-a1@example.com", DisplayName: "A1"})
	userA2 := mustCreateUser(t, d, CreateUserParams{Email: "list-mem-a2@example.com", DisplayName: "A2"})
	userB1 := mustCreateUser(t, d, CreateUserParams{Email: "list-mem-b1@example.com", DisplayName: "B1"})

	mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: orgA.ID, UserID: userA1.ID, Role: "member"})
	mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: orgA.ID, UserID: userA2.ID, Role: "member"})
	mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: orgB.ID, UserID: userB1.ID, Role: "member"})

	memberships, err := d.ListOrgMemberships(ctx, orgA.ID, "", 100)
	if err != nil {
		t.Fatalf("ListOrgMemberships() error = %v, want nil", err)
	}
	if len(memberships) != 2 {
		t.Fatalf("ListOrgMemberships(orgA) len = %d, want 2", len(memberships))
	}
	for _, m := range memberships {
		if m.OrgID != orgA.ID {
			t.Errorf("membership OrgID = %q, want %q", m.OrgID, orgA.ID)
		}
	}
}

func TestListOrgMemberships_Pagination(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Pag Mem Org", Slug: "pag-mem-org"})

	var users [5]*User
	for i := range 5 {
		users[i] = mustCreateUser(t, d, CreateUserParams{
			Email:       "pag-mem-user-" + string(rune('a'+i)) + "@example.com",
			DisplayName: "User " + string(rune('a'+i)),
		})
		mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: users[i].ID, Role: "member"})
	}

	// Page 1: first 2.
	page1, err := d.ListOrgMemberships(ctx, org.ID, "", 2)
	if err != nil {
		t.Fatalf("ListOrgMemberships page1 error = %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}

	// Page 2: next 2 using last ID from page 1 as cursor.
	page2, err := d.ListOrgMemberships(ctx, org.ID, page1[len(page1)-1].ID, 2)
	if err != nil {
		t.Fatalf("ListOrgMemberships page2 error = %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2))
	}

	// Page 3: last 1.
	page3, err := d.ListOrgMemberships(ctx, org.ID, page2[len(page2)-1].ID, 2)
	if err != nil {
		t.Fatalf("ListOrgMemberships page3 error = %v", err)
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

// ---- UpdateOrgMembership -----------------------------------------------------

func TestUpdateOrgMembership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, d *DB) string // returns membership ID
		params    UpdateOrgMembershipParams
		wantErr   error
		checkFunc func(t *testing.T, got *OrgMembership)
	}{
		{
			name: "role change is persisted",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Upd Mem Org", Slug: "upd-mem-org-role"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "upd-mem-role@example.com", DisplayName: "U"})
				m := mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: user.ID, Role: "member"})
				return m.ID
			},
			params:  UpdateOrgMembershipParams{Role: ptr("org_admin")},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *OrgMembership) {
				t.Helper()
				if got.Role != "org_admin" {
					t.Errorf("Role = %q, want %q", got.Role, "org_admin")
				}
			},
		},
		{
			name: "nil role returns record unchanged",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Nil Role Org", Slug: "upd-mem-org-nil"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "upd-mem-nil@example.com", DisplayName: "U"})
				m := mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: user.ID, Role: "member"})
				return m.ID
			},
			params:  UpdateOrgMembershipParams{Role: nil},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *OrgMembership) {
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
			params:  UpdateOrgMembershipParams{Role: ptr("member")},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			id := tc.setup(t, d)
			got, err := d.UpdateOrgMembership(context.Background(), id, tc.params)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("UpdateOrgMembership() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, got)
			}
		})
	}
}

// ---- DeleteOrgMembership -----------------------------------------------------

func TestDeleteOrgMembership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string // returns membership ID to delete
		wantErr error
	}{
		{
			name: "hard delete removes record so GetOrgMembership returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Del Mem Org", Slug: "del-mem-org"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "del-mem@example.com", DisplayName: "D"})
				m := mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: user.ID, Role: "member"})
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
			err := d.DeleteOrgMembership(context.Background(), id)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("DeleteOrgMembership() error = %v, wantErr %v", err, tc.wantErr)
			}

			// On success, confirm the record is gone (hard delete).
			if tc.wantErr == nil {
				_, getErr := d.GetOrgMembership(context.Background(), id)
				if !errors.Is(getErr, ErrNotFound) {
					t.Errorf("GetOrgMembership() after DeleteOrgMembership() error = %v, want ErrNotFound", getErr)
				}
			}
		})
	}
}
