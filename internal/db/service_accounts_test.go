package db

import (
	"context"
	"errors"
	"testing"
)

// mustCreateServiceAccount creates a service account and fatals the test on error.
func mustCreateServiceAccount(t *testing.T, d *DB, params CreateServiceAccountParams) *ServiceAccount {
	t.Helper()
	sa, err := d.CreateServiceAccount(context.Background(), params)
	if err != nil {
		t.Fatalf("mustCreateServiceAccount(%q): %v", params.Name, err)
	}
	return sa
}

// ---- CreateServiceAccount ---------------------------------------------------

func TestCreateServiceAccount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, d *DB) CreateServiceAccountParams
		wantErr   error
		checkFunc func(t *testing.T, got *ServiceAccount, params CreateServiceAccountParams)
	}{
		{
			name: "org-scoped SA has nil team_id",
			setup: func(t *testing.T, d *DB) CreateServiceAccountParams {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org SA", Slug: "org-sa-create"})
				user := mustCreateUser(t, d, CreateUserParams{
					Email:        "creator@example.com",
					DisplayName:  "Creator",
					PasswordHash: testPasswordHash(t),
					AuthProvider: "local",
				})
				return CreateServiceAccountParams{
					Name:      "CI Bot",
					OrgID:     org.ID,
					TeamID:    nil,
					CreatedBy: user.ID,
				}
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *ServiceAccount, params CreateServiceAccountParams) {
				t.Helper()
				if got.ID == "" {
					t.Error("ID is empty, want non-empty UUID")
				}
				if got.Name != params.Name {
					t.Errorf("Name = %q, want %q", got.Name, params.Name)
				}
				if got.OrgID != params.OrgID {
					t.Errorf("OrgID = %q, want %q", got.OrgID, params.OrgID)
				}
				if got.TeamID != nil {
					t.Errorf("TeamID = %v, want nil", got.TeamID)
				}
				if got.CreatedBy != params.CreatedBy {
					t.Errorf("CreatedBy = %q, want %q", got.CreatedBy, params.CreatedBy)
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
			name: "team-scoped SA has team_id populated",
			setup: func(t *testing.T, d *DB) CreateServiceAccountParams {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Team SA Org", Slug: "team-sa-org"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Eng", Slug: "eng-sa"})
				user := mustCreateUser(t, d, CreateUserParams{
					Email:        "teamcreator@example.com",
					DisplayName:  "Team Creator",
					PasswordHash: testPasswordHash(t),
					AuthProvider: "local",
				})
				return CreateServiceAccountParams{
					Name:      "Deploy Bot",
					OrgID:     org.ID,
					TeamID:    &team.ID,
					CreatedBy: user.ID,
				}
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *ServiceAccount, params CreateServiceAccountParams) {
				t.Helper()
				if got.TeamID == nil {
					t.Fatal("TeamID = nil, want non-nil")
				}
				if *got.TeamID != *params.TeamID {
					t.Errorf("TeamID = %q, want %q", *got.TeamID, *params.TeamID)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			params := tc.setup(t, d)
			got, err := d.CreateServiceAccount(context.Background(), params)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateServiceAccount() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, got, params)
			}
		})
	}
}

// ---- GetServiceAccount ------------------------------------------------------

func TestGetServiceAccount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string // returns ID to look up
		wantErr error
	}{
		{
			name: "existing SA returns correct data",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Get SA Org", Slug: "get-sa-org"})
				user := mustCreateUser(t, d, CreateUserParams{
					Email:        "getter@example.com",
					DisplayName:  "Getter",
					PasswordHash: testPasswordHash(t),
					AuthProvider: "local",
				})
				sa := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name:      "Read Bot",
					OrgID:     org.ID,
					CreatedBy: user.ID,
				})
				return sa.ID
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
			name: "soft-deleted SA returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Deleted SA Org", Slug: "deleted-sa-org"})
				user := mustCreateUser(t, d, CreateUserParams{
					Email:        "deleter@example.com",
					DisplayName:  "Deleter",
					PasswordHash: testPasswordHash(t),
					AuthProvider: "local",
				})
				sa := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name:      "Gone Bot",
					OrgID:     org.ID,
					CreatedBy: user.ID,
				})
				if err := d.DeleteServiceAccount(context.Background(), sa.ID); err != nil {
					t.Fatalf("DeleteServiceAccount(): %v", err)
				}
				return sa.ID
			},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			id := tc.setup(t, d)
			got, err := d.GetServiceAccount(context.Background(), id)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetServiceAccount() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				if got == nil {
					t.Fatal("GetServiceAccount() returned nil, want non-nil")
				}
				if got.ID != id {
					t.Errorf("GetServiceAccount().ID = %q, want %q", got.ID, id)
				}
			}
		})
	}
}

// ---- ListServiceAccounts ----------------------------------------------------

func TestListServiceAccounts_ByOrgID(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org1 := mustCreateOrg(t, d, CreateOrgParams{Name: "List SA Org1", Slug: "list-sa-org1"})
	org2 := mustCreateOrg(t, d, CreateOrgParams{Name: "List SA Org2", Slug: "list-sa-org2"})
	user := mustCreateUser(t, d, CreateUserParams{
		Email:        "listcreator@example.com",
		DisplayName:  "List Creator",
		PasswordHash: testPasswordHash(t),
		AuthProvider: "local",
	})

	mustCreateServiceAccount(t, d, CreateServiceAccountParams{Name: "Bot A", OrgID: org1.ID, CreatedBy: user.ID})
	mustCreateServiceAccount(t, d, CreateServiceAccountParams{Name: "Bot B", OrgID: org1.ID, CreatedBy: user.ID})
	mustCreateServiceAccount(t, d, CreateServiceAccountParams{Name: "Bot C", OrgID: org2.ID, CreatedBy: user.ID})

	accounts, err := d.ListServiceAccounts(ctx, org1.ID, "", "", 100, false)
	if err != nil {
		t.Fatalf("ListServiceAccounts() error = %v", err)
	}
	if len(accounts) != 2 {
		t.Errorf("len(accounts) = %d, want 2 (only org1's SAs)", len(accounts))
	}
	for _, a := range accounts {
		if a.OrgID != org1.ID {
			t.Errorf("account OrgID = %q, want %q", a.OrgID, org1.ID)
		}
	}
}

func TestListServiceAccounts_Pagination(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Paged SA Org", Slug: "paged-sa-org"})
	user := mustCreateUser(t, d, CreateUserParams{
		Email:        "pagecreator@example.com",
		DisplayName:  "Page Creator",
		PasswordHash: testPasswordHash(t),
		AuthProvider: "local",
	})

	for i := range 5 {
		mustCreateServiceAccount(t, d, CreateServiceAccountParams{
			Name:      "Bot " + string(rune('A'+i)),
			OrgID:     org.ID,
			CreatedBy: user.ID,
		})
	}

	page1, err := d.ListServiceAccounts(ctx, org.ID, "", "", 2, false)
	if err != nil {
		t.Fatalf("ListServiceAccounts page1 error = %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}

	page2, err := d.ListServiceAccounts(ctx, org.ID, "", page1[len(page1)-1].ID, 2, false)
	if err != nil {
		t.Fatalf("ListServiceAccounts page2 error = %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2))
	}

	page3, err := d.ListServiceAccounts(ctx, org.ID, "", page2[len(page2)-1].ID, 2, false)
	if err != nil {
		t.Fatalf("ListServiceAccounts page3 error = %v", err)
	}
	if len(page3) != 1 {
		t.Fatalf("page3 len = %d, want 1", len(page3))
	}

	all := append(append(page1, page2...), page3...)
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Errorf("IDs not in ascending order: all[%d].ID=%q <= all[%d].ID=%q",
				i, all[i].ID, i-1, all[i-1].ID)
		}
	}
}

func TestListServiceAccounts_ExcludesSoftDeleted(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Excl Del SA Org", Slug: "excl-del-sa-org"})
	user := mustCreateUser(t, d, CreateUserParams{
		Email:        "excl@example.com",
		DisplayName:  "Excl",
		PasswordHash: testPasswordHash(t),
		AuthProvider: "local",
	})

	live := mustCreateServiceAccount(t, d, CreateServiceAccountParams{Name: "Live Bot", OrgID: org.ID, CreatedBy: user.ID})
	gone := mustCreateServiceAccount(t, d, CreateServiceAccountParams{Name: "Gone Bot", OrgID: org.ID, CreatedBy: user.ID})

	if err := d.DeleteServiceAccount(ctx, gone.ID); err != nil {
		t.Fatalf("DeleteServiceAccount(): %v", err)
	}

	accounts, err := d.ListServiceAccounts(ctx, org.ID, "", "", 100, false)
	if err != nil {
		t.Fatalf("ListServiceAccounts() error = %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("ListServiceAccounts(includeDeleted=false) len = %d, want 1", len(accounts))
	}
	if accounts[0].ID != live.ID {
		t.Errorf("accounts[0].ID = %q, want %q", accounts[0].ID, live.ID)
	}
}

func TestListServiceAccounts_IncludesSoftDeleted(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Incl Del SA Org", Slug: "incl-del-sa-org"})
	user := mustCreateUser(t, d, CreateUserParams{
		Email:        "incl@example.com",
		DisplayName:  "Incl",
		PasswordHash: testPasswordHash(t),
		AuthProvider: "local",
	})

	mustCreateServiceAccount(t, d, CreateServiceAccountParams{Name: "Live Bot", OrgID: org.ID, CreatedBy: user.ID})
	gone := mustCreateServiceAccount(t, d, CreateServiceAccountParams{Name: "Gone Bot", OrgID: org.ID, CreatedBy: user.ID})

	if err := d.DeleteServiceAccount(ctx, gone.ID); err != nil {
		t.Fatalf("DeleteServiceAccount(): %v", err)
	}

	accounts, err := d.ListServiceAccounts(ctx, org.ID, "", "", 100, true)
	if err != nil {
		t.Fatalf("ListServiceAccounts() error = %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("ListServiceAccounts(includeDeleted=true) len = %d, want 2", len(accounts))
	}

	var foundDeleted bool
	for _, a := range accounts {
		if a.ID == gone.ID {
			foundDeleted = true
			if a.DeletedAt == nil {
				t.Error("deleted SA has nil DeletedAt, want a timestamp")
			}
		}
	}
	if !foundDeleted {
		t.Error("deleted SA not found in ListServiceAccounts(includeDeleted=true) results")
	}
}

// ---- UpdateServiceAccount ---------------------------------------------------

func TestUpdateServiceAccount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, d *DB) *ServiceAccount
		params    UpdateServiceAccountParams
		wantErr   error
		checkFunc func(t *testing.T, original, got *ServiceAccount)
	}{
		{
			name: "update name changes the name",
			setup: func(t *testing.T, d *DB) *ServiceAccount {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Upd SA Org", Slug: "upd-sa-org"})
				user := mustCreateUser(t, d, CreateUserParams{
					Email:        "updater@example.com",
					DisplayName:  "Updater",
					PasswordHash: testPasswordHash(t),
					AuthProvider: "local",
				})
				return mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name:      "Old Name Bot",
					OrgID:     org.ID,
					CreatedBy: user.ID,
				})
			},
			params:  UpdateServiceAccountParams{Name: ptr("New Name Bot")},
			wantErr: nil,
			checkFunc: func(t *testing.T, original, got *ServiceAccount) {
				t.Helper()
				if got.Name != "New Name Bot" {
					t.Errorf("Name = %q, want %q", got.Name, "New Name Bot")
				}
				if got.OrgID != original.OrgID {
					t.Errorf("OrgID changed: got %q, want %q", got.OrgID, original.OrgID)
				}
			},
		},
		{
			name: "no fields set returns SA unchanged",
			setup: func(t *testing.T, d *DB) *ServiceAccount {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Stable SA Org", Slug: "stable-sa-org"})
				user := mustCreateUser(t, d, CreateUserParams{
					Email:        "stable@example.com",
					DisplayName:  "Stable",
					PasswordHash: testPasswordHash(t),
					AuthProvider: "local",
				})
				return mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name:      "Stable Bot",
					OrgID:     org.ID,
					CreatedBy: user.ID,
				})
			},
			params:  UpdateServiceAccountParams{},
			wantErr: nil,
			checkFunc: func(t *testing.T, original, got *ServiceAccount) {
				t.Helper()
				if got.Name != original.Name {
					t.Errorf("Name = %q, want %q (unchanged)", got.Name, original.Name)
				}
			},
		},
		{
			name: "non-existent ID returns ErrNotFound",
			setup: func(t *testing.T, d *DB) *ServiceAccount {
				return &ServiceAccount{ID: "00000000-0000-0000-0000-000000000000"}
			},
			params:  UpdateServiceAccountParams{Name: ptr("Ghost Bot")},
			wantErr: ErrNotFound,
		},
		{
			name: "soft-deleted SA returns ErrNotFound",
			setup: func(t *testing.T, d *DB) *ServiceAccount {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Del Upd SA Org", Slug: "del-upd-sa-org"})
				user := mustCreateUser(t, d, CreateUserParams{
					Email:        "delupdater@example.com",
					DisplayName:  "DelUpdater",
					PasswordHash: testPasswordHash(t),
					AuthProvider: "local",
				})
				sa := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name:      "To Delete Bot",
					OrgID:     org.ID,
					CreatedBy: user.ID,
				})
				if err := d.DeleteServiceAccount(context.Background(), sa.ID); err != nil {
					t.Fatalf("DeleteServiceAccount(): %v", err)
				}
				return sa
			},
			params:  UpdateServiceAccountParams{Name: ptr("Still Gone Bot")},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			original := tc.setup(t, d)
			got, err := d.UpdateServiceAccount(context.Background(), original.ID, tc.params)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("UpdateServiceAccount() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, original, got)
			}
		})
	}
}

// ---- DeleteServiceAccount ---------------------------------------------------

func TestDeleteServiceAccount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string
		wantErr error
	}{
		{
			name: "delete existing SA makes GetServiceAccount return ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Del SA Org", Slug: "del-sa-org"})
				user := mustCreateUser(t, d, CreateUserParams{
					Email:        "todeluser@example.com",
					DisplayName:  "ToDel",
					PasswordHash: testPasswordHash(t),
					AuthProvider: "local",
				})
				sa := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name:      "To Delete Bot",
					OrgID:     org.ID,
					CreatedBy: user.ID,
				})
				return sa.ID
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
		{
			name: "delete already-deleted SA returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Double Del SA Org", Slug: "double-del-sa-org"})
				user := mustCreateUser(t, d, CreateUserParams{
					Email:        "doubledeluser@example.com",
					DisplayName:  "DoubleDel",
					PasswordHash: testPasswordHash(t),
					AuthProvider: "local",
				})
				sa := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name:      "Double Delete Bot",
					OrgID:     org.ID,
					CreatedBy: user.ID,
				})
				if err := d.DeleteServiceAccount(context.Background(), sa.ID); err != nil {
					t.Fatalf("first DeleteServiceAccount(): %v", err)
				}
				return sa.ID
			},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			id := tc.setup(t, d)
			err := d.DeleteServiceAccount(context.Background(), id)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("DeleteServiceAccount() error = %v, wantErr %v", err, tc.wantErr)
			}

			if tc.wantErr == nil {
				_, getErr := d.GetServiceAccount(context.Background(), id)
				if !errors.Is(getErr, ErrNotFound) {
					t.Errorf("GetServiceAccount() after DeleteServiceAccount() error = %v, want ErrNotFound", getErr)
				}
			}
		})
	}
}
