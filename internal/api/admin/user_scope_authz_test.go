package admin_test

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
	"golang.org/x/crypto/bcrypt"
)

// Action indexes into the expected-status rows of the scope matrix.
const (
	uscActPassword = iota
	uscActEmail
	uscActDisplayName
	uscActGet
	uscActCreateKey
	uscActRotateKey
	uscActCount
)

var uscActNames = [uscActCount]string{
	"patch_password", "patch_email", "patch_display_name", "get_user", "create_user_key", "rotate_user_key",
}

// uscWorld holds one fully populated org layout for a single caller kind.
type uscWorld struct {
	app      *fiber.App
	database *db.DB
	keyCache *cache.Cache[string, auth.KeyInfo]
	orgID    string
	org2ID   string
	// userIDs maps a target label to its user ID.
	userIDs map[string]string
	// keyIDs maps a target label to the ID of a user_key owned by that user.
	keyIDs map[string]string
}

// uscHash returns a low-cost bcrypt hash for setup data.
func uscHash(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	return string(h)
}

// uscUser inserts a local user with a known password and an optional
// system-admin flag.
func uscUser(t *testing.T, database *db.DB, email, password string, sysAdmin bool) *db.User {
	t.Helper()
	hash := uscHash(t, password)
	u, err := database.CreateUser(context.Background(), db.CreateUserParams{
		Email:         email,
		DisplayName:   "Name " + email,
		PasswordHash:  &hash,
		AuthProvider:  "local",
		IsSystemAdmin: sysAdmin,
	})
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", email, err)
	}
	return u
}

// uscMember adds an org membership row.
func uscMember(t *testing.T, database *db.DB, orgID, userID, role string) {
	t.Helper()
	if _, err := database.CreateOrgMembership(context.Background(), db.CreateOrgMembershipParams{
		OrgID: orgID, UserID: userID, Role: role,
	}); err != nil {
		t.Fatalf("CreateOrgMembership: %v", err)
	}
}

// uscReq performs a request and returns the status code and body.
func uscReq(t *testing.T, app *fiber.App, method, url, token string, body any) (int, string) {
	t.Helper()
	var req = httptest.NewRequest(method, url, nil)
	if body != nil {
		req = httptest.NewRequest(method, url, bodyJSON(t, body))
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// countUserKeys counts non-deleted keys owned by userID across both orgs.
func (w *uscWorld) countUserKeys(t *testing.T, userID string) int {
	t.Helper()
	n := 0
	for _, org := range []string{w.orgID, w.org2ID} {
		keys, err := w.database.ListAPIKeys(context.Background(), org, "", "", "", 500, true)
		if err != nil {
			t.Fatalf("ListAPIKeys: %v", err)
		}
		for _, k := range keys {
			if k.UserID != nil && *k.UserID == userID {
				n++
			}
		}
	}
	return n
}

// uscSnapshot is the credential state of a user.
type uscSnapshot struct {
	email, displayName, hash string
}

func (w *uscWorld) snapshot(t *testing.T, userID string) uscSnapshot {
	t.Helper()
	u, err := w.database.GetUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	_, hash, err := w.database.GetUserPasswordHash(context.Background(), u.Email)
	if err != nil {
		t.Fatalf("GetUserPasswordHash: %v", err)
	}
	return uscSnapshot{email: u.Email, displayName: u.DisplayName, hash: hash}
}

// uscPassword is the initial password of every seeded user.
const uscPassword = "Initial-pass-1"

// newUscWorld builds the org layout and the target users, each with one
// user_key. The returned function seeds the caller's own user.
func newUscWorld(t *testing.T, dsn string) (*uscWorld, func(role string) string) {
	t.Helper()
	app, database, keyCache := setupTestApp(t, dsn)
	org := mustCreateOrg(t, database, "Scope Org", "scope-org")
	org2 := mustCreateOrg(t, database, "Scope Org Two", "scope-org-two")
	w := &uscWorld{
		app: app, database: database, keyCache: keyCache, orgID: org.ID, org2ID: org2.ID,
		userIDs: map[string]string{}, keyIDs: map[string]string{},
	}

	member := uscUser(t, database, "t-member@example.com", uscPassword, false)
	uscMember(t, database, org.ID, member.ID, auth.RoleMember)
	peer := uscUser(t, database, "t-orgadmin@example.com", uscPassword, false)
	uscMember(t, database, org.ID, peer.ID, auth.RoleOrgAdmin)
	sysMember := uscUser(t, database, "t-sysadmin-member@example.com", uscPassword, true)
	uscMember(t, database, org.ID, sysMember.ID, auth.RoleOrgAdmin)
	sysNone := uscUser(t, database, "t-sysadmin-none@example.com", uscPassword, true)
	multi := uscUser(t, database, "t-multi@example.com", uscPassword, false)
	uscMember(t, database, org.ID, multi.ID, auth.RoleMember)
	uscMember(t, database, org2.ID, multi.ID, auth.RoleMember)
	other := uscUser(t, database, "t-other@example.com", uscPassword, false)
	uscMember(t, database, org2.ID, other.ID, auth.RoleMember)

	w.userIDs["member"] = member.ID
	w.userIDs["org_admin"] = peer.ID
	w.userIDs["sysadmin_member"] = sysMember.ID
	w.userIDs["sysadmin_nomember"] = sysNone.ID
	w.userIDs["multi_org"] = multi.ID
	w.userIDs["other_org_only"] = other.ID

	for label, id := range w.userIDs {
		keyOrg := org.ID
		if label == "other_org_only" {
			keyOrg = org2.ID
		}
		w.keyIDs[label] = mustCreateAPIKeyDirect(t, database, keyOrg, nil, &id, id, 0).ID
	}

	addSelf := func(role string) string {
		u := uscUser(t, database, "caller-"+role+"@example.com", uscPassword, role == auth.RoleSystemAdmin)
		if role == auth.RoleOrgAdmin || role == auth.RoleMember {
			uscMember(t, database, org.ID, u.ID, role)
		}
		w.userIDs["self"] = u.ID
		w.keyIDs["self"] = mustCreateAPIKeyDirect(t, database, org.ID, nil, &u.ID, u.ID, 0).ID
		return u.ID
	}
	return w, addSelf
}

// runAction performs one action against one target and checks the status and
// the resulting state.
func (w *uscWorld) runAction(t *testing.T, callerName, token, label, targetID string, act, want int) {
	t.Helper()
	ctx := context.Background()
	forcedSelf := callerName == "member_user_key"
	tag := callerName + "-" + label

	switch act {
	case uscActPassword, uscActEmail, uscActDisplayName:
		before := w.snapshot(t, targetID)
		newPass := "Changed-pass-9"
		newEmail := "changed-" + tag + "@example.com"
		newName := "Changed " + tag
		body := map[string]any{}
		switch act {
		case uscActPassword:
			body["password"] = newPass
		case uscActEmail:
			body["email"] = newEmail
		default:
			body["display_name"] = newName
		}
		status, resp := uscReq(t, w.app, "PATCH", "/api/v1/users/"+targetID, token, body)
		if status != want {
			t.Fatalf("status = %d, want %d; body: %s", status, want, resp)
		}
		after := w.snapshot(t, targetID)
		if status != fiber.StatusOK {
			if after != before {
				t.Errorf("user changed after refused update: before=%+v after=%+v", before, after)
			}
			return
		}
		switch act {
		case uscActPassword:
			if after.hash == before.hash || bcrypt.CompareHashAndPassword([]byte(after.hash), []byte(newPass)) != nil {
				t.Errorf("password was not updated")
			}
			if after.email != before.email || after.displayName != before.displayName {
				t.Errorf("unrelated fields changed: before=%+v after=%+v", before, after)
			}
		case uscActEmail:
			if after.email != newEmail || after.hash != before.hash {
				t.Errorf("email/hash after update: %+v (before %+v)", after, before)
			}
		default:
			if after.displayName != newName || after.hash != before.hash || after.email != before.email {
				t.Errorf("display name update state: %+v (before %+v)", after, before)
			}
		}

	case uscActGet:
		status, resp := uscReq(t, w.app, "GET", "/api/v1/users/"+targetID, token, nil)
		if status != want {
			t.Fatalf("status = %d, want %d; body: %s", status, want, resp)
		}
		if status != fiber.StatusOK && strings.Contains(resp, "@example.com") {
			t.Errorf("refused response leaks user data: %s", resp)
		}

	case uscActCreateKey:
		before := w.countUserKeys(t, targetID)
		status, resp := uscReq(t, w.app, "POST", keysURL(w.orgID), token, map[string]any{
			"name": "scope-" + tag, "key_type": keygen.KeyTypeUser, "user_id": targetID,
		})
		if status != want {
			t.Fatalf("status = %d, want %d; body: %s", status, want, resp)
		}
		after := w.countUserKeys(t, targetID)
		wantDelta := 0
		if status == fiber.StatusCreated && (!forcedSelf || label == "self") {
			wantDelta = 1
		}
		if after-before != wantDelta {
			t.Errorf("keys owned by target changed by %d, want %d", after-before, wantDelta)
		}
		if status != fiber.StatusCreated && (strings.Contains(resp, keygen.PrefixUser) || strings.Contains(resp, `"key"`)) {
			t.Errorf("refused response carries key material: %s", resp)
		}

	case uscActRotateKey:
		before := w.countUserKeys(t, targetID)
		keyID := w.keyIDs[label]
		status, resp := uscReq(t, w.app, "POST", rotateKeyURL(w.orgID, keyID), token, nil)
		if status != want {
			t.Fatalf("status = %d, want %d; body: %s", status, want, resp)
		}
		after := w.countUserKeys(t, targetID)
		stored, err := w.database.GetAPIKey(ctx, keyID)
		if err != nil {
			t.Fatalf("GetAPIKey: %v", err)
		}
		if status == fiber.StatusOK {
			if after != before+1 {
				t.Errorf("key rows = %d, want %d", after, before+1)
			}
			return
		}
		if after != before {
			t.Errorf("key rows = %d after refused rotate, want %d", after, before)
		}
		if stored.ExpiresAt != nil {
			t.Errorf("target key expiry changed by refused rotate: %v", *stored.ExpiresAt)
		}
		if strings.Contains(resp, keygen.PrefixUser) || strings.Contains(resp, `"key"`) {
			t.Errorf("refused response carries key material: %s", resp)
		}
	}
}

// uscExpect lists the expected status per action for each target label.
type uscExpect map[string][uscActCount]int

var uscLabels = []string{"self", "member", "org_admin", "sysadmin_member", "sysadmin_nomember", "multi_org", "other_org_only"}

// TestUserScopeAuthzMatrix checks every caller kind against every target
// shape and action.
func TestUserScopeAuthzMatrix(t *testing.T) {
	t.Parallel()

	// A caller may act on single-org members ranked strictly below it; display
	// name changes are also allowed on multi-org members. Rows are ordered:
	// password, email, display_name, get, create key, rotate key.
	orgAdminRows := uscExpect{
		"self":              {200, 200, 200, 200, 201, 200},
		"member":            {200, 200, 200, 200, 201, 200},
		"org_admin":         {403, 403, 403, 200, 403, 403},
		"sysadmin_member":   {403, 403, 403, 200, 403, 403},
		"sysadmin_nomember": {404, 404, 404, 404, 400, 403},
		"multi_org":         {403, 403, 200, 200, 403, 403},
		"other_org_only":    {404, 404, 404, 404, 400, 404},
	}
	// A service-account key has no user, so key creation is rejected up front.
	saRows := uscExpect{}
	for label, row := range orgAdminRows {
		row[uscActCreateKey] = 400
		saRows[label] = row
	}
	// The user routes require org_admin, so lower roles are refused there. A
	// team_key carries no user, which key creation rejects up front. A member
	// creating a key always gets one for itself, never for the named user.
	teamAdminRows := uscExpect{}
	memberRows := uscExpect{}
	sysAdminRows := uscExpect{}
	for _, label := range uscLabels {
		teamAdminRows[label] = [uscActCount]int{403, 403, 403, 403, 400, 403}
		memberRows[label] = [uscActCount]int{403, 403, 403, 403, 201, 403}
		sysAdminRows[label] = [uscActCount]int{200, 200, 200, 200, 201, 200}
	}
	teamAdminRows["other_org_only"] = [uscActCount]int{403, 403, 403, 403, 400, 404}
	memberRows["self"] = [uscActCount]int{403, 403, 403, 403, 201, 200}
	memberRows["other_org_only"] = [uscActCount]int{403, 403, 403, 403, 201, 404}
	// For rotate on other_org_only the 404 comes from the key's org: that user_key
	// belongs to the second org, so it is not found under the caller's org URL.
	sysAdminRows["other_org_only"] = [uscActCount]int{200, 200, 200, 200, 400, 404}

	callers := []struct {
		name     string
		selfRole string // role of the caller's own user; empty when the caller has no user
		expect   uscExpect
		info     func(t *testing.T, w *uscWorld, selfID string) auth.KeyInfo
	}{
		{"org_admin_user_key", auth.RoleOrgAdmin, orgAdminRows,
			func(t *testing.T, w *uscWorld, selfID string) auth.KeyInfo {
				return auth.KeyInfo{KeyType: keygen.KeyTypeUser, Role: auth.RoleOrgAdmin, OrgID: w.orgID, UserID: selfID}
			}},
		{"org_scoped_sa_key", "", saRows,
			func(t *testing.T, w *uscWorld, selfID string) auth.KeyInfo {
				return auth.KeyInfo{KeyType: keygen.KeyTypeSA, Role: auth.RoleOrgAdmin, OrgID: w.orgID, ServiceAccountID: "uscope-sa"}
			}},
		{"team_admin_team_key", "", teamAdminRows,
			func(t *testing.T, w *uscWorld, selfID string) auth.KeyInfo {
				team := mustCreateTeam(t, w.database, w.orgID, "Scope Team", "scope-team")
				return auth.KeyInfo{KeyType: keygen.KeyTypeTeam, Role: auth.RoleTeamAdmin, OrgID: w.orgID, TeamID: team.ID}
			}},
		{"member_user_key", auth.RoleMember, memberRows,
			func(t *testing.T, w *uscWorld, selfID string) auth.KeyInfo {
				return auth.KeyInfo{KeyType: keygen.KeyTypeUser, Role: auth.RoleMember, OrgID: w.orgID, UserID: selfID}
			}},
		{"system_admin_user_key", auth.RoleSystemAdmin, sysAdminRows,
			func(t *testing.T, w *uscWorld, selfID string) auth.KeyInfo {
				return auth.KeyInfo{KeyType: keygen.KeyTypeUser, Role: auth.RoleSystemAdmin, OrgID: w.orgID, UserID: selfID}
			}},
	}

	for _, cc := range callers {
		t.Run(cc.name, func(t *testing.T) {
			t.Parallel()

			w, addSelf := newUscWorld(t, "file:TestUserScopeAuthzMatrix_"+cc.name+"?mode=memory&cache=private")
			selfID := ""
			if cc.selfRole != "" {
				selfID = addSelf(cc.selfRole)
			}
			token := addTestKeyInfo(t, w.keyCache, cc.info(t, w, selfID))

			for _, label := range uscLabels {
				targetID, ok := w.userIDs[label]
				if !ok {
					continue
				}
				want := cc.expect[label]
				for act := 0; act < uscActCount; act++ {
					// Inner subtests must not use t.Parallel: they share the
					// uscWorld state and compare before/after snapshots.
					t.Run(label+"/"+uscActNames[act], func(t *testing.T) {
						w.runAction(t, cc.name, token, label, targetID, act, want[act])
					})
				}
			}
		})
	}
}

// TestUserScopeAuthz_BootstrapShape reproduces the default-org layout: a
// system admin holding an org_admin membership, and an org_admin who may not
// change that account's password or issue keys for it.
func TestUserScopeAuthz_BootstrapShape(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUserScopeAuthz_BootstrapShape?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Default", "default")

	oldPass := "Bootstrap-old-1"
	boot := uscUser(t, database, "bootstrap@example.com", oldPass, true)
	uscMember(t, database, org.ID, boot.ID, auth.RoleOrgAdmin)
	admin := uscUser(t, database, "plain-admin@example.com", "Admin-pass-1234", false)
	uscMember(t, database, org.ID, admin.ID, auth.RoleOrgAdmin)

	token := addTestKeyInfo(t, keyCache, auth.KeyInfo{
		KeyType: keygen.KeyTypeUser, Role: auth.RoleOrgAdmin, OrgID: org.ID, UserID: admin.ID,
	})

	login := func(password string) int {
		status, _ := uscReq(t, app, "POST", "/api/v1/auth/login", "", map[string]any{
			"email": "bootstrap@example.com", "password": password,
		})
		return status
	}
	if got := login(oldPass); got != fiber.StatusOK {
		t.Fatalf("baseline login status = %d, want 200", got)
	}

	newPass := "Changed-by-peer-9"
	status, body := uscReq(t, app, "PATCH", "/api/v1/users/"+boot.ID, token, map[string]any{"password": newPass})
	if status != fiber.StatusForbidden {
		t.Fatalf("password change status = %d, want 403; body: %s", status, body)
	}
	if got := login(newPass); got != fiber.StatusUnauthorized {
		t.Errorf("login with the requested password status = %d, want 401", got)
	}
	if got := login(oldPass); got != fiber.StatusOK {
		t.Errorf("login with the original password status = %d, want 200", got)
	}

	before, err := database.ListAPIKeys(context.Background(), org.ID, "", "", "", 100, true)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	status, body = uscReq(t, app, "POST", keysURL(org.ID), token, map[string]any{
		"name": "for-bootstrap", "key_type": keygen.KeyTypeUser, "user_id": boot.ID,
	})
	if status != fiber.StatusForbidden {
		t.Fatalf("create key status = %d, want 403; body: %s", status, body)
	}
	if strings.Contains(body, keygen.PrefixUser) || strings.Contains(body, `"key"`) {
		t.Errorf("refused response carries key material: %s", body)
	}
	after, err := database.ListAPIKeys(context.Background(), org.ID, "", "", "", 100, true)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("key rows = %d, want %d", len(after), len(before))
	}
}

// TestUserScopeAuthz_SystemAdminKeyWithoutMembership checks that a system
// admin may create a user_key for itself in an org where it has no membership.
func TestUserScopeAuthz_SystemAdminKeyWithoutMembership(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUserScopeAuthz_SysAdminSelfKey?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Elsewhere", "elsewhere")
	sys := uscUser(t, database, "sys-self@example.com", "Sys-pass-12345", true)
	token := addTestKeyInfo(t, keyCache, auth.KeyInfo{
		KeyType: keygen.KeyTypeUser, Role: auth.RoleSystemAdmin, OrgID: org.ID, UserID: sys.ID,
	})

	status, body := uscReq(t, app, "POST", keysURL(org.ID), token, map[string]any{
		"name": "own", "key_type": keygen.KeyTypeUser, "user_id": sys.ID,
	})
	if status != fiber.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", status, body)
	}
	if !strings.Contains(body, fmt.Sprintf(`"%s`, keygen.PrefixUser)) {
		t.Errorf("created response lacks key plaintext: %s", body)
	}
}

// uscMembershipID returns the org_memberships row ID of userID in orgID.
func uscMembershipID(t *testing.T, database *db.DB, orgID, userID string) string {
	t.Helper()
	ms, err := database.ListOrgMemberships(context.Background(), orgID, "", 200)
	if err != nil {
		t.Fatalf("ListOrgMemberships: %v", err)
	}
	for _, m := range ms {
		if m.UserID == userID {
			return m.ID
		}
	}
	t.Fatalf("no membership for user %s in org %s", userID, orgID)
	return ""
}

// uscMembershipURL returns the member URL for a membership ID.
func uscMembershipURL(orgID, membershipID string) string {
	return "/api/v1/orgs/" + orgID + "/members/" + membershipID
}

// uscOrgAdminWorld returns a world plus a token for an org_admin caller with a
// user, and the caller's user ID.
func uscOrgAdminWorld(t *testing.T, dsn string) (*uscWorld, string, string) {
	t.Helper()
	w, addSelf := newUscWorld(t, dsn)
	selfID := addSelf(auth.RoleOrgAdmin)
	token := addTestKeyInfo(t, w.keyCache, auth.KeyInfo{
		KeyType: keygen.KeyTypeUser, Role: auth.RoleOrgAdmin, OrgID: w.orgID, UserID: selfID,
	})
	return w, token, selfID
}

func TestUserScopeAuthz_MembershipChanges(t *testing.T) {
	t.Parallel()
	w, token, _ := uscOrgAdminWorld(t, "file:TestUserScopeAuthz_MembershipChanges?mode=memory&cache=private")
	ctx := context.Background()

	roleOf := func(userID string) string {
		role, err := w.database.GetOrgMembershipRole(ctx, userID, w.orgID)
		if err != nil {
			t.Fatalf("GetOrgMembershipRole: %v", err)
		}
		return role
	}

	for _, label := range []string{"org_admin", "sysadmin_member"} {
		mid := uscMembershipID(t, w.database, w.orgID, w.userIDs[label])
		url := uscMembershipURL(w.orgID, mid)

		if status, body := uscReq(t, w.app, "PATCH", url, token, map[string]any{"role": "member"}); status != 403 {
			t.Errorf("%s: demote status = %d, want 403; body: %s", label, status, body)
		}
		if got := roleOf(w.userIDs[label]); got != auth.RoleOrgAdmin {
			t.Errorf("%s: role after refused demote = %q, want org_admin", label, got)
		}
		if status, body := uscReq(t, w.app, "PATCH", url, token, map[string]any{"daily_token_limit": 5}); status != 403 {
			t.Errorf("%s: set limits status = %d, want 403; body: %s", label, status, body)
		}
		if status, body := uscReq(t, w.app, "DELETE", url, token, nil); status != 403 {
			t.Errorf("%s: delete status = %d, want 403; body: %s", label, status, body)
		}
		if got := roleOf(w.userIDs[label]); got != auth.RoleOrgAdmin {
			t.Errorf("%s: membership changed after refused delete: role %q", label, got)
		}
	}

	// A lower-ranked member can still be managed.
	mid := uscMembershipID(t, w.database, w.orgID, w.userIDs["member"])
	if status, body := uscReq(t, w.app, "PATCH", uscMembershipURL(w.orgID, mid), token, map[string]any{"daily_token_limit": 5}); status != 200 {
		t.Errorf("member: set limits status = %d, want 200; body: %s", status, body)
	}
}

// TestUserScopeAuthz_OwnMembership checks that acting on one's own membership
// is unchanged. Each successful update reloads the key cache from the DB and
// drops the test-only token, so a fresh token is registered after each one.
func TestUserScopeAuthz_OwnMembership(t *testing.T) {
	t.Parallel()
	w, token, selfID := uscOrgAdminWorld(t, "file:TestUserScopeAuthz_OwnMembership?mode=memory&cache=private")
	selfURL := uscMembershipURL(w.orgID, uscMembershipID(t, w.database, w.orgID, selfID))
	info := auth.KeyInfo{KeyType: keygen.KeyTypeUser, Role: auth.RoleOrgAdmin, OrgID: w.orgID, UserID: selfID}

	if status, body := uscReq(t, w.app, "PATCH", selfURL, token, map[string]any{"daily_token_limit": 7}); status != 200 {
		t.Errorf("self: set limits status = %d, want 200; body: %s", status, body)
	}
	token = addTestKeyInfo(t, w.keyCache, info)
	if status, body := uscReq(t, w.app, "PATCH", selfURL, token, map[string]any{"role": "member"}); status != 200 {
		t.Errorf("self: demote status = %d, want 200; body: %s", status, body)
	}
}

func TestUserScopeAuthz_DeleteOwnMembership(t *testing.T) {
	t.Parallel()
	w, token, selfID := uscOrgAdminWorld(t, "file:TestUserScopeAuthz_DeleteOwnMembership?mode=memory&cache=private")
	mid := uscMembershipID(t, w.database, w.orgID, selfID)
	if status, body := uscReq(t, w.app, "DELETE", uscMembershipURL(w.orgID, mid), token, nil); status != 204 {
		t.Errorf("delete own membership status = %d, want 204; body: %s", status, body)
	}
}

func TestUserScopeAuthz_AddSystemAdminAsMember(t *testing.T) {
	t.Parallel()
	w, token, _ := uscOrgAdminWorld(t, "file:TestUserScopeAuthz_AddSysAdmin?mode=memory&cache=private")

	status, body := uscReq(t, w.app, "POST", "/api/v1/orgs/"+w.orgID+"/members", token,
		map[string]any{"user_id": w.userIDs["sysadmin_nomember"], "role": "member"})
	if status != 403 {
		t.Errorf("add system admin status = %d, want 403; body: %s", status, body)
	}
	if _, err := w.database.GetOrgMembershipRole(context.Background(), w.userIDs["sysadmin_nomember"], w.orgID); err == nil {
		t.Error("system admin gained a membership despite the refusal")
	}

	// Adding a member of another org stays allowed.
	status, body = uscReq(t, w.app, "POST", "/api/v1/orgs/"+w.orgID+"/members", token,
		map[string]any{"user_id": w.userIDs["other_org_only"], "role": "member"})
	if status != 201 {
		t.Errorf("add other-org user status = %d, want 201; body: %s", status, body)
	}
}

func TestUserScopeAuthz_KeyUpdateDelete(t *testing.T) {
	t.Parallel()
	w, token, _ := uscOrgAdminWorld(t, "file:TestUserScopeAuthz_KeyUpdateDelete?mode=memory&cache=private")
	ctx := context.Background()

	for _, label := range []string{"org_admin", "sysadmin_member"} {
		keyID := w.keyIDs[label]
		url := keysURL(w.orgID) + "/" + keyID
		before, err := w.database.GetAPIKey(ctx, keyID)
		if err != nil {
			t.Fatalf("GetAPIKey: %v", err)
		}
		if status, body := uscReq(t, w.app, "PATCH", url, token, map[string]any{"expires_at": "2099-01-01T00:00:00Z"}); status != 403 {
			t.Errorf("%s: patch status = %d, want 403; body: %s", label, status, body)
		}
		if status, body := uscReq(t, w.app, "DELETE", url, token, nil); status != 403 {
			t.Errorf("%s: delete status = %d, want 403; body: %s", label, status, body)
		}
		after, err := w.database.GetAPIKey(ctx, keyID)
		if err != nil {
			t.Fatalf("%s: key gone after refused delete: %v", label, err)
		}
		if (before.ExpiresAt == nil) != (after.ExpiresAt == nil) {
			t.Errorf("%s: expires_at changed by a refused patch", label)
		}
	}

	memberURL := keysURL(w.orgID) + "/" + w.keyIDs["member"]
	if status, body := uscReq(t, w.app, "PATCH", memberURL, token, map[string]any{"expires_at": "2099-01-01T00:00:00Z"}); status != 200 {
		t.Errorf("member key patch status = %d, want 200; body: %s", status, body)
	}
	if status, body := uscReq(t, w.app, "DELETE", memberURL, token, nil); status != 204 {
		t.Errorf("member key delete status = %d, want 204; body: %s", status, body)
	}
}

func TestUserScopeAuthz_DeletedOtherOrgIgnored(t *testing.T) {
	t.Parallel()
	w, token, _ := uscOrgAdminWorld(t, "file:TestUserScopeAuthz_DeletedOtherOrg?mode=memory&cache=private")
	ctx := context.Background()
	multi := w.userIDs["multi_org"]

	if n, err := w.database.CountOtherOrgMemberships(ctx, multi, w.orgID); err != nil || n != 1 {
		t.Fatalf("CountOtherOrgMemberships = %d, %v; want 1, nil", n, err)
	}
	status, body := uscReq(t, w.app, "PATCH", "/api/v1/users/"+multi, token, map[string]any{"password": "Another-pass-77"})
	if status != 403 {
		t.Fatalf("password change with live second org status = %d, want 403; body: %s", status, body)
	}

	if err := w.database.DeleteOrg(ctx, w.org2ID); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}
	if n, err := w.database.CountOtherOrgMemberships(ctx, multi, w.orgID); err != nil || n != 0 {
		t.Fatalf("CountOtherOrgMemberships after delete = %d, %v; want 0, nil", n, err)
	}
	status, body = uscReq(t, w.app, "PATCH", "/api/v1/users/"+multi, token, map[string]any{"password": "Another-pass-77"})
	if status != 200 {
		t.Errorf("password change with deleted second org status = %d, want 200; body: %s", status, body)
	}
}

func TestUserScopeAuthz_CreateKeyUniformUnknownUser(t *testing.T) {
	t.Parallel()
	w, token, _ := uscOrgAdminWorld(t, "file:TestUserScopeAuthz_UniformUnknown?mode=memory&cache=private")

	create := func(userID string) (int, string) {
		return uscReq(t, w.app, "POST", keysURL(w.orgID), token, map[string]any{
			"name": "probe", "key_type": keygen.KeyTypeUser, "user_id": userID,
		})
	}
	s1, b1 := create("00000000-0000-7000-8000-000000000000")
	s2, b2 := create(w.userIDs["other_org_only"])
	if s1 != 400 || s2 != 400 {
		t.Errorf("statuses = %d, %d; want 400, 400", s1, s2)
	}
	if b1 != b2 {
		t.Errorf("bodies differ:\n nonexistent: %s\n other org:   %s", b1, b2)
	}
}

// TestUserScopeAuthz_SSOUserPasswordPatch documents current behavior: a user
// without a password hash (OIDC) can have a password set by an org_admin when
// the usual rules pass, since the handler does not inspect auth_provider.
func TestUserScopeAuthz_SSOUserPasswordPatch(t *testing.T) {
	t.Parallel()
	w, token, _ := uscOrgAdminWorld(t, "file:TestUserScopeAuthz_SSOPassword?mode=memory&cache=private")

	sso, err := w.database.CreateUser(context.Background(), db.CreateUserParams{
		Email: "sso-user@example.com", DisplayName: "SSO", AuthProvider: "oidc",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	uscMember(t, w.database, w.orgID, sso.ID, auth.RoleMember)

	status, body := uscReq(t, w.app, "PATCH", "/api/v1/users/"+sso.ID, token, map[string]any{"password": "Sso-pass-123456"})
	if status != 200 {
		t.Errorf("status = %d, want 200 (current behavior); body: %s", status, body)
	}
}

func TestUserScopeAuthz_CrossOrgKeyIssuance(t *testing.T) {
	t.Parallel()
	w, token, _ := uscOrgAdminWorld(t, "file:TestUserScopeAuthz_CrossOrgKeys?mode=memory&cache=private")
	multi := w.userIDs["multi_org"]

	before := w.countUserKeys(t, multi)
	status, body := uscReq(t, w.app, "POST", keysURL(w.orgID), token, map[string]any{
		"name": "x", "key_type": keygen.KeyTypeUser, "user_id": multi,
	})
	if status != 403 {
		t.Errorf("org_admin create for multi-org member status = %d, want 403; body: %s", status, body)
	}
	if got := w.countUserKeys(t, multi); got != before {
		t.Errorf("key rows = %d, want %d", got, before)
	}

	status, body = uscReq(t, w.app, "POST", keysURL(w.orgID)+"/"+w.keyIDs["multi_org"]+"/rotate", token, nil)
	if status != 403 {
		t.Errorf("org_admin rotate multi-org member key status = %d, want 403; body: %s", status, body)
	}
	if got := w.countUserKeys(t, multi); got != before {
		t.Errorf("key rows after rotate = %d, want %d", got, before)
	}

	// The member itself may create keys in either org.
	for _, orgID := range []string{w.orgID, w.org2ID} {
		memberToken := addTestKeyInfo(t, w.keyCache, auth.KeyInfo{
			KeyType: keygen.KeyTypeUser, Role: auth.RoleMember, OrgID: orgID, UserID: multi,
		})
		status, body = uscReq(t, w.app, "POST", keysURL(orgID), memberToken, map[string]any{
			"name": "own", "key_type": keygen.KeyTypeUser, "user_id": multi,
		})
		if status != 201 {
			t.Errorf("member own key in org %s status = %d, want 201; body: %s", orgID, status, body)
		}
	}

	// A system admin may issue and rotate.
	sys := uscUser(t, w.database, "sys-cross@example.com", "Sys-pass-12345", true)
	sysToken := addTestKeyInfo(t, w.keyCache, auth.KeyInfo{
		KeyType: keygen.KeyTypeUser, Role: auth.RoleSystemAdmin, OrgID: w.orgID, UserID: sys.ID,
	})
	status, body = uscReq(t, w.app, "POST", keysURL(w.orgID), sysToken, map[string]any{
		"name": "by-sys", "key_type": keygen.KeyTypeUser, "user_id": multi,
	})
	if status != 201 {
		t.Errorf("system admin create status = %d, want 201; body: %s", status, body)
	}
	status, body = uscReq(t, w.app, "POST", keysURL(w.orgID)+"/"+w.keyIDs["multi_org"]+"/rotate", sysToken, nil)
	if status != 200 {
		t.Errorf("system admin rotate status = %d, want 200; body: %s", status, body)
	}
}

func TestUserScopeAuthz_AddUnknownUserAsMember(t *testing.T) {
	t.Parallel()
	w, token, _ := uscOrgAdminWorld(t, "file:TestUserScopeAuthz_AddUnknown?mode=memory&cache=private")
	status, body := uscReq(t, w.app, "POST", "/api/v1/orgs/"+w.orgID+"/members", token,
		map[string]any{"user_id": "00000000-0000-7000-8000-000000000000", "role": "member"})
	if status != 400 {
		t.Errorf("status = %d, want 400; body: %s", status, body)
	}
}
