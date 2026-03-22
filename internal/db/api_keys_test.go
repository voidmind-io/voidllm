package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// testHMACSecret is a fixed secret used for computing key hashes in DB-layer tests.
var testHMACSecret = []byte("test-hmac-secret-for-db-layer-tests")

// mustCreateAPIKey inserts an API key and fails the test on error.
func mustCreateAPIKey(t *testing.T, d *DB, params CreateAPIKeyParams) *APIKey {
	t.Helper()
	key, err := d.CreateAPIKey(context.Background(), params)
	if err != nil {
		t.Fatalf("mustCreateAPIKey(%q): %v", params.Name, err)
	}
	return key
}

// testKeyParams builds a minimal CreateAPIKeyParams for a user_key.
// org, user, team, and createdBy must be pre-existing IDs.
func testKeyParams(orgID, userID, teamID, createdBy string) CreateAPIKeyParams {
	plaintext := "vl_uk_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	return CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, testHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeUser,
		Name:      "test key",
		OrgID:     orgID,
		UserID:    ptr(userID),
		TeamID:    ptr(teamID),
		CreatedBy: createdBy,
	}
}

// ---- CreateAPIKey -----------------------------------------------------------

func TestCreateAPIKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, d *DB) CreateAPIKeyParams
		wantErr   error
		checkFunc func(t *testing.T, got *APIKey, params CreateAPIKeyParams)
	}{
		{
			name: "user_key has correct fields and generated ID",
			setup: func(t *testing.T, d *DB) CreateAPIKeyParams {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-uk-1"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "u@example.com", DisplayName: "U"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-uk-1"})
				return testKeyParams(org.ID, user.ID, team.ID, user.ID)
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *APIKey, params CreateAPIKeyParams) {
				t.Helper()
				if got.ID == "" {
					t.Error("ID is empty, want non-empty UUID")
				}
				if got.KeyHash != params.KeyHash {
					t.Errorf("KeyHash = %q, want %q", got.KeyHash, params.KeyHash)
				}
				if got.KeyHint != params.KeyHint {
					t.Errorf("KeyHint = %q, want %q", got.KeyHint, params.KeyHint)
				}
				if got.KeyType != keygen.KeyTypeUser {
					t.Errorf("KeyType = %q, want %q", got.KeyType, keygen.KeyTypeUser)
				}
				if got.Name != params.Name {
					t.Errorf("Name = %q, want %q", got.Name, params.Name)
				}
				if got.OrgID != params.OrgID {
					t.Errorf("OrgID = %q, want %q", got.OrgID, params.OrgID)
				}
				if got.UserID == nil || *got.UserID != *params.UserID {
					t.Errorf("UserID = %v, want %q", got.UserID, *params.UserID)
				}
				if got.TeamID == nil || *got.TeamID != *params.TeamID {
					t.Errorf("TeamID = %v, want %q", got.TeamID, *params.TeamID)
				}
				if got.ServiceAccountID != nil {
					t.Errorf("ServiceAccountID = %v, want nil", got.ServiceAccountID)
				}
				if got.CreatedBy != params.CreatedBy {
					t.Errorf("CreatedBy = %q, want %q", got.CreatedBy, params.CreatedBy)
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
			name: "team_key has team_id set and user_id nil",
			setup: func(t *testing.T, d *DB) CreateAPIKeyParams {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-tk-1"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "u2@example.com", DisplayName: "U2"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-tk-1"})
				plaintext := "vl_tk_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
				return CreateAPIKeyParams{
					KeyHash:   keygen.Hash(plaintext, testHMACSecret),
					KeyHint:   keygen.Hint(plaintext),
					KeyType:   keygen.KeyTypeTeam,
					Name:      "team key",
					OrgID:     org.ID,
					TeamID:    ptr(team.ID),
					CreatedBy: user.ID,
				}
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *APIKey, _ CreateAPIKeyParams) {
				t.Helper()
				if got.UserID != nil {
					t.Errorf("UserID = %v, want nil", got.UserID)
				}
				if got.TeamID == nil {
					t.Error("TeamID is nil, want non-nil")
				}
				if got.ServiceAccountID != nil {
					t.Errorf("ServiceAccountID = %v, want nil", got.ServiceAccountID)
				}
				if got.KeyType != keygen.KeyTypeTeam {
					t.Errorf("KeyType = %q, want %q", got.KeyType, keygen.KeyTypeTeam)
				}
			},
		},
		{
			name: "sa_key has service_account_id set and user_id/team_id nil",
			setup: func(t *testing.T, d *DB) CreateAPIKeyParams {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-sa-1"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "u3@example.com", DisplayName: "U3"})
				sa, err := d.CreateServiceAccount(context.Background(), CreateServiceAccountParams{
					Name:      "Bot",
					OrgID:     org.ID,
					CreatedBy: user.ID,
				})
				if err != nil {
					t.Fatalf("CreateServiceAccount: %v", err)
				}
				plaintext := "vl_sa_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
				return CreateAPIKeyParams{
					KeyHash:          keygen.Hash(plaintext, testHMACSecret),
					KeyHint:          keygen.Hint(plaintext),
					KeyType:          keygen.KeyTypeSA,
					Name:             "sa key",
					OrgID:            org.ID,
					ServiceAccountID: ptr(sa.ID),
					CreatedBy:        user.ID,
				}
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *APIKey, _ CreateAPIKeyParams) {
				t.Helper()
				if got.UserID != nil {
					t.Errorf("UserID = %v, want nil", got.UserID)
				}
				if got.TeamID != nil {
					t.Errorf("TeamID = %v, want nil", got.TeamID)
				}
				if got.ServiceAccountID == nil {
					t.Error("ServiceAccountID is nil, want non-nil")
				}
				if got.KeyType != keygen.KeyTypeSA {
					t.Errorf("KeyType = %q, want %q", got.KeyType, keygen.KeyTypeSA)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)
			params := tc.setup(t, d)

			got, err := d.CreateAPIKey(context.Background(), params)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateAPIKey() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, got, params)
			}
		})
	}
}

// ---- GetAPIKey --------------------------------------------------------------

func TestGetAPIKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string // returns ID to look up
		wantErr error
		check   func(t *testing.T, got *APIKey)
	}{
		{
			name: "existing key returns all 19 fields populated",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "o-getkey-1"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "g1@example.com", DisplayName: "G1"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-getkey-1"})
				k := mustCreateAPIKey(t, d, testKeyParams(org.ID, user.ID, team.ID, user.ID))
				return k.ID
			},
			wantErr: nil,
			check: func(t *testing.T, got *APIKey) {
				t.Helper()
				if got.ID == "" {
					t.Error("ID empty")
				}
				if got.KeyHash == "" {
					t.Error("KeyHash empty")
				}
				if got.KeyHint == "" {
					t.Error("KeyHint empty")
				}
				if got.KeyType == "" {
					t.Error("KeyType empty")
				}
				if got.Name == "" {
					t.Error("Name empty")
				}
				if got.OrgID == "" {
					t.Error("OrgID empty")
				}
				if got.CreatedBy == "" {
					t.Error("CreatedBy empty")
				}
				if got.CreatedAt == "" {
					t.Error("CreatedAt empty")
				}
				if got.UpdatedAt == "" {
					t.Error("UpdatedAt empty")
				}
				if got.DeletedAt != nil {
					t.Errorf("DeletedAt = %v, want nil", got.DeletedAt)
				}
			},
		},
		{
			name: "non-existent ID returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				return "00000000-0000-0000-0000-000000000000"
			},
			wantErr: ErrNotFound,
		},
		{
			name: "soft-deleted key returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "o-getkey-del"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "del@example.com", DisplayName: "Del"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-getkey-del"})
				k := mustCreateAPIKey(t, d, testKeyParams(org.ID, user.ID, team.ID, user.ID))
				if err := d.DeleteAPIKey(context.Background(), k.ID); err != nil {
					t.Fatalf("DeleteAPIKey: %v", err)
				}
				return k.ID
			},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)
			id := tc.setup(t, d)

			got, err := d.GetAPIKey(context.Background(), id)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetAPIKey() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				if got == nil {
					t.Fatal("GetAPIKey() returned nil, want non-nil")
				}
				if got.ID != id {
					t.Errorf("GetAPIKey().ID = %q, want %q", got.ID, id)
				}
				if tc.check != nil {
					tc.check(t, got)
				}
			}
		})
	}
}

// ---- ListAPIKeys ------------------------------------------------------------

func TestListAPIKeys_ByOrgID(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org1 := mustCreateOrg(t, d, CreateOrgParams{Name: "Org1", Slug: "list-org1"})
	org2 := mustCreateOrg(t, d, CreateOrgParams{Name: "Org2", Slug: "list-org2"})
	user := mustCreateUser(t, d, CreateUserParams{Email: "list@example.com", DisplayName: "L"})
	team1 := mustCreateTeam(t, d, CreateTeamParams{OrgID: org1.ID, Name: "T1", Slug: "t-list-1"})
	team2 := mustCreateTeam(t, d, CreateTeamParams{OrgID: org2.ID, Name: "T2", Slug: "t-list-2"})

	// Two keys in org1, one in org2.
	for i, suffix := range []string{"a", "b"} {
		_ = i
		plain := "vl_uk_listtest" + suffix + strings.Repeat("0", 40-len(suffix))
		mustCreateAPIKey(t, d, CreateAPIKeyParams{
			KeyHash:   keygen.Hash(plain, testHMACSecret),
			KeyHint:   keygen.Hint(plain),
			KeyType:   keygen.KeyTypeUser,
			Name:      "key-" + suffix,
			OrgID:     org1.ID,
			UserID:    ptr(user.ID),
			TeamID:    ptr(team1.ID),
			CreatedBy: user.ID,
		})
	}
	plain3 := "vl_uk_listtestc" + strings.Repeat("0", 38)
	mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plain3, testHMACSecret),
		KeyHint:   keygen.Hint(plain3),
		KeyType:   keygen.KeyTypeUser,
		Name:      "key-c",
		OrgID:     org2.ID,
		UserID:    ptr(user.ID),
		TeamID:    ptr(team2.ID),
		CreatedBy: user.ID,
	})

	keys, err := d.ListAPIKeys(ctx, org1.ID, "", "", "", 100, false)
	if err != nil {
		t.Fatalf("ListAPIKeys() error = %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("ListAPIKeys() len = %d, want 2", len(keys))
	}
	for _, k := range keys {
		if k.OrgID != org1.ID {
			t.Errorf("key OrgID = %q, want %q", k.OrgID, org1.ID)
		}
	}
}

func TestListAPIKeys_Pagination(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "pag-keys-org"})
	user := mustCreateUser(t, d, CreateUserParams{Email: "pag@example.com", DisplayName: "P"})
	team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-pag-1"})

	for i := range 5 {
		plain := "vl_uk_pagtest" + string(rune('a'+i)) + strings.Repeat("0", 40)
		mustCreateAPIKey(t, d, CreateAPIKeyParams{
			KeyHash:   keygen.Hash(plain, testHMACSecret),
			KeyHint:   keygen.Hint(plain),
			KeyType:   keygen.KeyTypeUser,
			Name:      "pag-key-" + string(rune('a'+i)),
			OrgID:     org.ID,
			UserID:    ptr(user.ID),
			TeamID:    ptr(team.ID),
			CreatedBy: user.ID,
		})
	}

	page1, err := d.ListAPIKeys(ctx, org.ID, "", "", "", 2, false)
	if err != nil {
		t.Fatalf("page1 error = %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}

	page2, err := d.ListAPIKeys(ctx, org.ID, "", "", page1[len(page1)-1].ID, 2, false)
	if err != nil {
		t.Fatalf("page2 error = %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2))
	}

	page3, err := d.ListAPIKeys(ctx, org.ID, "", "", page2[len(page2)-1].ID, 2, false)
	if err != nil {
		t.Fatalf("page3 error = %v", err)
	}
	if len(page3) != 1 {
		t.Fatalf("page3 len = %d, want 1", len(page3))
	}

	all := append(append(page1, page2...), page3...)
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Errorf("IDs not ascending: all[%d].ID=%q <= all[%d].ID=%q", i, all[i].ID, i-1, all[i-1].ID)
		}
	}
}

func TestListAPIKeys_IncludeDeleted(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "inc-del-org"})
	user := mustCreateUser(t, d, CreateUserParams{Email: "incl@example.com", DisplayName: "I"})
	team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-inc-del"})

	plain1 := "vl_uk_live" + strings.Repeat("0", 44)
	plain2 := "vl_uk_gone" + strings.Repeat("0", 44)
	live := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash(plain1, testHMACSecret), KeyHint: keygen.Hint(plain1),
		KeyType: keygen.KeyTypeUser, Name: "live", OrgID: org.ID,
		UserID: ptr(user.ID), TeamID: ptr(team.ID), CreatedBy: user.ID,
	})
	gone := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash(plain2, testHMACSecret), KeyHint: keygen.Hint(plain2),
		KeyType: keygen.KeyTypeUser, Name: "gone", OrgID: org.ID,
		UserID: ptr(user.ID), TeamID: ptr(team.ID), CreatedBy: user.ID,
	})
	if err := d.DeleteAPIKey(ctx, gone.ID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}

	// Without includeDeleted: only live.
	active, err := d.ListAPIKeys(ctx, org.ID, "", "", "", 100, false)
	if err != nil {
		t.Fatalf("ListAPIKeys(false) error = %v", err)
	}
	if len(active) != 1 || active[0].ID != live.ID {
		t.Errorf("ListAPIKeys(false) = %d keys, want 1 with ID %q", len(active), live.ID)
	}

	// With includeDeleted: both.
	all, err := d.ListAPIKeys(ctx, org.ID, "", "", "", 100, true)
	if err != nil {
		t.Fatalf("ListAPIKeys(true) error = %v", err)
	}
	if len(all) != 2 {
		t.Errorf("ListAPIKeys(true) = %d keys, want 2", len(all))
	}
}

// ---- UpdateAPIKey -----------------------------------------------------------

func TestUpdateAPIKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, d *DB) *APIKey
		params    UpdateAPIKeyParams
		wantErr   error
		checkFunc func(t *testing.T, original, got *APIKey)
	}{
		{
			name: "update name only leaves other fields unchanged",
			setup: func(t *testing.T, d *DB) *APIKey {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "upd-name-org"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "upd1@example.com", DisplayName: "U"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-upd-1"})
				return mustCreateAPIKey(t, d, testKeyParams(org.ID, user.ID, team.ID, user.ID))
			},
			params:  UpdateAPIKeyParams{Name: ptr("renamed key")},
			wantErr: nil,
			checkFunc: func(t *testing.T, original, got *APIKey) {
				t.Helper()
				if got.Name != "renamed key" {
					t.Errorf("Name = %q, want %q", got.Name, "renamed key")
				}
				if got.KeyType != original.KeyType {
					t.Errorf("KeyType changed: %q != %q", got.KeyType, original.KeyType)
				}
				if got.OrgID != original.OrgID {
					t.Errorf("OrgID changed: %q != %q", got.OrgID, original.OrgID)
				}
			},
		},
		{
			name: "update limits changes daily and monthly token limits",
			setup: func(t *testing.T, d *DB) *APIKey {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "upd-limits-org"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "upd2@example.com", DisplayName: "U"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-upd-2"})
				return mustCreateAPIKey(t, d, testKeyParams(org.ID, user.ID, team.ID, user.ID))
			},
			params: UpdateAPIKeyParams{
				DailyTokenLimit:   ptr(int64(5000)),
				MonthlyTokenLimit: ptr(int64(100000)),
				RequestsPerMinute: ptr(30),
				RequestsPerDay:    ptr(500),
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, _, got *APIKey) {
				t.Helper()
				if got.DailyTokenLimit != 5000 {
					t.Errorf("DailyTokenLimit = %d, want 5000", got.DailyTokenLimit)
				}
				if got.MonthlyTokenLimit != 100000 {
					t.Errorf("MonthlyTokenLimit = %d, want 100000", got.MonthlyTokenLimit)
				}
				if got.RequestsPerMinute != 30 {
					t.Errorf("RequestsPerMinute = %d, want 30", got.RequestsPerMinute)
				}
				if got.RequestsPerDay != 500 {
					t.Errorf("RequestsPerDay = %d, want 500", got.RequestsPerDay)
				}
			},
		},
		{
			name: "update expires_at sets expiry",
			setup: func(t *testing.T, d *DB) *APIKey {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "upd-exp-org"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "upd3@example.com", DisplayName: "U"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-upd-3"})
				return mustCreateAPIKey(t, d, testKeyParams(org.ID, user.ID, team.ID, user.ID))
			},
			params:  UpdateAPIKeyParams{ExpiresAt: ptr("2030-01-01T00:00:00Z")},
			wantErr: nil,
			checkFunc: func(t *testing.T, _, got *APIKey) {
				t.Helper()
				if got.ExpiresAt == nil {
					t.Fatal("ExpiresAt is nil, want non-nil")
				}
				if *got.ExpiresAt != "2030-01-01T00:00:00Z" {
					t.Errorf("ExpiresAt = %q, want %q", *got.ExpiresAt, "2030-01-01T00:00:00Z")
				}
			},
		},
		{
			name: "all-nil params returns current record unchanged",
			setup: func(t *testing.T, d *DB) *APIKey {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "upd-noop-org"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "upd4@example.com", DisplayName: "U"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-upd-4"})
				return mustCreateAPIKey(t, d, testKeyParams(org.ID, user.ID, team.ID, user.ID))
			},
			params:  UpdateAPIKeyParams{},
			wantErr: nil,
			checkFunc: func(t *testing.T, original, got *APIKey) {
				t.Helper()
				if got.Name != original.Name {
					t.Errorf("Name changed: %q != %q", got.Name, original.Name)
				}
				if got.KeyHash != original.KeyHash {
					t.Error("KeyHash changed, expected no-op update")
				}
			},
		},
		{
			name: "non-existent ID returns ErrNotFound",
			setup: func(t *testing.T, d *DB) *APIKey {
				return &APIKey{ID: "00000000-0000-0000-0000-000000000000"}
			},
			params:  UpdateAPIKeyParams{Name: ptr("ghost")},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)
			original := tc.setup(t, d)

			got, err := d.UpdateAPIKey(context.Background(), original.ID, tc.params)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("UpdateAPIKey() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, original, got)
			}
		})
	}
}

// ---- DeleteAPIKey -----------------------------------------------------------

func TestDeleteAPIKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string
		wantErr error
	}{
		{
			name: "soft-delete makes GetAPIKey return ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "del-key-org"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "dk@example.com", DisplayName: "DK"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-del-1"})
				k := mustCreateAPIKey(t, d, testKeyParams(org.ID, user.ID, team.ID, user.ID))
				return k.ID
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
			name: "already-deleted key returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "del-key-twice-org"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "dk2@example.com", DisplayName: "DK2"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-del-2"})
				k := mustCreateAPIKey(t, d, testKeyParams(org.ID, user.ID, team.ID, user.ID))
				if err := d.DeleteAPIKey(context.Background(), k.ID); err != nil {
					t.Fatalf("first DeleteAPIKey: %v", err)
				}
				return k.ID
			},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)
			id := tc.setup(t, d)

			err := d.DeleteAPIKey(context.Background(), id)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("DeleteAPIKey() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				_, getErr := d.GetAPIKey(context.Background(), id)
				if !errors.Is(getErr, ErrNotFound) {
					t.Errorf("GetAPIKey() after delete error = %v, want ErrNotFound", getErr)
				}
			}
		})
	}
}

// ---- GetUserOrgRole ---------------------------------------------------------

func TestGetUserOrgRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		setup    func(t *testing.T, d *DB) (userID, orgID string)
		wantRole string
		wantErr  error
	}{
		{
			name: "returns role from org_membership",
			setup: func(t *testing.T, d *DB) (string, string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "role-org-1"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "member@example.com", DisplayName: "M"})
				_, err := d.CreateOrgMembership(context.Background(), CreateOrgMembershipParams{
					OrgID:  org.ID,
					UserID: user.ID,
					Role:   "org_admin",
				})
				if err != nil {
					t.Fatalf("CreateOrgMembership: %v", err)
				}
				return user.ID, org.ID
			},
			wantRole: "org_admin",
			wantErr:  nil,
		},
		{
			name: "system_admin returns system_admin regardless of membership",
			setup: func(t *testing.T, d *DB) (string, string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "role-org-2"})
				user := mustCreateUser(t, d, CreateUserParams{
					Email:         "sysadmin@example.com",
					DisplayName:   "SA",
					IsSystemAdmin: true,
				})
				return user.ID, org.ID
			},
			wantRole: "system_admin",
			wantErr:  nil,
		},
		{
			name: "no membership and not system_admin returns ErrNotFound",
			setup: func(t *testing.T, d *DB) (string, string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "role-org-3"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "nomember@example.com", DisplayName: "N"})
				return user.ID, org.ID
			},
			wantRole: "",
			wantErr:  ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)
			userID, orgID := tc.setup(t, d)

			got, err := d.GetUserOrgRole(context.Background(), userID, orgID)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetUserOrgRole() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && got != tc.wantRole {
				t.Errorf("GetUserOrgRole() = %q, want %q", got, tc.wantRole)
			}
		})
	}
}
