package app

import (
	"context"
	"testing"

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// lkTestHMACSecret is the fixed HMAC secret used to hash keys created by
// these tests. The plaintext keys are never authenticated through an HTTP
// layer here, so any fixed secret works.
var lkTestHMACSecret = []byte("list-keys-for-caller-test-secret")

// mustCreateOrgLK creates an organization for listKeysForCaller tests.
func mustCreateOrgLK(t *testing.T, database *db.DB, slug string) *db.Org {
	t.Helper()
	org, err := database.CreateOrg(context.Background(), db.CreateOrgParams{
		Name: "Org " + slug,
		Slug: slug,
	})
	if err != nil {
		t.Fatalf("CreateOrg(%q): %v", slug, err)
	}
	return org
}

// mustCreateUserLK creates a user for listKeysForCaller tests.
func mustCreateUserLK(t *testing.T, database *db.DB, email string) *db.User {
	t.Helper()
	user, err := database.CreateUser(context.Background(), db.CreateUserParams{
		Email:        email,
		DisplayName:  "U",
		AuthProvider: "local",
	})
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", email, err)
	}
	return user
}

// mustCreateTeamLK creates a team for listKeysForCaller tests.
func mustCreateTeamLK(t *testing.T, database *db.DB, orgID, slug string) *db.Team {
	t.Helper()
	team, err := database.CreateTeam(context.Background(), db.CreateTeamParams{
		OrgID: orgID,
		Name:  "Team " + slug,
		Slug:  slug,
	})
	if err != nil {
		t.Fatalf("CreateTeam(%q): %v", slug, err)
	}
	return team
}

// mustCreateServiceAccountLK creates a service account for listKeysForCaller tests.
func mustCreateServiceAccountLK(t *testing.T, database *db.DB, orgID string, teamID *string, createdBy string) *db.ServiceAccount {
	t.Helper()
	sa, err := database.CreateServiceAccount(context.Background(), db.CreateServiceAccountParams{
		Name:      "SA",
		OrgID:     orgID,
		TeamID:    teamID,
		CreatedBy: createdBy,
	})
	if err != nil {
		t.Fatalf("CreateServiceAccount: %v", err)
	}
	return sa
}

// mustCreateUserKeyLK creates a user_key row for listKeysForCaller tests,
// with a unique plaintext (and therefore a unique key_hash) per call.
func mustCreateUserKeyLK(t *testing.T, database *db.DB, orgID, userID, name string) *db.APIKey {
	t.Helper()
	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("keygen.Generate: %v", err)
	}
	key, err := database.CreateAPIKey(context.Background(), db.CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, lkTestHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeUser,
		Name:      name,
		OrgID:     orgID,
		UserID:    &userID,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey(%q): %v", name, err)
	}
	return key
}

// mustCreateSessionKeyLK creates a session_key row for listKeysForCaller
// tests, used to verify session keys are never returned regardless of caller
// role.
func mustCreateSessionKeyLK(t *testing.T, database *db.DB, orgID, userID, name string) *db.APIKey {
	t.Helper()
	plaintext, err := keygen.Generate(keygen.KeyTypeSession)
	if err != nil {
		t.Fatalf("keygen.Generate: %v", err)
	}
	key, err := database.CreateAPIKey(context.Background(), db.CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, lkTestHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeSession,
		Name:      name,
		OrgID:     orgID,
		UserID:    &userID,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey(%q): %v", name, err)
	}
	return key
}

// keyIDs collects the "id" field of every entry returned by listKeysForCaller
// into a set for order-independent membership assertions.
func keyIDs(entries []map[string]any) map[string]bool {
	ids := make(map[string]bool, len(entries))
	for _, e := range entries {
		id, _ := e["id"].(string)
		ids[id] = true
	}
	return ids
}

// TestListKeysForCaller_TeamKeyIdentityRejected verifies that a team_key
// identity — role team_admin, no owning user, so UserID is empty — is
// rejected before any DB query narrows or widens the result, rather than
// falling through to an unfiltered listing of every key in the org.
func TestListKeysForCaller_TeamKeyIdentityRejected(t *testing.T) {
	t.Parallel()

	database := openTestDBForSchemaTests(t)
	org := mustCreateOrgLK(t, database, "lk-teamkey-org")
	user := mustCreateUserLK(t, database, "lk-teamkey@example.com")
	mustCreateUserKeyLK(t, database, org.ID, user.ID, "some-key")

	id := mcp.KeyIdentity{OrgID: org.ID, TeamID: "team-1", Role: auth.RoleTeamAdmin}

	got, err := listKeysForCaller(context.Background(), database, org.ID, auth.RoleTeamAdmin, id)
	if err == nil {
		t.Fatal("listKeysForCaller() error = nil, want an error for a team_key identity with no UserID")
	}
	if got != nil {
		t.Errorf("listKeysForCaller() result = %v, want nil on error", got)
	}
}

// TestListKeysForCaller_TeamBoundSAIdentityRejected verifies that a
// team-bound sa_key identity — role team_admin, ServiceAccountID set, no
// owning user — is rejected the same way as a team_key identity: the
// service account itself has no UserID to scope by.
func TestListKeysForCaller_TeamBoundSAIdentityRejected(t *testing.T) {
	t.Parallel()

	database := openTestDBForSchemaTests(t)
	org := mustCreateOrgLK(t, database, "lk-sakey-org")
	user := mustCreateUserLK(t, database, "lk-sakey@example.com")
	team := mustCreateTeamLK(t, database, org.ID, "lk-sakey-team")
	sa := mustCreateServiceAccountLK(t, database, org.ID, &team.ID, user.ID)
	mustCreateUserKeyLK(t, database, org.ID, user.ID, "some-key")

	id := mcp.KeyIdentity{OrgID: org.ID, TeamID: team.ID, Role: auth.RoleTeamAdmin, KeyID: sa.ID}

	got, err := listKeysForCaller(context.Background(), database, org.ID, auth.RoleTeamAdmin, id)
	if err == nil {
		t.Fatal("listKeysForCaller() error = nil, want an error for a team-bound sa_key identity with no UserID")
	}
	if got != nil {
		t.Errorf("listKeysForCaller() result = %v, want nil on error", got)
	}
}

// TestListKeysForCaller_MemberSeesOnlyOwnKeys verifies that a member
// identity is scoped by UserID: their own key is returned, another user's
// key in the same org is not, and session keys are excluded regardless.
func TestListKeysForCaller_MemberSeesOnlyOwnKeys(t *testing.T) {
	t.Parallel()

	database := openTestDBForSchemaTests(t)
	org := mustCreateOrgLK(t, database, "lk-member-org")
	member := mustCreateUserLK(t, database, "lk-member@example.com")
	other := mustCreateUserLK(t, database, "lk-member-other@example.com")

	ownKey := mustCreateUserKeyLK(t, database, org.ID, member.ID, "own-key")
	otherKey := mustCreateUserKeyLK(t, database, org.ID, other.ID, "other-key")
	sessionKey := mustCreateSessionKeyLK(t, database, org.ID, member.ID, "own-session-key")

	id := mcp.KeyIdentity{OrgID: org.ID, UserID: member.ID, Role: auth.RoleMember}

	got, err := listKeysForCaller(context.Background(), database, org.ID, auth.RoleMember, id)
	if err != nil {
		t.Fatalf("listKeysForCaller() error = %v, want nil", err)
	}

	ids := keyIDs(got)
	if !ids[ownKey.ID] {
		t.Errorf("member's own key %q missing from result: %v", ownKey.ID, ids)
	}
	if ids[otherKey.ID] {
		t.Errorf("another user's key %q leaked into member's result: %v", otherKey.ID, ids)
	}
	if ids[sessionKey.ID] {
		t.Errorf("session key %q leaked into result: %v", sessionKey.ID, ids)
	}
	if len(got) != 1 {
		t.Errorf("len(got) = %d, want 1 (only the member's own key)", len(got))
	}
}

// TestListKeysForCaller_OrgAdminAndSystemAdminSeeAllOrgKeys verifies that
// org_admin and system_admin identities are not scoped by UserID: every
// non-session key in the org is returned regardless of owner, and keys
// belonging to a different org are never included.
func TestListKeysForCaller_OrgAdminAndSystemAdminSeeAllOrgKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		role string
	}{
		{name: "org_admin", role: auth.RoleOrgAdmin},
		{name: "system_admin", role: auth.RoleSystemAdmin},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			database := openTestDBForSchemaTests(t)
			org := mustCreateOrgLK(t, database, "lk-admin-org-"+tc.name)
			otherOrg := mustCreateOrgLK(t, database, "lk-admin-otherorg-"+tc.name)
			userA := mustCreateUserLK(t, database, "lk-admin-a-"+tc.name+"@example.com")
			userB := mustCreateUserLK(t, database, "lk-admin-b-"+tc.name+"@example.com")
			caller := mustCreateUserLK(t, database, "lk-admin-caller-"+tc.name+"@example.com")

			keyA := mustCreateUserKeyLK(t, database, org.ID, userA.ID, "key-a")
			keyB := mustCreateUserKeyLK(t, database, org.ID, userB.ID, "key-b")
			sessionKey := mustCreateSessionKeyLK(t, database, org.ID, userA.ID, "session-a")
			otherOrgKey := mustCreateUserKeyLK(t, database, otherOrg.ID, userA.ID, "other-org-key")

			id := mcp.KeyIdentity{OrgID: org.ID, UserID: caller.ID, Role: tc.role}

			got, err := listKeysForCaller(context.Background(), database, org.ID, tc.role, id)
			if err != nil {
				t.Fatalf("listKeysForCaller() error = %v, want nil", err)
			}

			ids := keyIDs(got)
			if !ids[keyA.ID] {
				t.Errorf("userA's key %q missing from result: %v", keyA.ID, ids)
			}
			if !ids[keyB.ID] {
				t.Errorf("userB's key %q missing from result: %v", keyB.ID, ids)
			}
			if ids[sessionKey.ID] {
				t.Errorf("session key %q leaked into result: %v", sessionKey.ID, ids)
			}
			if ids[otherOrgKey.ID] {
				t.Errorf("another org's key %q leaked into result: %v", otherOrgKey.ID, ids)
			}
			if len(got) != 2 {
				t.Errorf("len(got) = %d, want 2 (every non-session key in the org)", len(got))
			}
		})
	}
}
