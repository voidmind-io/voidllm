package db

import (
	"context"
	"sort"
	"testing"

	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// ---- ListActiveKeyHashesByUser ------------------------------------------------

// TestListActiveKeyHashesByUser verifies that the hashes returned cover both
// user_key and session_key rows owned by the target user across every
// organization, and exclude soft-deleted keys and keys owned by other users.
func TestListActiveKeyHashesByUser(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	orgA := mustCreateOrg(t, d, CreateOrgParams{Name: "OA", Slug: "hash-by-user-org-a"})
	orgB := mustCreateOrg(t, d, CreateOrgParams{Name: "OB", Slug: "hash-by-user-org-b"})
	teamA := mustCreateTeam(t, d, CreateTeamParams{OrgID: orgA.ID, Name: "TA", Slug: "t-hash-by-user-a"})

	user := mustCreateUser(t, d, CreateUserParams{Email: "hash-by-user@example.com", DisplayName: "U"})
	otherUser := mustCreateUser(t, d, CreateUserParams{Email: "hash-by-user-other@example.com", DisplayName: "U2"})

	mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: orgA.ID, UserID: user.ID, Role: "member"})
	mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: orgB.ID, UserID: user.ID, Role: "member"})
	mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: orgA.ID, UserID: otherUser.ID, Role: "member"})

	userKeyOrgA := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash("vl_uk_hashbyuserorga0000000000000000000000000000000", testHMACSecret),
		KeyHint: "hint1", KeyType: keygen.KeyTypeUser, Name: "user-key-org-a",
		OrgID: orgA.ID, UserID: ptr(user.ID), TeamID: ptr(teamA.ID), CreatedBy: user.ID,
	})
	userKeyOrgB := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash("vl_uk_hashbyuserorgb0000000000000000000000000000000", testHMACSecret),
		KeyHint: "hint2", KeyType: keygen.KeyTypeUser, Name: "user-key-org-b",
		OrgID: orgB.ID, UserID: ptr(user.ID), CreatedBy: user.ID,
	})
	sessionKey := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash("vl_sk_hashbyusersession0000000000000000000000000000", testHMACSecret),
		KeyHint: "hint3", KeyType: keygen.KeyTypeSession, Name: "session-key",
		OrgID: orgA.ID, UserID: ptr(user.ID), CreatedBy: user.ID,
	})
	deletedKey := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash("vl_uk_hashbyuserdeleted0000000000000000000000000000", testHMACSecret),
		KeyHint: "hint4", KeyType: keygen.KeyTypeUser, Name: "deleted-key",
		OrgID: orgA.ID, UserID: ptr(user.ID), TeamID: ptr(teamA.ID), CreatedBy: user.ID,
	})
	if err := d.DeleteAPIKey(ctx, deletedKey.ID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	otherUserKey := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash("vl_uk_hashbyuserother0000000000000000000000000000000", testHMACSecret),
		KeyHint: "hint5", KeyType: keygen.KeyTypeUser, Name: "other-user-key",
		OrgID: orgA.ID, UserID: ptr(otherUser.ID), TeamID: ptr(teamA.ID), CreatedBy: otherUser.ID,
	})

	got, err := d.ListActiveKeyHashesByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListActiveKeyHashesByUser() error = %v", err)
	}

	want := []string{userKeyOrgA.KeyHash, userKeyOrgB.KeyHash, sessionKey.KeyHash}
	assertHashSetEqual(t, got, want)

	for _, h := range got {
		if h == deletedKey.KeyHash {
			t.Error("ListActiveKeyHashesByUser() included a soft-deleted key hash")
		}
		if h == otherUserKey.KeyHash {
			t.Error("ListActiveKeyHashesByUser() included another user's key hash")
		}
	}
}

// TestListActiveKeyHashesByUser_NoKeys verifies that a user with no keys at
// all returns an empty (nil) result without error.
func TestListActiveKeyHashesByUser_NoKeys(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	user := mustCreateUser(t, d, CreateUserParams{Email: "hash-by-user-none@example.com", DisplayName: "U"})

	got, err := d.ListActiveKeyHashesByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListActiveKeyHashesByUser() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListActiveKeyHashesByUser() = %v, want empty", got)
	}
}

// ---- ListActiveKeyHashesByUserInOrg -------------------------------------------

// TestListActiveKeyHashesByUserInOrg verifies that only the target user's
// non-deleted keys scoped to the target org are returned, including session
// keys, and that keys the same user holds in a different org are excluded.
func TestListActiveKeyHashesByUserInOrg(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	orgA := mustCreateOrg(t, d, CreateOrgParams{Name: "OA", Slug: "hash-by-user-org-scoped-a"})
	orgB := mustCreateOrg(t, d, CreateOrgParams{Name: "OB", Slug: "hash-by-user-org-scoped-b"})
	teamA := mustCreateTeam(t, d, CreateTeamParams{OrgID: orgA.ID, Name: "TA", Slug: "t-hash-by-user-scoped-a"})

	user := mustCreateUser(t, d, CreateUserParams{Email: "hash-by-user-scoped@example.com", DisplayName: "U"})
	otherUser := mustCreateUser(t, d, CreateUserParams{Email: "hash-by-user-scoped-other@example.com", DisplayName: "U2"})

	mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: orgA.ID, UserID: user.ID, Role: "member"})
	mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: orgB.ID, UserID: user.ID, Role: "member"})
	mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: orgA.ID, UserID: otherUser.ID, Role: "member"})

	userKeyOrgA := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash("vl_uk_hashscopedorga00000000000000000000000000000000", testHMACSecret),
		KeyHint: "hint1", KeyType: keygen.KeyTypeUser, Name: "user-key-org-a",
		OrgID: orgA.ID, UserID: ptr(user.ID), TeamID: ptr(teamA.ID), CreatedBy: user.ID,
	})
	sessionKeyOrgA := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash("vl_sk_hashscopedsession0000000000000000000000000000", testHMACSecret),
		KeyHint: "hint2", KeyType: keygen.KeyTypeSession, Name: "session-key-org-a",
		OrgID: orgA.ID, UserID: ptr(user.ID), CreatedBy: user.ID,
	})
	userKeyOrgB := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash("vl_uk_hashscopedorgb00000000000000000000000000000000", testHMACSecret),
		KeyHint: "hint3", KeyType: keygen.KeyTypeUser, Name: "user-key-org-b",
		OrgID: orgB.ID, UserID: ptr(user.ID), CreatedBy: user.ID,
	})
	deletedKeyOrgA := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash("vl_uk_hashscopeddeleted0000000000000000000000000000", testHMACSecret),
		KeyHint: "hint4", KeyType: keygen.KeyTypeUser, Name: "deleted-key-org-a",
		OrgID: orgA.ID, UserID: ptr(user.ID), TeamID: ptr(teamA.ID), CreatedBy: user.ID,
	})
	if err := d.DeleteAPIKey(ctx, deletedKeyOrgA.ID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	otherUserKeyOrgA := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash("vl_uk_hashscopedother00000000000000000000000000000000", testHMACSecret),
		KeyHint: "hint5", KeyType: keygen.KeyTypeUser, Name: "other-user-key-org-a",
		OrgID: orgA.ID, UserID: ptr(otherUser.ID), TeamID: ptr(teamA.ID), CreatedBy: otherUser.ID,
	})

	got, err := d.ListActiveKeyHashesByUserInOrg(ctx, user.ID, orgA.ID)
	if err != nil {
		t.Fatalf("ListActiveKeyHashesByUserInOrg() error = %v", err)
	}

	want := []string{userKeyOrgA.KeyHash, sessionKeyOrgA.KeyHash}
	assertHashSetEqual(t, got, want)

	for _, h := range got {
		if h == userKeyOrgB.KeyHash {
			t.Error("ListActiveKeyHashesByUserInOrg() included a key scoped to a different org")
		}
		if h == deletedKeyOrgA.KeyHash {
			t.Error("ListActiveKeyHashesByUserInOrg() included a soft-deleted key hash")
		}
		if h == otherUserKeyOrgA.KeyHash {
			t.Error("ListActiveKeyHashesByUserInOrg() included another user's key hash")
		}
	}
}

// ---- ListActiveKeyHashesByServiceAccount ---------------------------------------

// TestListActiveKeyHashesByServiceAccount verifies that only the target
// service account's non-deleted keys are returned, excluding another service
// account's keys, and that the hashes are still returned after the owning
// service account itself has been soft-deleted (since DeleteServiceAccount
// never touches the owning api_keys rows).
func TestListActiveKeyHashesByServiceAccount(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "hash-by-sa-org"})
	user := mustCreateUser(t, d, CreateUserParams{Email: "hash-by-sa@example.com", DisplayName: "U"})

	sa := mustCreateServiceAccount(t, d, CreateServiceAccountParams{Name: "SA", OrgID: org.ID, CreatedBy: user.ID})
	otherSA := mustCreateServiceAccount(t, d, CreateServiceAccountParams{Name: "SA2", OrgID: org.ID, CreatedBy: user.ID})

	saKey1 := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash("vl_sa_hashbysakey1000000000000000000000000000000000", testHMACSecret),
		KeyHint: "hint1", KeyType: keygen.KeyTypeSA, Name: "sa-key-1",
		OrgID: org.ID, ServiceAccountID: ptr(sa.ID), CreatedBy: user.ID,
	})
	saKey2 := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash("vl_sa_hashbysakey2000000000000000000000000000000000", testHMACSecret),
		KeyHint: "hint2", KeyType: keygen.KeyTypeSA, Name: "sa-key-2",
		OrgID: org.ID, ServiceAccountID: ptr(sa.ID), CreatedBy: user.ID,
	})
	deletedSAKey := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash("vl_sa_hashbysadeleted00000000000000000000000000000000", testHMACSecret),
		KeyHint: "hint3", KeyType: keygen.KeyTypeSA, Name: "sa-key-deleted",
		OrgID: org.ID, ServiceAccountID: ptr(sa.ID), CreatedBy: user.ID,
	})
	if err := d.DeleteAPIKey(ctx, deletedSAKey.ID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	otherSAKey := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash("vl_sa_hashbyotherSAkey0000000000000000000000000000000", testHMACSecret),
		KeyHint: "hint4", KeyType: keygen.KeyTypeSA, Name: "other-sa-key",
		OrgID: org.ID, ServiceAccountID: ptr(otherSA.ID), CreatedBy: user.ID,
	})

	got, err := d.ListActiveKeyHashesByServiceAccount(ctx, sa.ID)
	if err != nil {
		t.Fatalf("ListActiveKeyHashesByServiceAccount() error = %v", err)
	}
	want := []string{saKey1.KeyHash, saKey2.KeyHash}
	assertHashSetEqual(t, got, want)
	for _, h := range got {
		if h == deletedSAKey.KeyHash {
			t.Error("ListActiveKeyHashesByServiceAccount() included a soft-deleted key hash")
		}
		if h == otherSAKey.KeyHash {
			t.Error("ListActiveKeyHashesByServiceAccount() included another service account's key hash")
		}
	}

	// Soft-deleting the owning service account must not remove its api_keys
	// rows, so the same hashes are still returned afterward.
	if err := d.DeleteServiceAccount(ctx, sa.ID); err != nil {
		t.Fatalf("DeleteServiceAccount: %v", err)
	}
	gotAfterDelete, err := d.ListActiveKeyHashesByServiceAccount(ctx, sa.ID)
	if err != nil {
		t.Fatalf("ListActiveKeyHashesByServiceAccount() after SA delete: error = %v", err)
	}
	assertHashSetEqual(t, gotAfterDelete, want)
}

// assertHashSetEqual fails the test if got and want do not contain exactly
// the same set of strings, regardless of order.
func assertHashSetEqual(t *testing.T, got, want []string) {
	t.Helper()
	gotSorted := append([]string(nil), got...)
	wantSorted := append([]string(nil), want...)
	sort.Strings(gotSorted)
	sort.Strings(wantSorted)

	if len(gotSorted) != len(wantSorted) {
		t.Fatalf("got %d hashes %v, want %d hashes %v", len(gotSorted), gotSorted, len(wantSorted), wantSorted)
	}
	for i := range gotSorted {
		if gotSorted[i] != wantSorted[i] {
			t.Fatalf("got %v, want %v", gotSorted, wantSorted)
		}
	}
}

// ---- UserActive / MembershipExists columns ------------------------------------

// TestLoadActiveKey_UserActiveAndMembershipExists verifies the KeyRecord
// columns that back auth.Cacheable: UserActive reflects whether the owning
// user row exists and is not soft-deleted, and MembershipExists reflects
// whether the owning user has a surviving org_memberships row scoped to the
// key's own org. Both are meaningless (false) for keys with no owning user.
func TestLoadActiveKey_UserActiveAndMembershipExists(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, d *DB) string // returns key ID
		check func(t *testing.T, got *KeyRecord)
	}{
		{
			name: "active user with membership: UserActive true, MembershipExists true",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "useractive-with-membership"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "useractive-membership@example.com", DisplayName: "U"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-useractive-membership"})
				mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: user.ID, Role: "member"})
				k := mustCreateAPIKey(t, d, testKeyParams(org.ID, user.ID, team.ID, user.ID))
				return k.ID
			},
			check: func(t *testing.T, got *KeyRecord) {
				t.Helper()
				if !got.UserActive {
					t.Error("UserActive = false, want true")
				}
				if !got.MembershipExists {
					t.Error("MembershipExists = false, want true")
				}
			},
		},
		{
			name: "active user without any membership row: UserActive true, MembershipExists false",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "useractive-no-membership"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "useractive-no-membership@example.com", DisplayName: "U"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-useractive-no-membership"})
				// Deliberately no CreateOrgMembership call.
				k := mustCreateAPIKey(t, d, testKeyParams(org.ID, user.ID, team.ID, user.ID))
				return k.ID
			},
			check: func(t *testing.T, got *KeyRecord) {
				t.Helper()
				if !got.UserActive {
					t.Error("UserActive = false, want true")
				}
				if got.MembershipExists {
					t.Error("MembershipExists = true, want false (no membership row created)")
				}
			},
		},
		{
			name: "membership exists in a different org than the key: MembershipExists false",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "useractive-other-org-membership"})
				otherOrg := mustCreateOrg(t, d, CreateOrgParams{Name: "OO", Slug: "useractive-other-org-membership-2"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "useractive-other-org@example.com", DisplayName: "U"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-useractive-other-org"})
				mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: otherOrg.ID, UserID: user.ID, Role: "member"})
				k := mustCreateAPIKey(t, d, testKeyParams(org.ID, user.ID, team.ID, user.ID))
				return k.ID
			},
			check: func(t *testing.T, got *KeyRecord) {
				t.Helper()
				if !got.UserActive {
					t.Error("UserActive = false, want true")
				}
				if got.MembershipExists {
					t.Error("MembershipExists = true, want false (membership is scoped to a different org)")
				}
			},
		},
		{
			name: "soft-deleted user: UserActive false even with a surviving membership row",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "useractive-deleted-user"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "useractive-deleted@example.com", DisplayName: "U"})
				team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-useractive-deleted"})
				mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: user.ID, Role: "member"})
				k := mustCreateAPIKey(t, d, testKeyParams(org.ID, user.ID, team.ID, user.ID))
				if err := d.DeleteUser(context.Background(), user.ID); err != nil {
					t.Fatalf("DeleteUser: %v", err)
				}
				return k.ID
			},
			check: func(t *testing.T, got *KeyRecord) {
				t.Helper()
				if got.UserActive {
					t.Error("UserActive = true, want false for a soft-deleted user")
				}
			},
		},
		{
			name: "sa_key has no owning user: UserActive false, MembershipExists false",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "useractive-sakey"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "useractive-sakey@example.com", DisplayName: "U"})
				sa := mustCreateServiceAccount(t, d, CreateServiceAccountParams{Name: "SA", OrgID: org.ID, CreatedBy: user.ID})
				plain := "vl_sa_useractivesakey0000000000000000000000000000000"
				k := mustCreateAPIKey(t, d, CreateAPIKeyParams{
					KeyHash: keygen.Hash(plain, testHMACSecret), KeyHint: keygen.Hint(plain),
					KeyType: keygen.KeyTypeSA, Name: "sa-key",
					OrgID: org.ID, ServiceAccountID: ptr(sa.ID), CreatedBy: user.ID,
				})
				return k.ID
			},
			check: func(t *testing.T, got *KeyRecord) {
				t.Helper()
				if got.UserActive {
					t.Error("UserActive = true, want false for a sa_key with no owning user")
				}
				if got.MembershipExists {
					t.Error("MembershipExists = true, want false for a sa_key with no owning user")
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

// TestLoadAllActiveKeys_UserActiveAndMembershipExists mirrors
// TestLoadActiveKey_UserActiveAndMembershipExists for the bulk-load path,
// verifying UserActive and MembershipExists are populated consistently
// across multiple rows in a single query.
func TestLoadAllActiveKeys_UserActiveAndMembershipExists(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "loadall-useractive-org"})
	team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "t-loadall-useractive"})

	activeUser := mustCreateUser(t, d, CreateUserParams{Email: "loadall-useractive-active@example.com", DisplayName: "U"})
	mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: activeUser.ID, Role: "member"})
	activeKey := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash("vl_uk_loadallactiveuser000000000000000000000000000000", testHMACSecret),
		KeyHint: "hint1", KeyType: keygen.KeyTypeUser, Name: "active-user-key",
		OrgID: org.ID, UserID: ptr(activeUser.ID), TeamID: ptr(team.ID), CreatedBy: activeUser.ID,
	})

	noMembershipUser := mustCreateUser(t, d, CreateUserParams{Email: "loadall-useractive-nomembership@example.com", DisplayName: "U2"})
	noMembershipKey := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash("vl_uk_loadallnomembership0000000000000000000000000000", testHMACSecret),
		KeyHint: "hint2", KeyType: keygen.KeyTypeUser, Name: "no-membership-key",
		OrgID: org.ID, UserID: ptr(noMembershipUser.ID), TeamID: ptr(team.ID), CreatedBy: noMembershipUser.ID,
	})

	deletedUser := mustCreateUser(t, d, CreateUserParams{Email: "loadall-useractive-deleted@example.com", DisplayName: "U3"})
	mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: deletedUser.ID, Role: "member"})
	deletedUserKey := mustCreateAPIKey(t, d, CreateAPIKeyParams{
		KeyHash: keygen.Hash("vl_uk_loadalldeleteduser00000000000000000000000000000", testHMACSecret),
		KeyHint: "hint3", KeyType: keygen.KeyTypeUser, Name: "deleted-user-key",
		OrgID: org.ID, UserID: ptr(deletedUser.ID), TeamID: ptr(team.ID), CreatedBy: deletedUser.ID,
	})
	if err := d.DeleteUser(ctx, deletedUser.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
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

	ak, ok := byID[activeKey.ID]
	if !ok {
		t.Fatal("active user key not found in LoadAllActiveKeys() results")
	}
	if !ak.UserActive || !ak.MembershipExists {
		t.Errorf("active user key UserActive/MembershipExists = %v/%v, want true/true", ak.UserActive, ak.MembershipExists)
	}

	nmk, ok := byID[noMembershipKey.ID]
	if !ok {
		t.Fatal("no-membership user key not found in LoadAllActiveKeys() results")
	}
	if !nmk.UserActive || nmk.MembershipExists {
		t.Errorf("no-membership user key UserActive/MembershipExists = %v/%v, want true/false", nmk.UserActive, nmk.MembershipExists)
	}

	duk, ok := byID[deletedUserKey.ID]
	if !ok {
		t.Fatal("deleted user's key not found in LoadAllActiveKeys() results (the key itself is not deleted)")
	}
	if duk.UserActive {
		t.Error("deleted user's key UserActive = true, want false")
	}
}
