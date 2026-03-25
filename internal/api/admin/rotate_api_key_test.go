package admin_test

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// rotateKeyURL returns the rotate endpoint URL for a given key within an org.
func rotateKeyURL(orgID, keyID string) string {
	return "/api/v1/orgs/" + orgID + "/keys/" + keyID + "/rotate"
}

// mustCreateUserKeyViaAPI creates a user_key by calling the CreateAPIKey handler
// and returns the created key's ID and plaintext value.
func mustCreateUserKeyViaAPI(t *testing.T, app *fiber.App, orgID, userID, teamID, callerKey string) (keyID, plaintext string) {
	t.Helper()
	body := map[string]any{
		"name":     "rotate-test-key",
		"key_type": keygen.KeyTypeUser,
		"user_id":  userID,
		"team_id":  teamID,
	}
	req := httptest.NewRequest("POST", keysURL(orgID), bodyJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+callerKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("mustCreateUserKeyViaAPI: app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("mustCreateUserKeyViaAPI: status = %d, want 201; body: %s", resp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)
	keyID = got["id"].(string)
	plaintext = got["key"].(string)
	return keyID, plaintext
}

// mustCreateSessionKeyInDB inserts a session_key row directly via the DB layer,
// bypassing the CreateAPIKey handler (which rejects session keys).
func mustCreateSessionKeyInDB(t *testing.T, database *db.DB, orgID, userID, createdBy string) *db.APIKey {
	t.Helper()
	plaintext, err := keygen.Generate(keygen.KeyTypeSession)
	if err != nil {
		t.Fatalf("mustCreateSessionKeyInDB: generate: %v", err)
	}
	keyHash := keygen.Hash(plaintext, testHMACSecret)
	keyHint := keygen.Hint(plaintext)

	apiKey, err := database.CreateAPIKey(context.Background(), db.CreateAPIKeyParams{
		KeyHash:   keyHash,
		KeyHint:   keyHint,
		KeyType:   keygen.KeyTypeSession,
		Name:      "session-key-for-rotate-test",
		OrgID:     orgID,
		UserID:    &userID,
		CreatedBy: createdBy,
	})
	if err != nil {
		t.Fatalf("mustCreateSessionKeyInDB: CreateAPIKey: %v", err)
	}
	return apiKey
}

// ---- POST /api/v1/orgs/:org_id/keys/:key_id/rotate --------------------------

func TestRotateAPIKey_MemberRotatesOwnKey(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestRotateAPIKey_MemberOwn?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "Acme", "rotate-member-own-org")
	user := mustCreateUser(t, database, "rotate-member@example.com", "Rotate Member")
	team := mustCreateTeam(t, database, org.ID, "Dev", "rotate-member-team")
	mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)

	// The member's caller key — used both to create the target key and to rotate it.
	callerKey := addTestKeyWithUser(t, keyCache, auth.RoleMember, org.ID, user.ID)

	// Create a user_key owned by this member.
	keyID, _ := mustCreateUserKeyViaAPI(t, app, org.ID, user.ID, team.ID, callerKey)

	// Rotate it.
	req := httptest.NewRequest("POST", rotateKeyURL(org.ID, keyID), nil)
	req.Header.Set("Authorization", "Bearer "+callerKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	// new_key must contain a plaintext key and a hint.
	newKey, ok := got["new_key"].(map[string]any)
	if !ok {
		t.Fatalf("response missing 'new_key' object; got: %v", got)
	}
	plaintextNew, ok := newKey["key"].(string)
	if !ok || plaintextNew == "" {
		t.Errorf("new_key.key is absent or empty; got: %v", newKey)
	}
	hint, ok := newKey["hint"].(string)
	if !ok || hint == "" {
		t.Errorf("new_key.hint is absent or empty; got: %v", newKey)
	}

	// old_key must have expires_at set (grace period).
	oldKey, ok := got["old_key"].(map[string]any)
	if !ok {
		t.Fatalf("response missing 'old_key' object; got: %v", got)
	}
	oldExpiresAt, ok := oldKey["expires_at"].(string)
	if !ok || oldExpiresAt == "" {
		t.Errorf("old_key.expires_at is absent or empty; got: %v", oldKey)
	}

	// The new key must start with the user_key prefix.
	if !strings.HasPrefix(plaintextNew, keygen.PrefixUser) {
		t.Errorf("new_key.key = %q, want prefix %q", plaintextNew, keygen.PrefixUser)
	}
}

func TestRotateAPIKey_ResponseFields(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestRotateAPIKey_Fields?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "Acme", "rotate-fields-org")
	user := mustCreateUser(t, database, "rotate-fields@example.com", "Rotate Fields")
	team := mustCreateTeam(t, database, org.ID, "Dev", "rotate-fields-team")
	mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)
	callerKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

	keyID, _ := mustCreateUserKeyViaAPI(t, app, org.ID, user.ID, team.ID, callerKey)

	req := httptest.NewRequest("POST", rotateKeyURL(org.ID, keyID), nil)
	req.Header.Set("Authorization", "Bearer "+callerKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	// Verify new_key fields.
	newKey, _ := got["new_key"].(map[string]any)
	if newKey == nil {
		t.Fatalf("new_key missing from response")
	}
	for _, field := range []string{"id", "key", "hint"} {
		v, ok := newKey[field].(string)
		if !ok || v == "" {
			t.Errorf("new_key.%s is absent or empty; got: %v", field, newKey)
		}
	}

	// Verify old_key fields.
	oldKey, _ := got["old_key"].(map[string]any)
	if oldKey == nil {
		t.Fatalf("old_key missing from response")
	}
	for _, field := range []string{"id", "hint"} {
		v, ok := oldKey[field].(string)
		if !ok || v == "" {
			t.Errorf("old_key.%s is absent or empty; got: %v", field, oldKey)
		}
	}
	// old_key must NOT expose the plaintext key.
	if _, exists := oldKey["key"]; exists {
		t.Errorf("old_key must not contain plaintext 'key' field")
	}
}

func TestRotateAPIKey_MemberCannotRotateOtherUsersKey(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestRotateAPIKey_CrossUser?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "Acme", "rotate-cross-user-org")
	owner := mustCreateUser(t, database, "rotate-owner@example.com", "Owner")
	attacker := mustCreateUser(t, database, "rotate-attacker@example.com", "Attacker")
	team := mustCreateTeam(t, database, org.ID, "Dev", "rotate-cross-user-team")
	mustCreateUserMemberships(t, database, org.ID, team.ID, owner.ID)
	mustCreateUserMemberships(t, database, org.ID, team.ID, attacker.ID)

	// Create a key owned by 'owner', using an org_admin caller key.
	adminKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, owner.ID)
	keyID, _ := mustCreateUserKeyViaAPI(t, app, org.ID, owner.ID, team.ID, adminKey)

	// 'attacker' is a member scoped to their own user ID — cannot rotate owner's key.
	attackerKey := addTestKeyWithUser(t, keyCache, auth.RoleMember, org.ID, attacker.ID)

	req := httptest.NewRequest("POST", rotateKeyURL(org.ID, keyID), nil)
	req.Header.Set("Authorization", "Bearer "+attackerKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 403; body: %s", resp.StatusCode, b)
	}
}

func TestRotateAPIKey_DeletedKeyReturns404(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestRotateAPIKey_Deleted?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "Acme", "rotate-deleted-org")
	user := mustCreateUser(t, database, "rotate-deleted@example.com", "Del User")
	team := mustCreateTeam(t, database, org.ID, "Dev", "rotate-deleted-team")
	mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)
	callerKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

	keyID, _ := mustCreateUserKeyViaAPI(t, app, org.ID, user.ID, team.ID, callerKey)

	// Soft-delete the key directly.
	if err := database.DeleteAPIKey(context.Background(), keyID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}

	req := httptest.NewRequest("POST", rotateKeyURL(org.ID, keyID), nil)
	req.Header.Set("Authorization", "Bearer "+callerKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 404; body: %s", resp.StatusCode, b)
	}
}

func TestRotateAPIKey_SessionKeyReturns400(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestRotateAPIKey_Session?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "Acme", "rotate-session-org")
	user := mustCreateUser(t, database, "rotate-session@example.com", "Session User")
	callerKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

	// Insert a session_key directly — the handler forbids rotating it.
	sessionKey := mustCreateSessionKeyInDB(t, database, org.ID, user.ID, user.ID)

	req := httptest.NewRequest("POST", rotateKeyURL(org.ID, sessionKey.ID), nil)
	req.Header.Set("Authorization", "Bearer "+callerKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, b)
	}

	var body map[string]any
	decodeBody(t, resp.Body, &body)
	// Error envelope: {"error": {"code": "...", "message": "..."}}
	errObj, _ := body["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "only user, team, and service account keys can be rotated") {
		t.Errorf("error message = %q, want it to mention 'only user, team, and service account keys can be rotated'", msg)
	}
}

func TestRotateAPIKey_GracePeriodIsSet(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestRotateAPIKey_GracePeriod?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "Acme", "rotate-grace-org")
	user := mustCreateUser(t, database, "rotate-grace@example.com", "Grace User")
	team := mustCreateTeam(t, database, org.ID, "Dev", "rotate-grace-team")
	mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)
	callerKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

	keyID, _ := mustCreateUserKeyViaAPI(t, app, org.ID, user.ID, team.ID, callerKey)

	before := time.Now().UTC()

	req := httptest.NewRequest("POST", rotateKeyURL(org.ID, keyID), nil)
	req.Header.Set("Authorization", "Bearer "+callerKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	after := time.Now().UTC()

	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	oldKey, _ := got["old_key"].(map[string]any)
	if oldKey == nil {
		t.Fatalf("old_key missing from response")
	}
	expiresAtStr, ok := oldKey["expires_at"].(string)
	if !ok || expiresAtStr == "" {
		t.Fatalf("old_key.expires_at is absent; got: %v", oldKey)
	}

	expiresAt, err := time.Parse(time.RFC3339, expiresAtStr)
	if err != nil {
		t.Fatalf("parse old_key.expires_at %q: %v", expiresAtStr, err)
	}

	// Grace deadline must be approximately now + 24h.
	// Allow 5 seconds of tolerance on each side for test execution time.
	const gracePeriod = 24 * time.Hour
	const tolerance = 5 * time.Second
	lowerBound := before.Add(gracePeriod - tolerance)
	upperBound := after.Add(gracePeriod + tolerance)

	if expiresAt.Before(lowerBound) || expiresAt.After(upperBound) {
		t.Errorf("old_key.expires_at = %v, want approximately now+24h (between %v and %v)",
			expiresAt, lowerBound, upperBound)
	}
}

func TestRotateAPIKey_ExistingShorterExpiryIsPreserved(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestRotateAPIKey_ShorterExpiry?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "Acme", "rotate-short-expiry-org")
	user := mustCreateUser(t, database, "rotate-short@example.com", "Short Expiry User")
	team := mustCreateTeam(t, database, org.ID, "Dev", "rotate-short-team")
	mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)
	callerKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

	// Create a key that expires in 1 hour — shorter than the 24h grace period.
	shortExpiry := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
	body := map[string]any{
		"name":       "short-expiry-key",
		"key_type":   keygen.KeyTypeUser,
		"user_id":    user.ID,
		"team_id":    team.ID,
		"expires_at": shortExpiry,
	}
	createReq := httptest.NewRequest("POST", keysURL(org.ID), bodyJSON(t, body))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+callerKey)

	createResp, err := app.Test(createReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var created map[string]any
	decodeBody(t, createResp.Body, &created)
	createResp.Body.Close()
	keyID := created["id"].(string)

	// Rotate the key.
	req := httptest.NewRequest("POST", rotateKeyURL(org.ID, keyID), nil)
	req.Header.Set("Authorization", "Bearer "+callerKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	oldKey, _ := got["old_key"].(map[string]any)
	if oldKey == nil {
		t.Fatalf("old_key missing from response")
	}
	expiresAtStr, ok := oldKey["expires_at"].(string)
	if !ok || expiresAtStr == "" {
		t.Fatalf("old_key.expires_at is absent; got: %v", oldKey)
	}

	expiresAt, err := time.Parse(time.RFC3339, expiresAtStr)
	if err != nil {
		t.Fatalf("parse old_key.expires_at %q: %v", expiresAtStr, err)
	}

	// The expiry must be preserved at approximately 1 hour from now, not 24h.
	// Allow 30 seconds of tolerance.
	const tolerance = 30 * time.Second
	upperBound := time.Now().UTC().Add(1*time.Hour + tolerance)

	if expiresAt.After(upperBound) {
		t.Errorf("old_key.expires_at = %v, want it to be <= 1h from now (the original shorter expiry), got > %v",
			expiresAt, upperBound)
	}
}

func TestRotateAPIKey_NewKeyHasSameMetadata(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestRotateAPIKey_Metadata?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "Acme", "rotate-metadata-org")
	user := mustCreateUser(t, database, "rotate-meta@example.com", "Meta User")
	team := mustCreateTeam(t, database, org.ID, "Dev", "rotate-meta-team")
	mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)
	callerKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

	// Create a key with non-default limits so we can verify they are copied.
	body := map[string]any{
		"name":                "meta-test-key",
		"key_type":            keygen.KeyTypeUser,
		"user_id":             user.ID,
		"team_id":             team.ID,
		"daily_token_limit":   float64(5000),
		"monthly_token_limit": float64(50000),
		"requests_per_minute": float64(10),
		"requests_per_day":    float64(100),
	}
	createReq := httptest.NewRequest("POST", keysURL(org.ID), bodyJSON(t, body))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+callerKey)

	createResp, err := app.Test(createReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var created map[string]any
	decodeBody(t, createResp.Body, &created)
	createResp.Body.Close()
	oldKeyID := created["id"].(string)

	// Rotate.
	req := httptest.NewRequest("POST", rotateKeyURL(org.ID, oldKeyID), nil)
	req.Header.Set("Authorization", "Bearer "+callerKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	// Retrieve the new key to verify its metadata.
	newKey, _ := got["new_key"].(map[string]any)
	if newKey == nil {
		t.Fatalf("new_key missing from response")
	}
	newKeyID, _ := newKey["id"].(string)
	if newKeyID == "" {
		t.Fatalf("new_key.id is empty")
	}

	getReq := httptest.NewRequest("GET", keyItemURL(org.ID, newKeyID), nil)
	getReq.Header.Set("Authorization", "Bearer "+callerKey)

	getResp, err := app.Test(getReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("get new key: %v", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(getResp.Body)
		t.Fatalf("GET new key status = %d; body: %s", getResp.StatusCode, b)
	}

	var newKeyDetails map[string]any
	decodeBody(t, getResp.Body, &newKeyDetails)

	if newKeyDetails["key_type"] != keygen.KeyTypeUser {
		t.Errorf("new key key_type = %q, want %q", newKeyDetails["key_type"], keygen.KeyTypeUser)
	}
	if newKeyDetails["org_id"] != org.ID {
		t.Errorf("new key org_id = %q, want %q", newKeyDetails["org_id"], org.ID)
	}
	if newKeyDetails["daily_token_limit"] != float64(5000) {
		t.Errorf("new key daily_token_limit = %v, want 5000", newKeyDetails["daily_token_limit"])
	}
	if newKeyDetails["monthly_token_limit"] != float64(50000) {
		t.Errorf("new key monthly_token_limit = %v, want 50000", newKeyDetails["monthly_token_limit"])
	}
	if newKeyDetails["requests_per_minute"] != float64(10) {
		t.Errorf("new key requests_per_minute = %v, want 10", newKeyDetails["requests_per_minute"])
	}
	if newKeyDetails["requests_per_day"] != float64(100) {
		t.Errorf("new key requests_per_day = %v, want 100", newKeyDetails["requests_per_day"])
	}
}

func TestRotateAPIKey_OrgAdminCanRotateAnyKeyInOrg(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestRotateAPIKey_OrgAdmin?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "Acme", "rotate-orgadmin-org")
	owner := mustCreateUser(t, database, "rotate-owner2@example.com", "Owner2")
	admin := mustCreateUser(t, database, "rotate-admin@example.com", "Admin")
	team := mustCreateTeam(t, database, org.ID, "Dev", "rotate-orgadmin-team")
	mustCreateUserMemberships(t, database, org.ID, team.ID, owner.ID)
	mustCreateUserMemberships(t, database, org.ID, team.ID, admin.ID)

	// Use org_admin key to create a key owned by 'owner'.
	adminKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, admin.ID)
	keyID, _ := mustCreateUserKeyViaAPI(t, app, org.ID, owner.ID, team.ID, adminKey)

	// The org_admin (admin user) rotates a key they don't own — must succeed.
	req := httptest.NewRequest("POST", rotateKeyURL(org.ID, keyID), nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}
}

func TestRotateAPIKey_NotFound(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestRotateAPIKey_NotFound?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Acme", "rotate-notfound-org")
	user := mustCreateUser(t, database, "rotate-nf@example.com", "NF User")
	callerKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

	req := httptest.NewRequest("POST", rotateKeyURL(org.ID, "00000000-0000-0000-0000-000000000000"), nil)
	req.Header.Set("Authorization", "Bearer "+callerKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 404; body: %s", resp.StatusCode, b)
	}
}

func TestRotateAPIKey_CrossOrgKeyReturns404(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestRotateAPIKey_CrossOrg?mode=memory&cache=private")

	orgA := mustCreateOrg(t, database, "Org A", "rotate-cross-org-a")
	orgB := mustCreateOrg(t, database, "Org B", "rotate-cross-org-b")
	userA := mustCreateUser(t, database, "rotate-cross-a@example.com", "User A")
	userB := mustCreateUser(t, database, "rotate-cross-b@example.com", "User B")
	teamA := mustCreateTeam(t, database, orgA.ID, "Dev A", "rotate-cross-team-a")
	teamB := mustCreateTeam(t, database, orgB.ID, "Dev B", "rotate-cross-team-b")
	mustCreateUserMemberships(t, database, orgA.ID, teamA.ID, userA.ID)
	mustCreateUserMemberships(t, database, orgB.ID, teamB.ID, userB.ID)

	// Create a key in orgB.
	adminKeyB := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, orgB.ID, userB.ID)
	keyIDInOrgB, _ := mustCreateUserKeyViaAPI(t, app, orgB.ID, userB.ID, teamB.ID, adminKeyB)

	// Try to rotate orgB's key using orgA's admin.
	adminKeyA := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, orgA.ID, userA.ID)
	req := httptest.NewRequest("POST", rotateKeyURL(orgA.ID, keyIDInOrgB), nil)
	req.Header.Set("Authorization", "Bearer "+adminKeyA)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 404; body: %s", resp.StatusCode, b)
	}
}

func TestRotateAPIKey_NoAuth(t *testing.T) {
	t.Parallel()

	app, database, _ := setupTestApp(t, "file:TestRotateAPIKey_NoAuth?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Acme", "rotate-noauth-org")

	req := httptest.NewRequest("POST", rotateKeyURL(org.ID, "00000000-0000-0000-0000-000000000000"), nil)
	// No Authorization header.

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestRotateAPIKey_NewKeyAddedToCache(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestRotateAPIKey_Cache?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "Acme", "rotate-cache-org")
	user := mustCreateUser(t, database, "rotate-cache@example.com", "Cache User")
	team := mustCreateTeam(t, database, org.ID, "Dev", "rotate-cache-team")
	mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)
	callerKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

	keyID, _ := mustCreateUserKeyViaAPI(t, app, org.ID, user.ID, team.ID, callerKey)

	req := httptest.NewRequest("POST", rotateKeyURL(org.ID, keyID), nil)
	req.Header.Set("Authorization", "Bearer "+callerKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	newKey, _ := got["new_key"].(map[string]any)
	if newKey == nil {
		t.Fatalf("new_key missing from response")
	}
	newPlaintext, _ := newKey["key"].(string)
	if newPlaintext == "" {
		t.Fatalf("new_key.key is empty")
	}

	// The new key must be in the auth cache so it can authenticate immediately.
	newKeyHash := keygen.Hash(newPlaintext, testHMACSecret)
	if _, ok := keyCache.Get(newKeyHash); !ok {
		t.Error("new key not found in auth cache after rotation, want it to be cached")
	}
}

// TestRotateAPIKey_KeyTypeVariants verifies that rotation works for all
// routable key types: user_key, team_key, and sa_key.
func TestRotateAPIKey_KeyTypeVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setupKey   func(t *testing.T, database *db.DB, org *db.Org, user *db.User, team *db.Team, callerID string) (keyID string)
		wantPrefix string
	}{
		{
			name: "rotate user_key",
			setupKey: func(t *testing.T, database *db.DB, org *db.Org, user *db.User, team *db.Team, callerID string) string {
				t.Helper()
				plaintext, err := keygen.Generate(keygen.KeyTypeUser)
				if err != nil {
					t.Fatalf("generate user key: %v", err)
				}
				apiKey, err := database.CreateAPIKey(context.Background(), db.CreateAPIKeyParams{
					KeyHash:   keygen.Hash(plaintext, testHMACSecret),
					KeyHint:   keygen.Hint(plaintext),
					KeyType:   keygen.KeyTypeUser,
					Name:      "variant-user-key",
					OrgID:     org.ID,
					UserID:    &user.ID,
					TeamID:    &team.ID,
					CreatedBy: callerID,
				})
				if err != nil {
					t.Fatalf("CreateAPIKey user: %v", err)
				}
				return apiKey.ID
			},
			wantPrefix: keygen.PrefixUser,
		},
		{
			name: "rotate team_key",
			setupKey: func(t *testing.T, database *db.DB, org *db.Org, user *db.User, team *db.Team, callerID string) string {
				t.Helper()
				plaintext, err := keygen.Generate(keygen.KeyTypeTeam)
				if err != nil {
					t.Fatalf("generate team key: %v", err)
				}
				apiKey, err := database.CreateAPIKey(context.Background(), db.CreateAPIKeyParams{
					KeyHash:   keygen.Hash(plaintext, testHMACSecret),
					KeyHint:   keygen.Hint(plaintext),
					KeyType:   keygen.KeyTypeTeam,
					Name:      "variant-team-key",
					OrgID:     org.ID,
					TeamID:    &team.ID,
					CreatedBy: callerID,
				})
				if err != nil {
					t.Fatalf("CreateAPIKey team: %v", err)
				}
				return apiKey.ID
			},
			wantPrefix: "vl_tk_",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestRotateAPIKey_Variant_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "O", "rotate-var-org-"+strings.ReplaceAll(tc.name, " ", "-"))
			user := mustCreateUser(t, database, "rotate-var-"+strings.ReplaceAll(tc.name, " ", "")+"@example.com", "V")
			team := mustCreateTeam(t, database, org.ID, "T", "rotate-var-team-"+strings.ReplaceAll(tc.name, " ", "-"))
			mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)
			callerKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

			keyID := tc.setupKey(t, database, org, user, team, user.ID)

			req := httptest.NewRequest("POST", rotateKeyURL(org.ID, keyID), nil)
			req.Header.Set("Authorization", "Bearer "+callerKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusOK {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want 200; body: %s", resp.StatusCode, b)
				return
			}

			var got map[string]any
			decodeBody(t, resp.Body, &got)

			newKey, _ := got["new_key"].(map[string]any)
			if newKey == nil {
				t.Fatalf("new_key missing")
			}
			newPlaintext, _ := newKey["key"].(string)
			if !strings.HasPrefix(newPlaintext, tc.wantPrefix) {
				t.Errorf("new_key.key = %q, want prefix %q", newPlaintext, tc.wantPrefix)
			}
		})
	}
}
