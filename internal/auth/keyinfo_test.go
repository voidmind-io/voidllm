package auth

import (
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// TestKeyInfoFromRecord_RoleResolution is the role-resolution table for
// KeyInfoFromRecord: every combination of key type, system-admin flag, and
// membership presence that determines the cached Role and the returned "ok"
// (confidently resolved) flag.
func TestKeyInfoFromRecord_RoleResolution(t *testing.T) {
	t.Parallel()

	teamID := "team-1"

	tests := []struct {
		name     string
		record   db.KeyRecord
		wantRole string
		wantOK   bool
	}{
		{
			name:     "user_key with system admin flag resolves system_admin",
			record:   db.KeyRecord{KeyType: keygen.KeyTypeUser, IsSystemAdmin: 1, MembershipRole: RoleMember},
			wantRole: RoleSystemAdmin,
			wantOK:   true,
		},
		{
			name:     "session_key with system admin flag resolves system_admin",
			record:   db.KeyRecord{KeyType: keygen.KeyTypeSession, IsSystemAdmin: 1, MembershipRole: RoleMember},
			wantRole: RoleSystemAdmin,
			wantOK:   true,
		},
		{
			name:     "user_key with org_admin membership resolves org_admin",
			record:   db.KeyRecord{KeyType: keygen.KeyTypeUser, IsSystemAdmin: 0, MembershipRole: RoleOrgAdmin},
			wantRole: RoleOrgAdmin,
			wantOK:   true,
		},
		{
			name:     "user_key with member membership resolves member",
			record:   db.KeyRecord{KeyType: keygen.KeyTypeUser, IsSystemAdmin: 0, MembershipRole: RoleMember},
			wantRole: RoleMember,
			wantOK:   true,
		},
		{
			name:     "session_key with org_admin membership resolves org_admin",
			record:   db.KeyRecord{KeyType: keygen.KeyTypeSession, IsSystemAdmin: 0, MembershipRole: RoleOrgAdmin},
			wantRole: RoleOrgAdmin,
			wantOK:   true,
		},
		{
			name:     "user_key with no membership and not system admin defaults to member, ok=false",
			record:   db.KeyRecord{KeyType: keygen.KeyTypeUser, IsSystemAdmin: 0, MembershipRole: ""},
			wantRole: RoleMember,
			wantOK:   false,
		},
		{
			name:     "session_key with no membership and not system admin defaults to member, ok=false",
			record:   db.KeyRecord{KeyType: keygen.KeyTypeSession, IsSystemAdmin: 0, MembershipRole: ""},
			wantRole: RoleMember,
			wantOK:   false,
		},
		{
			name:     "team_key always resolves team_admin",
			record:   db.KeyRecord{KeyType: keygen.KeyTypeTeam, TeamID: &teamID},
			wantRole: RoleTeamAdmin,
			wantOK:   true,
		},
		{
			name:     "team_key resolves team_admin even with no membership row",
			record:   db.KeyRecord{KeyType: keygen.KeyTypeTeam, TeamID: &teamID, MembershipRole: ""},
			wantRole: RoleTeamAdmin,
			wantOK:   true,
		},
		{
			name: "sa_key with active team-bound service account resolves team_admin",
			record: db.KeyRecord{
				KeyType:              keygen.KeyTypeSA,
				ServiceAccountActive: true,
				ServiceAccountTeamID: &teamID,
			},
			wantRole: RoleTeamAdmin,
			wantOK:   true,
		},
		{
			name: "sa_key with active org-scoped service account (no team) resolves org_admin",
			record: db.KeyRecord{
				KeyType:              keygen.KeyTypeSA,
				ServiceAccountActive: true,
				ServiceAccountTeamID: nil,
			},
			wantRole: RoleOrgAdmin,
			wantOK:   true,
		},
		{
			name: "sa_key with inactive (missing or soft-deleted) service account resolves member, ok=false",
			record: db.KeyRecord{
				KeyType:              keygen.KeyTypeSA,
				ServiceAccountActive: false,
				ServiceAccountTeamID: &teamID,
			},
			wantRole: RoleMember,
			wantOK:   false,
		},
		{
			name: "sa_key with inactive org-scoped service account also resolves member, ok=false",
			record: db.KeyRecord{
				KeyType:              keygen.KeyTypeSA,
				ServiceAccountActive: false,
				ServiceAccountTeamID: nil,
			},
			wantRole: RoleMember,
			wantOK:   false,
		},
		{
			// k.team_id (record.TeamID) must never influence sa_key role
			// resolution — only the owning service account's own team_id does.
			// Here k.team_id is set (as it never is via the real API, but the
			// resolver must not be fooled by it either way) and the role still
			// comes from ServiceAccountTeamID.
			name: "sa_key role is unaffected by k.team_id being set alongside an active team-bound service account",
			record: db.KeyRecord{
				KeyType:              keygen.KeyTypeSA,
				TeamID:               &teamID,
				ServiceAccountActive: true,
				ServiceAccountTeamID: &teamID,
			},
			wantRole: RoleTeamAdmin,
			wantOK:   true,
		},
		{
			// Same as above but k.team_id is unset (the realistic case, since
			// the API never sets team_id on sa_key rows) — same result.
			name: "sa_key role is unaffected by k.team_id being unset alongside an active team-bound service account",
			record: db.KeyRecord{
				KeyType:              keygen.KeyTypeSA,
				TeamID:               nil,
				ServiceAccountActive: true,
				ServiceAccountTeamID: &teamID,
			},
			wantRole: RoleTeamAdmin,
			wantOK:   true,
		},
		{
			name: "sa_key with active service account whose team_id is an empty string resolves org_admin",
			record: db.KeyRecord{
				KeyType:              keygen.KeyTypeSA,
				ServiceAccountActive: true,
				ServiceAccountTeamID: ptrEmptyString(),
			},
			wantRole: RoleOrgAdmin,
			wantOK:   true,
		},
		{
			name:     "unknown key type defaults to member, ok=false",
			record:   db.KeyRecord{KeyType: "bogus_key_type"},
			wantRole: RoleMember,
			wantOK:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ki, ok := KeyInfoFromRecord(tc.record)

			if ki.Role != tc.wantRole {
				t.Errorf("Role = %q, want %q", ki.Role, tc.wantRole)
			}
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
		})
	}
}

// TestKeyInfoFromRecord_FieldMapping verifies that every limit field, the
// nullable ID fields, and ExpiresAt are copied faithfully from KeyRecord to
// KeyInfo, across all four limit tiers (key, org, team, user).
func TestKeyInfoFromRecord_FieldMapping(t *testing.T) {
	t.Parallel()

	teamID := "team-42"
	userID := "user-42"
	saID := "sa-42"
	expiresAt := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

	record := db.KeyRecord{
		ID:               "key-42",
		KeyHash:          "hash-42",
		KeyType:          keygen.KeyTypeUser,
		Name:             "test key",
		OrgID:            "org-42",
		TeamID:           &teamID,
		UserID:           &userID,
		ServiceAccountID: &saID,

		DailyTokenLimit:   111,
		MonthlyTokenLimit: 222,
		RequestsPerMinute: 3,
		RequestsPerDay:    4,

		ExpiresAt: &expiresAt,

		OrgDailyTokenLimit:   1_111,
		OrgMonthlyTokenLimit: 2_222,
		OrgRequestsPerMinute: 33,
		OrgRequestsPerDay:    44,

		TeamDailyTokenLimit:   11_111,
		TeamMonthlyTokenLimit: 22_222,
		TeamRequestsPerMinute: 333,
		TeamRequestsPerDay:    444,

		UserDailyTokenLimit:   111_111,
		UserMonthlyTokenLimit: 222_222,
		UserRequestsPerMinute: 3_333,
		UserRequestsPerDay:    4_444,

		IsSystemAdmin:  0,
		MembershipRole: RoleOrgAdmin,
	}

	ki, ok := KeyInfoFromRecord(record)
	if !ok {
		t.Fatal("KeyInfoFromRecord() ok = false, want true")
	}

	if ki.ID != record.ID {
		t.Errorf("ID = %q, want %q", ki.ID, record.ID)
	}
	if ki.KeyType != record.KeyType {
		t.Errorf("KeyType = %q, want %q", ki.KeyType, record.KeyType)
	}
	if ki.Name != record.Name {
		t.Errorf("Name = %q, want %q", ki.Name, record.Name)
	}
	if ki.OrgID != record.OrgID {
		t.Errorf("OrgID = %q, want %q", ki.OrgID, record.OrgID)
	}
	if ki.TeamID != teamID {
		t.Errorf("TeamID = %q, want %q", ki.TeamID, teamID)
	}
	if ki.UserID != userID {
		t.Errorf("UserID = %q, want %q", ki.UserID, userID)
	}
	if ki.ServiceAccountID != saID {
		t.Errorf("ServiceAccountID = %q, want %q", ki.ServiceAccountID, saID)
	}
	if ki.ExpiresAt == nil || !ki.ExpiresAt.Equal(expiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", ki.ExpiresAt, expiresAt)
	}

	// Key-level.
	if ki.DailyTokenLimit != 111 || ki.MonthlyTokenLimit != 222 || ki.RequestsPerMinute != 3 || ki.RequestsPerDay != 4 {
		t.Errorf("key-level limits = %+v, want {111 222 3 4}", []int64{ki.DailyTokenLimit, ki.MonthlyTokenLimit, int64(ki.RequestsPerMinute), int64(ki.RequestsPerDay)})
	}
	// Org-level.
	if ki.OrgDailyTokenLimit != 1_111 || ki.OrgMonthlyTokenLimit != 2_222 || ki.OrgRequestsPerMinute != 33 || ki.OrgRequestsPerDay != 44 {
		t.Errorf("org-level limits not mapped correctly: %+v", ki)
	}
	// Team-level.
	if ki.TeamDailyTokenLimit != 11_111 || ki.TeamMonthlyTokenLimit != 22_222 || ki.TeamRequestsPerMinute != 333 || ki.TeamRequestsPerDay != 444 {
		t.Errorf("team-level limits not mapped correctly: %+v", ki)
	}
	// User-level (new in this change).
	if ki.UserDailyTokenLimit != 111_111 || ki.UserMonthlyTokenLimit != 222_222 || ki.UserRequestsPerMinute != 3_333 || ki.UserRequestsPerDay != 4_444 {
		t.Errorf("user-level limits not mapped correctly: %+v", ki)
	}
}

// TestKeyInfoFromRecord_NilIDsMapToEmptyString verifies that a KeyRecord with
// nil TeamID/UserID/ServiceAccountID (e.g. an org-scoped sa_key) maps to empty
// strings rather than panicking or leaving stale values.
func TestKeyInfoFromRecord_NilIDsMapToEmptyString(t *testing.T) {
	t.Parallel()

	record := db.KeyRecord{
		ID:      "sa-key-1",
		KeyType: keygen.KeyTypeSA,
		OrgID:   "org-1",
		// TeamID, UserID, ServiceAccountID all nil.
		// ServiceAccountActive must be true (as it would be for any real,
		// non-soft-deleted service account) so this org-scoped sa_key resolves
		// org_admin confidently rather than falling back to member, ok=false.
		ServiceAccountActive: true,
	}

	ki, ok := KeyInfoFromRecord(record)
	if !ok {
		t.Fatal("KeyInfoFromRecord() ok = false, want true (sa_key without team resolves org_admin confidently)")
	}
	if ki.TeamID != "" {
		t.Errorf("TeamID = %q, want empty", ki.TeamID)
	}
	if ki.UserID != "" {
		t.Errorf("UserID = %q, want empty", ki.UserID)
	}
	if ki.ServiceAccountID != "" {
		t.Errorf("ServiceAccountID = %q, want empty", ki.ServiceAccountID)
	}
}

// ptrEmptyString returns a pointer to an empty string, for exercising the
// (unrealistic in practice, but defensively handled) case of a
// KeyRecord.ServiceAccountTeamID that is non-nil but empty.
func ptrEmptyString() *string {
	s := ""
	return &s
}

// TestCacheable is the table for Cacheable: team keys are always cacheable
// regardless of ServiceAccountActive, UserActive, or MembershipExists (all
// meaningless for that key type); a user_key or session_key is cacheable
// only while its owning user is active and either has a surviving org
// membership row or is a system admin; a sa_key is cacheable only while its
// owning service account is active, i.e. not missing and not soft-deleted.
func TestCacheable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rec  db.KeyRecord
		want bool
	}{
		{
			name: "active user with membership is cacheable",
			rec:  db.KeyRecord{KeyType: keygen.KeyTypeUser, UserActive: true, MembershipExists: true, IsSystemAdmin: 0},
			want: true,
		},
		{
			name: "active session key with membership is cacheable",
			rec:  db.KeyRecord{KeyType: keygen.KeyTypeSession, UserActive: true, MembershipExists: true, IsSystemAdmin: 0},
			want: true,
		},
		{
			name: "active user, no membership, not system admin is not cacheable",
			rec:  db.KeyRecord{KeyType: keygen.KeyTypeUser, UserActive: true, MembershipExists: false, IsSystemAdmin: 0},
			want: false,
		},
		{
			name: "active session key, no membership, not system admin is not cacheable",
			rec:  db.KeyRecord{KeyType: keygen.KeyTypeSession, UserActive: true, MembershipExists: false, IsSystemAdmin: 0},
			want: false,
		},
		{
			name: "active system admin without membership is cacheable",
			rec:  db.KeyRecord{KeyType: keygen.KeyTypeUser, UserActive: true, MembershipExists: false, IsSystemAdmin: 1},
			want: true,
		},
		{
			name: "active system admin session key without membership is cacheable",
			rec:  db.KeyRecord{KeyType: keygen.KeyTypeSession, UserActive: true, MembershipExists: false, IsSystemAdmin: 1},
			want: true,
		},
		{
			name: "deleted user is not cacheable even with a surviving membership row",
			rec:  db.KeyRecord{KeyType: keygen.KeyTypeUser, UserActive: false, MembershipExists: true, IsSystemAdmin: 0},
			want: false,
		},
		{
			name: "deleted former system admin is not cacheable",
			rec:  db.KeyRecord{KeyType: keygen.KeyTypeUser, UserActive: false, MembershipExists: false, IsSystemAdmin: 1},
			want: false,
		},
		{
			name: "deleted session key former system admin is not cacheable",
			rec:  db.KeyRecord{KeyType: keygen.KeyTypeSession, UserActive: false, MembershipExists: true, IsSystemAdmin: 1},
			want: false,
		},
		{
			name: "team_key is always cacheable regardless of user/membership/service account state",
			rec:  db.KeyRecord{KeyType: keygen.KeyTypeTeam, ServiceAccountActive: false, UserActive: false, MembershipExists: false, IsSystemAdmin: 0},
			want: true,
		},
		{
			name: "sa_key with active service account is cacheable",
			rec:  db.KeyRecord{KeyType: keygen.KeyTypeSA, ServiceAccountActive: true},
			want: true,
		},
		{
			name: "sa_key with inactive (missing or soft-deleted) service account is not cacheable",
			rec:  db.KeyRecord{KeyType: keygen.KeyTypeSA, ServiceAccountActive: false},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := Cacheable(tc.rec); got != tc.want {
				t.Errorf("Cacheable(%+v) = %v, want %v", tc.rec, got, tc.want)
			}
		})
	}
}
