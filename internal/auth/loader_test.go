package auth

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// loaderHMACSecret is a stable HMAC secret used across all loader tests.
var loaderHMACSecret = []byte("loader-test-hmac-secret-32bytes!")

// openLoaderDB opens an isolated in-memory SQLite DB, runs all migrations,
// and registers cleanup. The test name is sanitised to form a unique URI.
// MaxOpenConns=1 matches production behaviour; the single-query JOIN in
// LoadKeysIntoCache means no nested queries are issued while the rows cursor
// is held, so a pool of 1 does not deadlock.
func openLoaderDB(t *testing.T) *db.DB {
	t.Helper()
	safeName := strings.NewReplacer("/", "_", " ", "_", "#", "_", "=", "_").Replace(t.Name())
	cfg := config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             "file:" + safeName + "?mode=memory&cache=shared",
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	}
	ctx := context.Background()
	d, err := db.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if err := db.RunMigrations(ctx, d.SQL(), d.Dialect(), slog.Default()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	return d
}

// createOrg inserts an organization and returns its ID.
func createOrg(t *testing.T, d *db.DB, name, slug string, limits db.CreateOrgParams) string {
	t.Helper()
	limits.Name = name
	limits.Slug = slug
	org, err := d.CreateOrg(context.Background(), limits)
	if err != nil {
		t.Fatalf("CreateOrg(%q): %v", slug, err)
	}
	return org.ID
}

// createUser inserts a user and returns its ID. isSystemAdmin controls the flag.
func createUser(t *testing.T, d *db.DB, email string, isSystemAdmin bool) string {
	t.Helper()
	user, err := d.CreateUser(context.Background(), db.CreateUserParams{
		Email:         email,
		DisplayName:   "Test User",
		AuthProvider:  "local",
		IsSystemAdmin: isSystemAdmin,
	})
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", email, err)
	}
	return user.ID
}

// createMembership inserts an org membership for the given user/org/role.
func createMembership(t *testing.T, d *db.DB, orgID, userID, role string) {
	t.Helper()
	_, err := d.CreateOrgMembership(context.Background(), db.CreateOrgMembershipParams{
		OrgID:  orgID,
		UserID: userID,
		Role:   role,
	})
	if err != nil {
		t.Fatalf("CreateOrgMembership(org=%s, user=%s, role=%s): %v", orgID, userID, role, err)
	}
}

// createTeam inserts a team and returns its ID.
func createTeam(t *testing.T, d *db.DB, orgID, name, slug string, limits db.CreateTeamParams) string {
	t.Helper()
	limits.OrgID = orgID
	limits.Name = name
	limits.Slug = slug
	team, err := d.CreateTeam(context.Background(), limits)
	if err != nil {
		t.Fatalf("CreateTeam(%q): %v", slug, err)
	}
	return team.ID
}

// createSA inserts a service account and returns its ID. teamID may be nil for org-scoped SAs.
func createSA(t *testing.T, d *db.DB, orgID string, teamID *string, createdBy string) string {
	t.Helper()
	sa, err := d.CreateServiceAccount(context.Background(), db.CreateServiceAccountParams{
		Name:      "Test SA",
		OrgID:     orgID,
		TeamID:    teamID,
		CreatedBy: createdBy,
	})
	if err != nil {
		t.Fatalf("CreateServiceAccount: %v", err)
	}
	return sa.ID
}

// insertKey inserts an api_key directly using db.CreateAPIKey and returns the
// plaintext key so callers can compute its hash.
func insertKey(t *testing.T, d *db.DB, params db.CreateAPIKeyParams) string {
	t.Helper()
	plaintext, err := keygen.Generate(params.KeyType)
	if err != nil {
		t.Fatalf("keygen.Generate(%s): %v", params.KeyType, err)
	}
	params.KeyHash = keygen.Hash(plaintext, loaderHMACSecret)
	params.KeyHint = keygen.Hint(plaintext)
	_, err = d.CreateAPIKey(context.Background(), params)
	if err != nil {
		t.Fatalf("CreateAPIKey(%q): %v", params.Name, err)
	}
	return plaintext
}

// softDeleteKey sets deleted_at on an api_key to simulate a soft-delete.
func softDeleteKey(t *testing.T, d *db.DB, plaintext string) {
	t.Helper()
	hash := keygen.Hash(plaintext, loaderHMACSecret)
	_, err := d.SQL().ExecContext(context.Background(),
		"UPDATE api_keys SET deleted_at = CURRENT_TIMESTAMP WHERE key_hash = ?", hash)
	if err != nil {
		t.Fatalf("softDeleteKey: %v", err)
	}
}

// ---- TestLoadKeysIntoCache --------------------------------------------------

func TestLoadKeysIntoCache(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, d *db.DB) []string // returns plaintext keys inserted
		wantLen   int
		checkKeys func(t *testing.T, kc *cache.Cache[string, KeyInfo], plaintexts []string)
	}{
		{
			name:    "no keys in DB returns empty cache",
			setup:   func(t *testing.T, d *db.DB) []string { return nil },
			wantLen: 0,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], _ []string) {
				if kc.Len() != 0 {
					t.Errorf("cache.Len() = %d, want 0", kc.Len())
				}
			},
		},
		{
			name: "one user_key has correct KeyType and OrgID",
			setup: func(t *testing.T, d *db.DB) []string {
				t.Helper()
				orgID := createOrg(t, d, "Org1", "org1", db.CreateOrgParams{})
				userID := createUser(t, d, "u1@example.com", false)
				createMembership(t, d, orgID, userID, RoleMember)
				pt := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:   keygen.KeyTypeUser,
					Name:      "uk1",
					OrgID:     orgID,
					UserID:    ptrStr(userID),
					CreatedBy: userID,
				})
				return []string{pt}
			},
			wantLen: 1,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], pts []string) {
				t.Helper()
				ki, ok := kc.Get(keygen.Hash(pts[0], loaderHMACSecret))
				if !ok {
					t.Fatal("key not found in cache")
				}
				if ki.KeyType != keygen.KeyTypeUser {
					t.Errorf("KeyType = %q, want %q", ki.KeyType, keygen.KeyTypeUser)
				}
				if ki.OrgID == "" {
					t.Error("OrgID is empty")
				}
			},
		},
		{
			name: "user_key with system_admin user gets role system_admin",
			setup: func(t *testing.T, d *db.DB) []string {
				t.Helper()
				orgID := createOrg(t, d, "OrgSA", "org-sa", db.CreateOrgParams{})
				userID := createUser(t, d, "sysadmin@example.com", true)
				createMembership(t, d, orgID, userID, RoleOrgAdmin)
				pt := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:   keygen.KeyTypeUser,
					Name:      "sa-key",
					OrgID:     orgID,
					UserID:    ptrStr(userID),
					CreatedBy: userID,
				})
				return []string{pt}
			},
			wantLen: 1,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], pts []string) {
				t.Helper()
				ki, ok := kc.Get(keygen.Hash(pts[0], loaderHMACSecret))
				if !ok {
					t.Fatal("key not found in cache")
				}
				if ki.Role != RoleSystemAdmin {
					t.Errorf("Role = %q, want %q", ki.Role, RoleSystemAdmin)
				}
			},
		},
		{
			name: "user_key with org_admin membership gets role org_admin",
			setup: func(t *testing.T, d *db.DB) []string {
				t.Helper()
				orgID := createOrg(t, d, "OrgOA", "org-oa", db.CreateOrgParams{})
				userID := createUser(t, d, "orgadmin@example.com", false)
				createMembership(t, d, orgID, userID, RoleOrgAdmin)
				pt := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:   keygen.KeyTypeUser,
					Name:      "oa-key",
					OrgID:     orgID,
					UserID:    ptrStr(userID),
					CreatedBy: userID,
				})
				return []string{pt}
			},
			wantLen: 1,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], pts []string) {
				t.Helper()
				ki, ok := kc.Get(keygen.Hash(pts[0], loaderHMACSecret))
				if !ok {
					t.Fatal("key not found in cache")
				}
				if ki.Role != RoleOrgAdmin {
					t.Errorf("Role = %q, want %q", ki.Role, RoleOrgAdmin)
				}
			},
		},
		{
			name: "user_key with member membership gets role member",
			setup: func(t *testing.T, d *db.DB) []string {
				t.Helper()
				orgID := createOrg(t, d, "OrgMem", "org-mem", db.CreateOrgParams{})
				userID := createUser(t, d, "member@example.com", false)
				createMembership(t, d, orgID, userID, RoleMember)
				pt := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:   keygen.KeyTypeUser,
					Name:      "mem-key",
					OrgID:     orgID,
					UserID:    ptrStr(userID),
					CreatedBy: userID,
				})
				return []string{pt}
			},
			wantLen: 1,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], pts []string) {
				t.Helper()
				ki, ok := kc.Get(keygen.Hash(pts[0], loaderHMACSecret))
				if !ok {
					t.Fatal("key not found in cache")
				}
				if ki.Role != RoleMember {
					t.Errorf("Role = %q, want %q", ki.Role, RoleMember)
				}
			},
		},
		{
			name: "team_key gets role team_admin",
			setup: func(t *testing.T, d *db.DB) []string {
				t.Helper()
				orgID := createOrg(t, d, "OrgTK", "org-tk", db.CreateOrgParams{})
				userID := createUser(t, d, "tk-creator@example.com", false)
				createMembership(t, d, orgID, userID, RoleOrgAdmin)
				teamID := createTeam(t, d, orgID, "TeamTK", "team-tk", db.CreateTeamParams{})
				pt := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:   keygen.KeyTypeTeam,
					Name:      "tk-key",
					OrgID:     orgID,
					TeamID:    ptrStr(teamID),
					CreatedBy: userID,
				})
				return []string{pt}
			},
			wantLen: 1,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], pts []string) {
				t.Helper()
				ki, ok := kc.Get(keygen.Hash(pts[0], loaderHMACSecret))
				if !ok {
					t.Fatal("key not found in cache")
				}
				if ki.Role != RoleTeamAdmin {
					t.Errorf("Role = %q, want %q", ki.Role, RoleTeamAdmin)
				}
			},
		},
		{
			name: "sa_key org-scoped (no team) gets role org_admin",
			setup: func(t *testing.T, d *db.DB) []string {
				t.Helper()
				orgID := createOrg(t, d, "OrgSAOrg", "org-sa-org", db.CreateOrgParams{})
				userID := createUser(t, d, "sa-org-creator@example.com", false)
				createMembership(t, d, orgID, userID, RoleOrgAdmin)
				saID := createSA(t, d, orgID, nil, userID)
				pt := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:          keygen.KeyTypeSA,
					Name:             "sa-org-key",
					OrgID:            orgID,
					ServiceAccountID: ptrStr(saID),
					CreatedBy:        userID,
				})
				return []string{pt}
			},
			wantLen: 1,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], pts []string) {
				t.Helper()
				ki, ok := kc.Get(keygen.Hash(pts[0], loaderHMACSecret))
				if !ok {
					t.Fatal("key not found in cache")
				}
				if ki.Role != RoleOrgAdmin {
					t.Errorf("Role = %q, want %q", ki.Role, RoleOrgAdmin)
				}
				if ki.TeamID != "" {
					t.Errorf("TeamID = %q, want empty for org-scoped SA", ki.TeamID)
				}
			},
		},
		{
			name: "sa_key team-scoped gets role team_admin",
			setup: func(t *testing.T, d *db.DB) []string {
				t.Helper()
				orgID := createOrg(t, d, "OrgSATeam", "org-sa-team", db.CreateOrgParams{})
				userID := createUser(t, d, "sa-team-creator@example.com", false)
				createMembership(t, d, orgID, userID, RoleOrgAdmin)
				teamID := createTeam(t, d, orgID, "SA Team", "sa-team", db.CreateTeamParams{})
				saID := createSA(t, d, orgID, ptrStr(teamID), userID)
				pt := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:          keygen.KeyTypeSA,
					Name:             "sa-team-key",
					OrgID:            orgID,
					TeamID:           ptrStr(teamID),
					ServiceAccountID: ptrStr(saID),
					CreatedBy:        userID,
				})
				return []string{pt}
			},
			wantLen: 1,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], pts []string) {
				t.Helper()
				ki, ok := kc.Get(keygen.Hash(pts[0], loaderHMACSecret))
				if !ok {
					t.Fatal("key not found in cache")
				}
				if ki.Role != RoleTeamAdmin {
					t.Errorf("Role = %q, want %q", ki.Role, RoleTeamAdmin)
				}
				if ki.TeamID == "" {
					t.Error("TeamID is empty, want non-empty for team-scoped SA")
				}
			},
		},
		{
			name: "org limits populated from org table",
			setup: func(t *testing.T, d *db.DB) []string {
				t.Helper()
				orgID := createOrg(t, d, "OrgLim", "org-lim", db.CreateOrgParams{
					DailyTokenLimit:   100_000,
					MonthlyTokenLimit: 2_000_000,
					RequestsPerMinute: 60,
					RequestsPerDay:    5_000,
				})
				userID := createUser(t, d, "orglim@example.com", false)
				createMembership(t, d, orgID, userID, RoleMember)
				pt := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:   keygen.KeyTypeUser,
					Name:      "orglim-key",
					OrgID:     orgID,
					UserID:    ptrStr(userID),
					CreatedBy: userID,
				})
				return []string{pt}
			},
			wantLen: 1,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], pts []string) {
				t.Helper()
				ki, ok := kc.Get(keygen.Hash(pts[0], loaderHMACSecret))
				if !ok {
					t.Fatal("key not found in cache")
				}
				if ki.OrgDailyTokenLimit != 100_000 {
					t.Errorf("OrgDailyTokenLimit = %d, want 100000", ki.OrgDailyTokenLimit)
				}
				if ki.OrgMonthlyTokenLimit != 2_000_000 {
					t.Errorf("OrgMonthlyTokenLimit = %d, want 2000000", ki.OrgMonthlyTokenLimit)
				}
				if ki.OrgRequestsPerMinute != 60 {
					t.Errorf("OrgRequestsPerMinute = %d, want 60", ki.OrgRequestsPerMinute)
				}
				if ki.OrgRequestsPerDay != 5_000 {
					t.Errorf("OrgRequestsPerDay = %d, want 5000", ki.OrgRequestsPerDay)
				}
			},
		},
		{
			name: "team limits populated from team table",
			setup: func(t *testing.T, d *db.DB) []string {
				t.Helper()
				orgID := createOrg(t, d, "OrgTL", "org-tl", db.CreateOrgParams{})
				userID := createUser(t, d, "teamlim@example.com", false)
				createMembership(t, d, orgID, userID, RoleMember)
				teamID := createTeam(t, d, orgID, "LimTeam", "lim-team", db.CreateTeamParams{
					DailyTokenLimit:   50_000,
					MonthlyTokenLimit: 1_000_000,
					RequestsPerMinute: 30,
					RequestsPerDay:    2_500,
				})
				pt := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:   keygen.KeyTypeUser,
					Name:      "teamlim-key",
					OrgID:     orgID,
					TeamID:    ptrStr(teamID),
					UserID:    ptrStr(userID),
					CreatedBy: userID,
				})
				return []string{pt}
			},
			wantLen: 1,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], pts []string) {
				t.Helper()
				ki, ok := kc.Get(keygen.Hash(pts[0], loaderHMACSecret))
				if !ok {
					t.Fatal("key not found in cache")
				}
				if ki.TeamDailyTokenLimit != 50_000 {
					t.Errorf("TeamDailyTokenLimit = %d, want 50000", ki.TeamDailyTokenLimit)
				}
				if ki.TeamMonthlyTokenLimit != 1_000_000 {
					t.Errorf("TeamMonthlyTokenLimit = %d, want 1000000", ki.TeamMonthlyTokenLimit)
				}
				if ki.TeamRequestsPerMinute != 30 {
					t.Errorf("TeamRequestsPerMinute = %d, want 30", ki.TeamRequestsPerMinute)
				}
				if ki.TeamRequestsPerDay != 2_500 {
					t.Errorf("TeamRequestsPerDay = %d, want 2500", ki.TeamRequestsPerDay)
				}
			},
		},
		{
			name: "key without team has all team limits zero",
			setup: func(t *testing.T, d *db.DB) []string {
				t.Helper()
				orgID := createOrg(t, d, "OrgNT", "org-nt", db.CreateOrgParams{})
				userID := createUser(t, d, "noteam@example.com", false)
				createMembership(t, d, orgID, userID, RoleMember)
				pt := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:   keygen.KeyTypeUser,
					Name:      "noteam-key",
					OrgID:     orgID,
					UserID:    ptrStr(userID),
					CreatedBy: userID,
				})
				return []string{pt}
			},
			wantLen: 1,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], pts []string) {
				t.Helper()
				ki, ok := kc.Get(keygen.Hash(pts[0], loaderHMACSecret))
				if !ok {
					t.Fatal("key not found in cache")
				}
				if ki.TeamDailyTokenLimit != 0 {
					t.Errorf("TeamDailyTokenLimit = %d, want 0", ki.TeamDailyTokenLimit)
				}
				if ki.TeamMonthlyTokenLimit != 0 {
					t.Errorf("TeamMonthlyTokenLimit = %d, want 0", ki.TeamMonthlyTokenLimit)
				}
				if ki.TeamRequestsPerMinute != 0 {
					t.Errorf("TeamRequestsPerMinute = %d, want 0", ki.TeamRequestsPerMinute)
				}
				if ki.TeamRequestsPerDay != 0 {
					t.Errorf("TeamRequestsPerDay = %d, want 0", ki.TeamRequestsPerDay)
				}
			},
		},
		{
			name: "user limits populated from org membership",
			setup: func(t *testing.T, d *db.DB) []string {
				t.Helper()
				orgID := createOrg(t, d, "OrgUL", "org-ul", db.CreateOrgParams{})
				userID := createUser(t, d, "userlim@example.com", false)
				createMembership(t, d, orgID, userID, RoleMember)
				_, err := d.UpdateOrgMembership(context.Background(), mustFindMembershipID(t, d, orgID, userID), db.UpdateOrgMembershipParams{
					DailyTokenLimit:   ptrInt64(60_000),
					MonthlyTokenLimit: ptrInt64(900_000),
					RequestsPerMinute: ptrInt(20),
					RequestsPerDay:    ptrInt(1_500),
				})
				if err != nil {
					t.Fatalf("UpdateOrgMembership: %v", err)
				}
				pt := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:   keygen.KeyTypeUser,
					Name:      "userlim-key",
					OrgID:     orgID,
					UserID:    ptrStr(userID),
					CreatedBy: userID,
				})
				return []string{pt}
			},
			wantLen: 1,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], pts []string) {
				t.Helper()
				ki, ok := kc.Get(keygen.Hash(pts[0], loaderHMACSecret))
				if !ok {
					t.Fatal("key not found in cache")
				}
				if ki.UserDailyTokenLimit != 60_000 {
					t.Errorf("UserDailyTokenLimit = %d, want 60000", ki.UserDailyTokenLimit)
				}
				if ki.UserMonthlyTokenLimit != 900_000 {
					t.Errorf("UserMonthlyTokenLimit = %d, want 900000", ki.UserMonthlyTokenLimit)
				}
				if ki.UserRequestsPerMinute != 20 {
					t.Errorf("UserRequestsPerMinute = %d, want 20", ki.UserRequestsPerMinute)
				}
				if ki.UserRequestsPerDay != 1_500 {
					t.Errorf("UserRequestsPerDay = %d, want 1500", ki.UserRequestsPerDay)
				}
			},
		},
		{
			name: "key without user limits has all user limits zero",
			setup: func(t *testing.T, d *db.DB) []string {
				t.Helper()
				orgID := createOrg(t, d, "OrgNUL", "org-nul", db.CreateOrgParams{})
				userID := createUser(t, d, "nouserlim@example.com", false)
				createMembership(t, d, orgID, userID, RoleMember)
				pt := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:   keygen.KeyTypeUser,
					Name:      "nouserlim-key",
					OrgID:     orgID,
					UserID:    ptrStr(userID),
					CreatedBy: userID,
				})
				return []string{pt}
			},
			wantLen: 1,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], pts []string) {
				t.Helper()
				ki, ok := kc.Get(keygen.Hash(pts[0], loaderHMACSecret))
				if !ok {
					t.Fatal("key not found in cache")
				}
				if ki.UserDailyTokenLimit != 0 || ki.UserMonthlyTokenLimit != 0 || ki.UserRequestsPerMinute != 0 || ki.UserRequestsPerDay != 0 {
					t.Errorf("user limits = %+v, want all zero (membership has no limits set)", ki)
				}
			},
		},
		{
			name: "team_key has all user limits zero (no owning user)",
			setup: func(t *testing.T, d *db.DB) []string {
				t.Helper()
				orgID := createOrg(t, d, "OrgTKUL", "org-tkul", db.CreateOrgParams{})
				userID := createUser(t, d, "tkul-creator@example.com", false)
				createMembership(t, d, orgID, userID, RoleOrgAdmin)
				teamID := createTeam(t, d, orgID, "TeamTKUL", "team-tkul", db.CreateTeamParams{})
				pt := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:   keygen.KeyTypeTeam,
					Name:      "tkul-key",
					OrgID:     orgID,
					TeamID:    ptrStr(teamID),
					CreatedBy: userID,
				})
				return []string{pt}
			},
			wantLen: 1,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], pts []string) {
				t.Helper()
				ki, ok := kc.Get(keygen.Hash(pts[0], loaderHMACSecret))
				if !ok {
					t.Fatal("key not found in cache")
				}
				if ki.UserDailyTokenLimit != 0 || ki.UserMonthlyTokenLimit != 0 || ki.UserRequestsPerMinute != 0 || ki.UserRequestsPerDay != 0 {
					t.Errorf("team_key user limits = %+v, want all zero", ki)
				}
			},
		},
		{
			name: "deleted key is not loaded into cache",
			setup: func(t *testing.T, d *db.DB) []string {
				t.Helper()
				orgID := createOrg(t, d, "OrgDel", "org-del", db.CreateOrgParams{})
				userID := createUser(t, d, "del@example.com", false)
				createMembership(t, d, orgID, userID, RoleMember)
				pt := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:   keygen.KeyTypeUser,
					Name:      "del-key",
					OrgID:     orgID,
					UserID:    ptrStr(userID),
					CreatedBy: userID,
				})
				softDeleteKey(t, d, pt)
				return []string{pt}
			},
			wantLen: 0,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], pts []string) {
				t.Helper()
				if _, ok := kc.Get(keygen.Hash(pts[0], loaderHMACSecret)); ok {
					t.Error("deleted key found in cache, want absent")
				}
			},
		},
		{
			name: "multiple keys all loaded",
			setup: func(t *testing.T, d *db.DB) []string {
				t.Helper()
				orgID := createOrg(t, d, "OrgMulti", "org-multi", db.CreateOrgParams{})
				u1 := createUser(t, d, "multi1@example.com", false)
				u2 := createUser(t, d, "multi2@example.com", false)
				u3 := createUser(t, d, "multi3@example.com", false)
				createMembership(t, d, orgID, u1, RoleMember)
				createMembership(t, d, orgID, u2, RoleMember)
				createMembership(t, d, orgID, u3, RoleMember)
				pts := make([]string, 3)
				for i, uid := range []string{u1, u2, u3} {
					pts[i] = insertKey(t, d, db.CreateAPIKeyParams{
						KeyType:   keygen.KeyTypeUser,
						Name:      "multi-key-" + uid,
						OrgID:     orgID,
						UserID:    ptrStr(uid),
						CreatedBy: uid,
					})
				}
				return pts
			},
			wantLen: 3,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], pts []string) {
				t.Helper()
				for _, pt := range pts {
					if _, ok := kc.Get(keygen.Hash(pt, loaderHMACSecret)); !ok {
						t.Errorf("key with hint %q not found in cache", keygen.Hint(pt))
					}
				}
			},
		},
		{
			name: "key limits preserved from api_keys table",
			setup: func(t *testing.T, d *db.DB) []string {
				t.Helper()
				orgID := createOrg(t, d, "OrgKL", "org-kl", db.CreateOrgParams{})
				userID := createUser(t, d, "keylim@example.com", false)
				createMembership(t, d, orgID, userID, RoleMember)
				pt := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:           keygen.KeyTypeUser,
					Name:              "keylim-key",
					OrgID:             orgID,
					UserID:            ptrStr(userID),
					CreatedBy:         userID,
					DailyTokenLimit:   12_345,
					MonthlyTokenLimit: 678_900,
					RequestsPerMinute: 15,
					RequestsPerDay:    750,
				})
				return []string{pt}
			},
			wantLen: 1,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], pts []string) {
				t.Helper()
				ki, ok := kc.Get(keygen.Hash(pts[0], loaderHMACSecret))
				if !ok {
					t.Fatal("key not found in cache")
				}
				if ki.DailyTokenLimit != 12_345 {
					t.Errorf("DailyTokenLimit = %d, want 12345", ki.DailyTokenLimit)
				}
				if ki.MonthlyTokenLimit != 678_900 {
					t.Errorf("MonthlyTokenLimit = %d, want 678900", ki.MonthlyTokenLimit)
				}
				if ki.RequestsPerMinute != 15 {
					t.Errorf("RequestsPerMinute = %d, want 15", ki.RequestsPerMinute)
				}
				if ki.RequestsPerDay != 750 {
					t.Errorf("RequestsPerDay = %d, want 750", ki.RequestsPerDay)
				}
			},
		},
		{
			name: "LoadAll replaces cache — deleted key disappears on reload",
			setup: func(t *testing.T, d *db.DB) []string {
				t.Helper()
				orgID := createOrg(t, d, "OrgRepl", "org-repl", db.CreateOrgParams{})
				u1 := createUser(t, d, "repl1@example.com", false)
				u2 := createUser(t, d, "repl2@example.com", false)
				createMembership(t, d, orgID, u1, RoleMember)
				createMembership(t, d, orgID, u2, RoleMember)
				pt1 := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:   keygen.KeyTypeUser,
					Name:      "repl-key-1",
					OrgID:     orgID,
					UserID:    ptrStr(u1),
					CreatedBy: u1,
				})
				pt2 := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:   keygen.KeyTypeUser,
					Name:      "repl-key-2",
					OrgID:     orgID,
					UserID:    ptrStr(u2),
					CreatedBy: u2,
				})
				return []string{pt1, pt2}
			},
			wantLen: 2,
			checkKeys: func(t *testing.T, kc *cache.Cache[string, KeyInfo], pts []string) {
				// Both keys present after first load — verified by wantLen above.
				// Now simulate a second load where pt2 was deleted from DB.
				// We cannot re-run setup here, so we drive the reload scenario
				// from within checkKeys by soft-deleting via raw SQL on the
				// already-configured DB. The DB is not accessible here, so we
				// instead verify the baseline and delegate the reload sub-scenario
				// to TestLoadKeysIntoCache_LoadAllReplacesCache.
				t.Helper()
				for _, pt := range pts {
					if _, ok := kc.Get(keygen.Hash(pt, loaderHMACSecret)); !ok {
						t.Errorf("expected key %q in cache after first load", keygen.Hint(pt))
					}
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := openLoaderDB(t)
			kc := cache.New[string, KeyInfo]()
			log := discardLogger()

			pts := tc.setup(t, d)

			if err := LoadKeysIntoCache(context.Background(), d, kc, log); err != nil {
				t.Fatalf("LoadKeysIntoCache() error = %v", err)
			}

			if kc.Len() != tc.wantLen {
				t.Errorf("cache.Len() = %d, want %d", kc.Len(), tc.wantLen)
			}

			if tc.checkKeys != nil {
				tc.checkKeys(t, kc, pts)
			}
		})
	}
}

// TestLoadKeysIntoCache_LoadAllReplacesCache verifies that a second call to
// LoadKeysIntoCache shrinks the cache when a key has been soft-deleted between
// the two calls.
func TestLoadKeysIntoCache_LoadAllReplacesCache(t *testing.T) {
	t.Parallel()

	d := openLoaderDB(t)
	kc := cache.New[string, KeyInfo]()
	log := discardLogger()
	ctx := context.Background()

	orgID := createOrg(t, d, "OrgReload", "org-reload", db.CreateOrgParams{})
	u1 := createUser(t, d, "reload1@example.com", false)
	u2 := createUser(t, d, "reload2@example.com", false)
	createMembership(t, d, orgID, u1, RoleMember)
	createMembership(t, d, orgID, u2, RoleMember)

	pt1 := insertKey(t, d, db.CreateAPIKeyParams{
		KeyType:   keygen.KeyTypeUser,
		Name:      "reload-key-1",
		OrgID:     orgID,
		UserID:    ptrStr(u1),
		CreatedBy: u1,
	})
	pt2 := insertKey(t, d, db.CreateAPIKeyParams{
		KeyType:   keygen.KeyTypeUser,
		Name:      "reload-key-2",
		OrgID:     orgID,
		UserID:    ptrStr(u2),
		CreatedBy: u2,
	})

	// First load — both keys present.
	if err := LoadKeysIntoCache(ctx, d, kc, log); err != nil {
		t.Fatalf("first LoadKeysIntoCache() error = %v", err)
	}
	if kc.Len() != 2 {
		t.Fatalf("cache.Len() after first load = %d, want 2", kc.Len())
	}

	// Soft-delete pt2 from the database.
	softDeleteKey(t, d, pt2)

	// Second load — only pt1 should remain.
	if err := LoadKeysIntoCache(ctx, d, kc, log); err != nil {
		t.Fatalf("second LoadKeysIntoCache() error = %v", err)
	}
	if kc.Len() != 1 {
		t.Errorf("cache.Len() after reload = %d, want 1", kc.Len())
	}
	if _, ok := kc.Get(keygen.Hash(pt1, loaderHMACSecret)); !ok {
		t.Error("pt1 not found in cache after reload, want present")
	}
	if _, ok := kc.Get(keygen.Hash(pt2, loaderHMACSecret)); ok {
		t.Error("pt2 still in cache after reload, want absent")
	}
}

// ---- TestStartCacheRefresh --------------------------------------------------

// TestStartCacheRefresh verifies that the background goroutine picks up a key
// inserted into the database after it starts.
func TestStartCacheRefresh(t *testing.T) {
	t.Parallel()

	d := openLoaderDB(t)
	kc := cache.New[string, KeyInfo]()
	log := discardLogger()

	// Start the refresh loop with a short interval.
	stop := StartCacheRefresh(d, kc, 50*time.Millisecond, log)
	t.Cleanup(stop)

	// Insert a key after the goroutine is already running.
	orgID := createOrg(t, d, "OrgRefresh", "org-refresh", db.CreateOrgParams{})
	userID := createUser(t, d, "refresh@example.com", false)
	createMembership(t, d, orgID, userID, RoleMember)
	plaintext := insertKey(t, d, db.CreateAPIKeyParams{
		KeyType:   keygen.KeyTypeUser,
		Name:      "refresh-key",
		OrgID:     orgID,
		UserID:    ptrStr(userID),
		CreatedBy: userID,
	})
	hash := keygen.Hash(plaintext, loaderHMACSecret)

	// Wait up to 500ms for the refresh goroutine to pick up the key.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, ok := kc.Get(hash); ok {
			return // key appeared — test passes
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Error("key did not appear in cache within 500ms after StartCacheRefresh")
}

// ---- TestLoadKeysIntoCache_ServiceAccountKeyRoleResolution -------------------

// softDeleteServiceAccount soft-deletes a service account via the normal DB
// function, mirroring the real API path (DeleteServiceAccount), rather than
// poking deleted_at directly.
func softDeleteServiceAccount(t *testing.T, d *db.DB, saID string) {
	t.Helper()
	if err := d.DeleteServiceAccount(context.Background(), saID); err != nil {
		t.Fatalf("DeleteServiceAccount(%q): %v", saID, err)
	}
}

// TestLoadKeysIntoCache_ServiceAccountKeyRoleResolution is the regression test
// for the sa_key role-resolution fix: previously every sa_key resolved to
// org_admin because api_keys.team_id is never populated for sa_key rows (the
// real create-key API rejects team_id on sa_key requests — see
// admin.CreateAPIKey). The role must instead come from the owning service
// account's own team_id, resolved via LoadActiveKey/LoadAllActiveKeys's LEFT
// JOIN on service_accounts. Every sa_key created here mirrors the real API:
// no team_id is ever set on the key row itself.
func TestLoadKeysIntoCache_ServiceAccountKeyRoleResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setup      func(t *testing.T, d *db.DB) string // returns the plaintext sa_key
		wantAbsent bool                                // true: the key must not be present in the cache at all
		wantRole   string                              // checked only when wantAbsent is false
	}{
		{
			name: "team-bound service account: sa_key resolves team_admin",
			setup: func(t *testing.T, d *db.DB) string {
				t.Helper()
				orgID := createOrg(t, d, "OrgSARole1", "org-sarole-team", db.CreateOrgParams{})
				userID := createUser(t, d, "sarole-team@example.com", false)
				createMembership(t, d, orgID, userID, RoleOrgAdmin)
				teamID := createTeam(t, d, orgID, "SARoleTeam", "sarole-team", db.CreateTeamParams{})
				saID := createSA(t, d, orgID, ptrStr(teamID), userID)
				// No TeamID set on the key row — matches the real API, which
				// rejects team_id on sa_key create requests.
				return insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:          keygen.KeyTypeSA,
					Name:             "sarole-team-key",
					OrgID:            orgID,
					ServiceAccountID: ptrStr(saID),
					CreatedBy:        userID,
				})
			},
			wantRole: RoleTeamAdmin,
		},
		{
			name: "org-level service account: sa_key resolves org_admin",
			setup: func(t *testing.T, d *db.DB) string {
				t.Helper()
				orgID := createOrg(t, d, "OrgSARole2", "org-sarole-org", db.CreateOrgParams{})
				userID := createUser(t, d, "sarole-org@example.com", false)
				createMembership(t, d, orgID, userID, RoleOrgAdmin)
				saID := createSA(t, d, orgID, nil, userID)
				return insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:          keygen.KeyTypeSA,
					Name:             "sarole-org-key",
					OrgID:            orgID,
					ServiceAccountID: ptrStr(saID),
					CreatedBy:        userID,
				})
			},
			wantRole: RoleOrgAdmin,
		},
		{
			name: "soft-deleted service account: sa_key is not cached at all (Cacheable skips it)",
			setup: func(t *testing.T, d *db.DB) string {
				t.Helper()
				orgID := createOrg(t, d, "OrgSARole3", "org-sarole-deleted", db.CreateOrgParams{})
				userID := createUser(t, d, "sarole-deleted@example.com", false)
				createMembership(t, d, orgID, userID, RoleOrgAdmin)
				teamID := createTeam(t, d, orgID, "SARoleDeletedTeam", "sarole-deleted-team", db.CreateTeamParams{})
				saID := createSA(t, d, orgID, ptrStr(teamID), userID)
				pt := insertKey(t, d, db.CreateAPIKeyParams{
					KeyType:          keygen.KeyTypeSA,
					Name:             "sarole-deleted-key",
					OrgID:            orgID,
					ServiceAccountID: ptrStr(saID),
					CreatedBy:        userID,
				})
				// The key itself is not deleted — only the service account it
				// belongs to. The key row therefore still loads from the DB,
				// but auth.Cacheable rejects it before it ever reaches the
				// cache: a sa_key whose service account is missing or
				// soft-deleted must never authenticate again, so it must not
				// be written into the cache under any role — including the
				// least-privileged RoleMember default KeyInfoFromRecord would
				// otherwise assign.
				softDeleteServiceAccount(t, d, saID)
				return pt
			},
			wantAbsent: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := openLoaderDB(t)
			kc := cache.New[string, KeyInfo]()
			log := discardLogger()

			pt := tc.setup(t, d)

			if err := LoadKeysIntoCache(context.Background(), d, kc, log); err != nil {
				t.Fatalf("LoadKeysIntoCache() error = %v", err)
			}

			ki, ok := kc.Get(keygen.Hash(pt, loaderHMACSecret))

			if tc.wantAbsent {
				if ok {
					t.Errorf("sa_key found in cache with Role = %q, want absent (soft-deleted service account must never be cached)", ki.Role)
				}
				return
			}

			if !ok {
				t.Fatal("sa_key not found in cache after LoadKeysIntoCache()")
			}
			if ki.Role != tc.wantRole {
				t.Errorf("Role = %q, want %q", ki.Role, tc.wantRole)
			}
		})
	}
}

// ptrStr is a convenience helper returning a pointer to s.
func ptrStr(s string) *string { return &s }

// ptrInt64 is a convenience helper returning a pointer to v.
func ptrInt64(v int64) *int64 { return &v }

// ptrInt is a convenience helper returning a pointer to v.
func ptrInt(v int) *int { return &v }

// mustFindMembershipID looks up the org membership ID for a given org/user
// pair. CreateOrgMembership does not accept limit fields directly — tests
// that need non-zero user limits must create the membership first and then
// call UpdateOrgMembership using the ID this helper resolves.
func mustFindMembershipID(t *testing.T, d *db.DB, orgID, userID string) string {
	t.Helper()
	memberships, err := d.ListOrgMemberships(context.Background(), orgID, "", 1000)
	if err != nil {
		t.Fatalf("mustFindMembershipID: ListOrgMemberships: %v", err)
	}
	for _, m := range memberships {
		if m.UserID == userID {
			return m.ID
		}
	}
	t.Fatalf("mustFindMembershipID: no membership found for org=%s user=%s", orgID, userID)
	return ""
}
