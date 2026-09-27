package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// ---- LoadActiveKey ------------------------------------------------------------

// TestLoadActiveKey verifies that LoadActiveKey returns a full KeyRecord with
// org, team, and per-user (org membership) limits resolved via JOIN, and
// returns ErrNotFound for deleted, expired, and unknown keys.
func TestLoadActiveKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string // returns the key ID to load
		wantErr error
		check   func(t *testing.T, got *KeyRecord)
	}{
		{
			name: "active key returns full record with org/team/user limits",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{
					Name: "O", Slug: "load-active-org",
					DailyTokenLimit: 100_000, MonthlyTokenLimit: 2_000_000,
					RequestsPerMinute: 60, RequestsPerDay: 5_000,
				})
				user := mustCreateUser(t, d, CreateUserParams{Email: "load-active@example.com", DisplayName: "U"})
				team := mustCreateTeam(t, d, CreateTeamParams{
					OrgID: org.ID, Name: "T", Slug: "t-load-active",
					DailyTokenLimit: 50_000, MonthlyTokenLimit: 1_000_000,
					RequestsPerMinute: 30, RequestsPerDay: 2_500,
				})
				mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: user.ID, Role: "member"})
				_, err := d.UpdateOrgMembership(context.Background(), mustGetMembershipID(t, d, org.ID, user.ID), UpdateOrgMembershipParams{
					DailyTokenLimit:   ptr(int64(5_000)),
					MonthlyTokenLimit: ptr(int64(80_000)),
					RequestsPerMinute: ptr(15),
					RequestsPerDay:    ptr(300),
				})
				if err != nil {
					t.Fatalf("UpdateOrgMembership: %v", err)
				}
				k := mustCreateAPIKey(t, d, CreateAPIKeyParams{
					KeyHash: keygen.Hash("vl_uk_loadactivekeytest0000000000000000000000000000", testHMACSecret),
					KeyHint: keygen.Hint("vl_uk_loadactivekeytest0000000000000000000000000000"),
					KeyType: keygen.KeyTypeUser, Name: "load-active-key",
					OrgID: org.ID, UserID: ptr(user.ID), TeamID: ptr(team.ID),
					DailyTokenLimit: 1_234, RequestsPerMinute: 7,
					CreatedBy: user.ID,
				})
				return k.ID
			},
			wantErr: nil,
			check: func(t *testing.T, got *KeyRecord) {
				t.Helper()
				if got.DailyTokenLimit != 1_234 {
					t.Errorf("DailyTokenLimit = %d, want 1234", got.DailyTokenLimit)
				}
				if got.RequestsPerMinute != 7 {
					t.Errorf("RequestsPerMinute = %d, want 7", got.RequestsPerMinute)
				}
				if got.OrgDailyTokenLimit != 100_000 {
					t.Errorf("OrgDailyTokenLimit = %d, want 100000", got.OrgDailyTokenLimit)
				}
				if got.OrgRequestsPerMinute != 60 {
					t.Errorf("OrgRequestsPerMinute = %d, want 60", got.OrgRequestsPerMinute)
				}
				if got.TeamDailyTokenLimit != 50_000 {
					t.Errorf("TeamDailyTokenLimit = %d, want 50000", got.TeamDailyTokenLimit)
				}
				if got.TeamRequestsPerMinute != 30 {
					t.Errorf("TeamRequestsPerMinute = %d, want 30", got.TeamRequestsPerMinute)
				}
				if got.UserDailyTokenLimit != 5_000 {
					t.Errorf("UserDailyTokenLimit = %d, want 5000", got.UserDailyTokenLimit)
				}
				if got.UserMonthlyTokenLimit != 80_000 {
					t.Errorf("UserMonthlyTokenLimit = %d, want 80000", got.UserMonthlyTokenLimit)
				}
				if got.UserRequestsPerMinute != 15 {
					t.Errorf("UserRequestsPerMinute = %d, want 15", got.UserRequestsPerMinute)
				}
				if got.UserRequestsPerDay != 300 {
					t.Errorf("UserRequestsPerDay = %d, want 300", got.UserRequestsPerDay)
				}
				if got.MembershipRole != "member" {
					t.Errorf("MembershipRole = %q, want %q", got.MembershipRole, "member")
				}
			},
		},
		{
			name: "unknown ID returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				return "00000000-0000-0000-0000-000000000000"
			},
			wantErr: ErrNotFound,
		},
		{
			name: "soft-deleted key returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "load-active-deleted-org"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "load-active-del@example.com", DisplayName: "U"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-load-active-del"})
				k := mustCreateAPIKey(t, d, testKeyParams(org.ID, user.ID, team.ID, user.ID))
				if err := d.DeleteAPIKey(context.Background(), k.ID); err != nil {
					t.Fatalf("DeleteAPIKey: %v", err)
				}
				return k.ID
			},
			wantErr: ErrNotFound,
		},
		{
			name: "expired key returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "load-active-expired-org"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "load-active-exp@example.com", DisplayName: "U"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-load-active-exp"})
				plain := "vl_uk_expiredloadactivekey0000000000000000000000000"
				k := mustCreateAPIKey(t, d, CreateAPIKeyParams{
					KeyHash: keygen.Hash(plain, testHMACSecret), KeyHint: keygen.Hint(plain),
					KeyType: keygen.KeyTypeUser, Name: "expired-key",
					OrgID: org.ID, UserID: ptr(user.ID), TeamID: ptr(team.ID),
					ExpiresAt: ptr("2000-01-01T00:00:00Z"),
					CreatedBy: user.ID,
				})
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

			got, err := d.LoadActiveKey(context.Background(), id)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("LoadActiveKey() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				if got == nil {
					t.Fatal("LoadActiveKey() returned nil, want non-nil")
				}
				if got.ID != id {
					t.Errorf("LoadActiveKey().ID = %q, want %q", got.ID, id)
				}
				if tc.check != nil {
					tc.check(t, got)
				}
			}
		})
	}
}

// mustGetMembershipID looks up the membership ID for a given org/user pair by
// scanning ListOrgMemberships. Test-only convenience since CreateOrgMembership
// does not accept limit fields directly (limits are set via UpdateOrgMembership).
func mustGetMembershipID(t *testing.T, d *DB, orgID, userID string) string {
	t.Helper()
	memberships, err := d.ListOrgMemberships(context.Background(), orgID, "", 1000)
	if err != nil {
		t.Fatalf("mustGetMembershipID: ListOrgMemberships: %v", err)
	}
	for _, m := range memberships {
		if m.UserID == userID {
			return m.ID
		}
	}
	t.Fatalf("mustGetMembershipID: no membership found for org=%s user=%s", orgID, userID)
	return ""
}

// ---- LoadActiveKey / LoadAllActiveKeys — service account metadata -----------

// TestLoadActiveKey_ServiceAccountMetadata verifies that LoadActiveKey resolves
// KeyRecord.ServiceAccountTeamID and ServiceAccountActive from the owning
// service account row via the sa LEFT JOIN, matching the real API path where
// a sa_key's own team_id column is never populated — team scoping for a
// service-account key comes exclusively from the service account it belongs
// to. Deleting the service account (soft-delete) does not delete the key, so
// LoadActiveKey must still return it, but flagged as ServiceAccountActive =
// false so role resolution downgrades to member instead of trusting stale
// service-account scoping.
func TestLoadActiveKey_ServiceAccountMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, d *DB) string // returns the key ID to load
		check func(t *testing.T, got *KeyRecord)
	}{
		{
			name: "team-bound service account: sa_key has no team_id of its own, but resolves ServiceAccountTeamID and ServiceAccountActive from the SA",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "sa-meta-team-org"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "sa-meta-team@example.com", DisplayName: "U"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-sa-meta-team"})
				sa := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name: "Team SA", OrgID: org.ID, TeamID: ptr(team.ID), CreatedBy: user.ID,
				})
				plain := "vl_sa_metateamsakey00000000000000000000000000000000"
				// Mirrors the real API: sa_key rows never carry team_id — team
				// scoping comes only from the service account, so TeamID is
				// intentionally left nil here.
				k := mustCreateAPIKey(t, d, CreateAPIKeyParams{
					KeyHash: keygen.Hash(plain, testHMACSecret), KeyHint: keygen.Hint(plain),
					KeyType: keygen.KeyTypeSA, Name: "team-sa-key",
					OrgID: org.ID, ServiceAccountID: ptr(sa.ID),
					CreatedBy: user.ID,
				})
				return k.ID
			},
			check: func(t *testing.T, got *KeyRecord) {
				t.Helper()
				if got.TeamID != nil {
					t.Errorf("TeamID = %v, want nil (sa_key rows never carry their own team_id)", *got.TeamID)
				}
				if got.ServiceAccountTeamID == nil {
					t.Fatal("ServiceAccountTeamID is nil, want set to the owning team's ID")
				}
				if !got.ServiceAccountActive {
					t.Error("ServiceAccountActive = false, want true for a live service account")
				}
			},
		},
		{
			name: "org-level service account: sa_key resolves ServiceAccountTeamID nil and ServiceAccountActive true",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "sa-meta-org-org"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "sa-meta-org@example.com", DisplayName: "U"})
				sa := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name: "Org SA", OrgID: org.ID, CreatedBy: user.ID,
				})
				plain := "vl_sa_metaorgsakey0000000000000000000000000000000000"
				k := mustCreateAPIKey(t, d, CreateAPIKeyParams{
					KeyHash: keygen.Hash(plain, testHMACSecret), KeyHint: keygen.Hint(plain),
					KeyType: keygen.KeyTypeSA, Name: "org-sa-key",
					OrgID: org.ID, ServiceAccountID: ptr(sa.ID),
					CreatedBy: user.ID,
				})
				return k.ID
			},
			check: func(t *testing.T, got *KeyRecord) {
				t.Helper()
				if got.ServiceAccountTeamID != nil {
					t.Errorf("ServiceAccountTeamID = %q, want nil for an org-scoped service account", *got.ServiceAccountTeamID)
				}
				if !got.ServiceAccountActive {
					t.Error("ServiceAccountActive = false, want true for a live service account")
				}
			},
		},
		{
			name: "soft-deleted service account: sa_key row is still returned (not deleted), but ServiceAccountActive is false",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "sa-meta-deleted-org"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "sa-meta-deleted@example.com", DisplayName: "U"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-sa-meta-deleted"})
				sa := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name: "Deleted SA", OrgID: org.ID, TeamID: ptr(team.ID), CreatedBy: user.ID,
				})
				plain := "vl_sa_metadeletedsakey0000000000000000000000000000000"
				k := mustCreateAPIKey(t, d, CreateAPIKeyParams{
					KeyHash: keygen.Hash(plain, testHMACSecret), KeyHint: keygen.Hint(plain),
					KeyType: keygen.KeyTypeSA, Name: "deleted-sa-key",
					OrgID: org.ID, ServiceAccountID: ptr(sa.ID),
					CreatedBy: user.ID,
				})
				if err := d.DeleteServiceAccount(context.Background(), sa.ID); err != nil {
					t.Fatalf("DeleteServiceAccount: %v", err)
				}
				return k.ID
			},
			check: func(t *testing.T, got *KeyRecord) {
				t.Helper()
				if got.ServiceAccountActive {
					t.Error("ServiceAccountActive = true, want false for a soft-deleted service account")
				}
			},
		},
		{
			name: "user_key has ServiceAccountActive false and ServiceAccountTeamID nil (no owning service account)",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "sa-meta-userkey-org"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "sa-meta-userkey@example.com", DisplayName: "U"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-sa-meta-userkey"})
				k := mustCreateAPIKey(t, d, testKeyParams(org.ID, user.ID, team.ID, user.ID))
				return k.ID
			},
			check: func(t *testing.T, got *KeyRecord) {
				t.Helper()
				if got.ServiceAccountActive {
					t.Error("ServiceAccountActive = true, want false for a user_key with no service account")
				}
				if got.ServiceAccountTeamID != nil {
					t.Errorf("ServiceAccountTeamID = %q, want nil for a user_key", *got.ServiceAccountTeamID)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)
			id := tc.setup(t, d)

			got, err := d.LoadActiveKey(context.Background(), id)
			if err != nil {
				t.Fatalf("LoadActiveKey() error = %v", err)
			}
			tc.check(t, got)
		})
	}
}

// TestLoadAllActiveKeys_ServiceAccountMetadata mirrors
// TestLoadActiveKey_ServiceAccountMetadata for the bulk-load path used by the
// key cache loader, verifying the same service-account metadata is populated
// consistently across multiple rows in a single query.
func TestLoadAllActiveKeys_ServiceAccountMetadata(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "sa-meta-bulk-org"})
	user := mustCreateUser(t, d, CreateUserParams{Email: "sa-meta-bulk@example.com", DisplayName: "U"})
	team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-sa-meta-bulk"})

	teamSA := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
		Name: "Team SA", OrgID: org.ID, TeamID: ptr(team.ID), CreatedBy: user.ID,
	})
	orgSA := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
		Name: "Org SA", OrgID: org.ID, CreatedBy: user.ID,
	})
	deletedSA := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
		Name: "Deleted SA", OrgID: org.ID, TeamID: ptr(team.ID), CreatedBy: user.ID,
	})

	plainTeamSAKey := "vl_sa_bulkteamsakey000000000000000000000000000000000"
	teamSAKey := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash(plainTeamSAKey, testHMACSecret), KeyHint: keygen.Hint(plainTeamSAKey),
		KeyType: keygen.KeyTypeSA, Name: "team-sa-key", OrgID: org.ID, ServiceAccountID: ptr(teamSA.ID), CreatedBy: user.ID,
	})
	plainOrgSAKey := "vl_sa_bulkorgsakey0000000000000000000000000000000000"
	orgSAKey := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash(plainOrgSAKey, testHMACSecret), KeyHint: keygen.Hint(plainOrgSAKey),
		KeyType: keygen.KeyTypeSA, Name: "org-sa-key", OrgID: org.ID, ServiceAccountID: ptr(orgSA.ID), CreatedBy: user.ID,
	})
	plainDeletedSAKey := "vl_sa_bulkdeletedsakey00000000000000000000000000000000"
	deletedSAKey := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash(plainDeletedSAKey, testHMACSecret), KeyHint: keygen.Hint(plainDeletedSAKey),
		KeyType: keygen.KeyTypeSA, Name: "deleted-sa-key", OrgID: org.ID, ServiceAccountID: ptr(deletedSA.ID), CreatedBy: user.ID,
	})
	if err := d.DeleteServiceAccount(ctx, deletedSA.ID); err != nil {
		t.Fatalf("DeleteServiceAccount: %v", err)
	}

	records, skipErrors, err := d.LoadAllActiveKeys(ctx)
	if err != nil {
		t.Fatalf("LoadAllActiveKeys() error = %v", err)
	}
	if len(skipErrors) != 0 {
		t.Fatalf("LoadAllActiveKeys() skipErrors = %v, want none", skipErrors)
	}

	byID := make(map[string]KeyRecord, len(records))
	for _, r := range records {
		byID[r.ID] = r
	}

	tsk, ok := byID[teamSAKey.ID]
	if !ok {
		t.Fatal("team sa_key not found in LoadAllActiveKeys() results")
	}
	if tsk.ServiceAccountTeamID == nil || *tsk.ServiceAccountTeamID != team.ID {
		t.Errorf("team sa_key ServiceAccountTeamID = %v, want %q", tsk.ServiceAccountTeamID, team.ID)
	}
	if !tsk.ServiceAccountActive {
		t.Error("team sa_key ServiceAccountActive = false, want true")
	}

	osk, ok := byID[orgSAKey.ID]
	if !ok {
		t.Fatal("org sa_key not found in LoadAllActiveKeys() results")
	}
	if osk.ServiceAccountTeamID != nil {
		t.Errorf("org sa_key ServiceAccountTeamID = %q, want nil", *osk.ServiceAccountTeamID)
	}
	if !osk.ServiceAccountActive {
		t.Error("org sa_key ServiceAccountActive = false, want true")
	}

	dsk, ok := byID[deletedSAKey.ID]
	if !ok {
		t.Fatal("deleted sa_key not found in LoadAllActiveKeys() results (the key itself is not deleted)")
	}
	if dsk.ServiceAccountActive {
		t.Error("deleted sa_key ServiceAccountActive = true, want false (owning service account is soft-deleted)")
	}
}

// ---- LoadAllActiveKeys — user limits -------------------------------------------

// TestLoadAllActiveKeys_UserLimits verifies that user-level limits are resolved
// via the org_membership LEFT JOIN for user_key rows, and are zero for
// team_key and sa_key rows (which have no org_memberships row to join against).
func TestLoadAllActiveKeys_UserLimits(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "loadall-userlim-org"})
	user := mustCreateUser(t, d, CreateUserParams{Email: "loadall-userlim@example.com", DisplayName: "U"})
	team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-loadall-userlim"})
	mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: user.ID, Role: "member"})

	membershipID := mustGetMembershipID(t, d, org.ID, user.ID)
	if _, err := d.UpdateOrgMembership(ctx, membershipID, UpdateOrgMembershipParams{
		DailyTokenLimit:   ptr(int64(9_000)),
		RequestsPerMinute: ptr(20),
	}); err != nil {
		t.Fatalf("UpdateOrgMembership: %v", err)
	}

	userKey := mustCreateAPIKey(t, d, testKeyParams(org.ID, user.ID, team.ID, user.ID))

	sa, err := d.CreateServiceAccount(ctx, CreateServiceAccountParams{Name: "SA", OrgID: org.ID, CreatedBy: user.ID})
	if err != nil {
		t.Fatalf("CreateServiceAccount: %v", err)
	}
	plainTeam := "vl_tk_loadallteamkey00000000000000000000000000000000"
	teamKey := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash(plainTeam, testHMACSecret), KeyHint: keygen.Hint(plainTeam),
		KeyType: keygen.KeyTypeTeam, Name: "team-key", OrgID: org.ID, TeamID: ptr(team.ID), CreatedBy: user.ID,
	})
	plainSA := "vl_sa_loadallsakey000000000000000000000000000000000"
	saKey := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash(plainSA, testHMACSecret), KeyHint: keygen.Hint(plainSA),
		KeyType: keygen.KeyTypeSA, Name: "sa-key", OrgID: org.ID, ServiceAccountID: ptr(sa.ID), CreatedBy: user.ID,
	})

	records, skipErrors, err := d.LoadAllActiveKeys(ctx)
	if err != nil {
		t.Fatalf("LoadAllActiveKeys() error = %v", err)
	}
	if len(skipErrors) != 0 {
		t.Fatalf("LoadAllActiveKeys() skipErrors = %v, want none", skipErrors)
	}

	byID := make(map[string]KeyRecord, len(records))
	for _, r := range records {
		byID[r.ID] = r
	}

	uk, ok := byID[userKey.ID]
	if !ok {
		t.Fatal("user_key not found in LoadAllActiveKeys() results")
	}
	if uk.UserDailyTokenLimit != 9_000 {
		t.Errorf("user_key UserDailyTokenLimit = %d, want 9000", uk.UserDailyTokenLimit)
	}
	if uk.UserRequestsPerMinute != 20 {
		t.Errorf("user_key UserRequestsPerMinute = %d, want 20", uk.UserRequestsPerMinute)
	}

	tk, ok := byID[teamKey.ID]
	if !ok {
		t.Fatal("team_key not found in LoadAllActiveKeys() results")
	}
	if tk.UserDailyTokenLimit != 0 || tk.UserMonthlyTokenLimit != 0 || tk.UserRequestsPerMinute != 0 || tk.UserRequestsPerDay != 0 {
		t.Errorf("team_key user limits = %+v, want all zero (no owning user)", tk)
	}

	sk, ok := byID[saKey.ID]
	if !ok {
		t.Fatal("sa_key not found in LoadAllActiveKeys() results")
	}
	if sk.UserDailyTokenLimit != 0 || sk.UserMonthlyTokenLimit != 0 || sk.UserRequestsPerMinute != 0 || sk.UserRequestsPerDay != 0 {
		t.Errorf("sa_key user limits = %+v, want all zero (no owning user)", sk)
	}
}

// ---- LoadAllActiveKeys — corrupt expires_at ------------------------------------

// TestLoadAllActiveKeys_InvalidExpiresAtOmitsRawValue verifies that a row
// whose stored expires_at cannot be parsed as RFC3339 is skipped rather than
// aborting the whole load, and that the collected error identifies the key by
// ID without embedding the raw stored value — the error text ends up in logs
// via the key cache loader's skipErrors, and the raw value must not leak there.
func TestLoadAllActiveKeys_InvalidExpiresAtOmitsRawValue(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "loadall-badexpiry-org"})
	user := mustCreateUser(t, d, CreateUserParams{Email: "loadall-badexpiry@example.com", DisplayName: "U"})

	const rawValue = "not-a-real-timestamp-marker"
	plainBad := "vl_uk_loadallbadexpirykeytest00000000000000000000000"
	badKey := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash(plainBad, testHMACSecret), KeyHint: keygen.Hint(plainBad),
		KeyType: keygen.KeyTypeUser, Name: "bad-expiry-key",
		OrgID: org.ID, UserID: ptr(user.ID),
		ExpiresAt: ptr(rawValue),
		CreatedBy: user.ID,
	})

	records, skipErrors, err := d.LoadAllActiveKeys(ctx)
	if err != nil {
		t.Fatalf("LoadAllActiveKeys() error = %v", err)
	}
	if len(skipErrors) != 1 {
		t.Fatalf("LoadAllActiveKeys() skipErrors = %v, want exactly 1", skipErrors)
	}
	for _, r := range records {
		if r.ID == badKey.ID {
			t.Fatalf("bad-expiry key %s should have been skipped, not returned", badKey.ID)
		}
	}

	msg := skipErrors[0].Error()
	if strings.Contains(msg, rawValue) {
		t.Errorf("skip error = %q, must not contain the raw expires_at value %q", msg, rawValue)
	}
	if !strings.Contains(msg, badKey.ID) {
		t.Errorf("skip error = %q, want it to include the key id %q", msg, badKey.ID)
	}
	if !errors.Is(skipErrors[0], ErrInvalidTimestamp) {
		t.Errorf("skip error = %v, want errors.Is(err, ErrInvalidTimestamp)", skipErrors[0])
	}
}
