package auth

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"

	_ "modernc.org/sqlite"
)

// bootstrapSecret is a stable 32-byte HMAC secret used across all bootstrap tests.
var bootstrapSecret = []byte("bootstrap-test-secret-32-bytes!!")

// defaultBootstrapCfg returns a SettingsConfig with the default bootstrap values
// and the given admin key. Tests that only care about key length or no-op
// behaviour can use this helper rather than constructing the struct inline.
func defaultBootstrapCfg(adminKey string) config.SettingsConfig {
	return config.SettingsConfig{
		AdminKey: adminKey,
		Bootstrap: config.BootstrapConfig{
			OrgName:    "Default",
			OrgSlug:    "default",
			AdminEmail: "admin@voidllm.local",
		},
	}
}

// setupBootstrapDB opens a private in-memory SQLite database using testName as
// the unique URI component, applies all migrations, and registers cleanup.
func setupBootstrapDB(t *testing.T, testName string) *sql.DB {
	t.Helper()
	// Use cache=private so each test gets a fully isolated database even when
	// run in parallel. The testName makes the URI unique within the process.
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=private", testName)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })

	if err := db.RunMigrations(context.Background(), sqlDB, db.SQLiteDialect{}); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return sqlDB
}

// discardLogger returns a slog.Logger that drops all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(noopWriter{}, nil))
}

// noopWriter implements io.Writer and discards all writes.
type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }

// countRows returns the number of rows in the named table.
func countRows(t *testing.T, sqlDB *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := sqlDB.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestBootstrap_EmptyAdminKey verifies that passing an empty adminKey is a
// no-op: Bootstrap returns nil and nothing is written to the database.
func TestBootstrap_EmptyAdminKey(t *testing.T) {
	t.Parallel()

	sqlDB := setupBootstrapDB(t, t.Name())
	keyCache := cache.New[string, KeyInfo]()

	_, err := Bootstrap(context.Background(), sqlDB, db.SQLiteDialect{},
		keyCache, defaultBootstrapCfg(""), bootstrapSecret, discardLogger())
	if err != nil {
		t.Fatalf("Bootstrap() with empty key returned error: %v", err)
	}

	if got := keyCache.Len(); got != 0 {
		t.Errorf("cache.Len() = %d, want 0", got)
	}
	if got := countRows(t, sqlDB, "organizations"); got != 0 {
		t.Errorf("organizations count = %d, want 0", got)
	}
	if got := countRows(t, sqlDB, "users"); got != 0 {
		t.Errorf("users count = %d, want 0", got)
	}
	if got := countRows(t, sqlDB, "api_keys"); got != 0 {
		t.Errorf("api_keys count = %d, want 0", got)
	}
}

// TestBootstrap_AdminKeyTooShort verifies that a key shorter than 32 characters
// returns an error containing "at least 32 characters".
func TestBootstrap_AdminKeyTooShort(t *testing.T) {
	t.Parallel()

	sqlDB := setupBootstrapDB(t, t.Name())
	keyCache := cache.New[string, KeyInfo]()

	tests := []struct {
		name string
		key  string
	}{
		{name: "one char", key: "x"},
		{name: "31 chars", key: strings.Repeat("a", 31)},
		{name: "five chars", key: "short"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Bootstrap(context.Background(), sqlDB, db.SQLiteDialect{},
				keyCache, defaultBootstrapCfg(tc.key), bootstrapSecret, discardLogger())
			if err == nil {
				t.Fatal("Bootstrap() returned nil, want error")
			}
			if !strings.Contains(err.Error(), "at least 32 characters") {
				t.Errorf("error = %q, want message containing %q",
					err.Error(), "at least 32 characters")
			}
		})
	}
}

// TestBootstrap_DBAlreadyHasKeys verifies that when the database already
// contains an API key, Bootstrap is a no-op and returns nil. We achieve this
// by calling Bootstrap twice and checking that the second call does not add
// any additional rows.
func TestBootstrap_DBAlreadyHasKeys(t *testing.T) {
	t.Parallel()

	sqlDB := setupBootstrapDB(t, t.Name())
	keyCache := cache.New[string, KeyInfo]()
	cfg := defaultBootstrapCfg(strings.Repeat("b", 32))
	log := discardLogger()

	// First call creates everything.
	if _, err := Bootstrap(context.Background(), sqlDB, db.SQLiteDialect{},
		keyCache, cfg, bootstrapSecret, log); err != nil {
		t.Fatalf("first Bootstrap() call: %v", err)
	}

	orgs := countRows(t, sqlDB, "organizations")
	users := countRows(t, sqlDB, "users")
	keys := countRows(t, sqlDB, "api_keys")

	// Second call should be a no-op.
	if _, err := Bootstrap(context.Background(), sqlDB, db.SQLiteDialect{},
		keyCache, cfg, bootstrapSecret, log); err != nil {
		t.Fatalf("second Bootstrap() call: %v", err)
	}

	if got := countRows(t, sqlDB, "organizations"); got != orgs {
		t.Errorf("organizations count after second call = %d, want %d", got, orgs)
	}
	if got := countRows(t, sqlDB, "users"); got != users {
		t.Errorf("users count after second call = %d, want %d", got, users)
	}
	if got := countRows(t, sqlDB, "api_keys"); got != keys {
		t.Errorf("api_keys count after second call = %d, want %d", got, keys)
	}
}

// TestBootstrap_CreatesEntities verifies that a valid adminKey causes Bootstrap
// to insert exactly one organization, one user, one org_membership, and one
// api_key with the expected field values.
func TestBootstrap_CreatesEntities(t *testing.T) {
	t.Parallel()

	sqlDB := setupBootstrapDB(t, t.Name())
	keyCache := cache.New[string, KeyInfo]()
	cfg := defaultBootstrapCfg(strings.Repeat("c", 32))

	if _, err := Bootstrap(context.Background(), sqlDB, db.SQLiteDialect{},
		keyCache, cfg, bootstrapSecret, discardLogger()); err != nil {
		t.Fatalf("Bootstrap(): %v", err)
	}

	// Verify organization.
	var orgName, orgSlug string
	if err := sqlDB.QueryRowContext(context.Background(),
		"SELECT name, slug FROM organizations WHERE deleted_at IS NULL").
		Scan(&orgName, &orgSlug); err != nil {
		t.Fatalf("query organization: %v", err)
	}
	if orgName != "Default" {
		t.Errorf("org name = %q, want %q", orgName, "Default")
	}
	if orgSlug != "default" {
		t.Errorf("org slug = %q, want %q", orgSlug, "default")
	}
	if got := countRows(t, sqlDB, "organizations"); got != 1 {
		t.Errorf("organizations count = %d, want 1", got)
	}

	// Verify user.
	var email string
	var isAdmin int
	if err := sqlDB.QueryRowContext(context.Background(),
		"SELECT email, is_system_admin FROM users WHERE deleted_at IS NULL").
		Scan(&email, &isAdmin); err != nil {
		t.Fatalf("query user: %v", err)
	}
	if email != "admin@voidllm.local" {
		t.Errorf("user email = %q, want %q", email, "admin@voidllm.local")
	}
	if isAdmin != 1 {
		t.Errorf("is_system_admin = %d, want 1", isAdmin)
	}
	if got := countRows(t, sqlDB, "users"); got != 1 {
		t.Errorf("users count = %d, want 1", got)
	}

	// Verify org membership.
	var memberRole string
	if err := sqlDB.QueryRowContext(context.Background(),
		"SELECT role FROM org_memberships").Scan(&memberRole); err != nil {
		t.Fatalf("query org_membership: %v", err)
	}
	if memberRole != RoleOrgAdmin {
		t.Errorf("membership role = %q, want %q", memberRole, RoleOrgAdmin)
	}
	if got := countRows(t, sqlDB, "org_memberships"); got != 1 {
		t.Errorf("org_memberships count = %d, want 1", got)
	}

	// Verify api_key.
	var keyType, keyName string
	if err := sqlDB.QueryRowContext(context.Background(),
		"SELECT key_type, name FROM api_keys WHERE deleted_at IS NULL").
		Scan(&keyType, &keyName); err != nil {
		t.Fatalf("query api_key: %v", err)
	}
	if keyType != keygen.KeyTypeUser {
		t.Errorf("key_type = %q, want %q", keyType, keygen.KeyTypeUser)
	}
	if keyName != "Bootstrap Admin Key" {
		t.Errorf("key name = %q, want %q", keyName, "Bootstrap Admin Key")
	}
	if got := countRows(t, sqlDB, "api_keys"); got != 1 {
		t.Errorf("api_keys count = %d, want 1", got)
	}
}

// TestBootstrap_KeyInCache verifies that after a successful Bootstrap, the
// generated key hash is present in the cache with the correct KeyInfo fields.
func TestBootstrap_KeyInCache(t *testing.T) {
	t.Parallel()

	sqlDB := setupBootstrapDB(t, t.Name())
	keyCache := cache.New[string, KeyInfo]()
	cfg := defaultBootstrapCfg(strings.Repeat("d", 32))

	if _, err := Bootstrap(context.Background(), sqlDB, db.SQLiteDialect{},
		keyCache, cfg, bootstrapSecret, discardLogger()); err != nil {
		t.Fatalf("Bootstrap(): %v", err)
	}

	// Retrieve the stored hash from the api_keys table.
	var storedHash string
	if err := sqlDB.QueryRowContext(context.Background(),
		"SELECT key_hash FROM api_keys WHERE deleted_at IS NULL").
		Scan(&storedHash); err != nil {
		t.Fatalf("query key_hash: %v", err)
	}

	keyInfo, ok := keyCache.Get(storedHash)
	if !ok {
		t.Fatal("key hash not found in cache")
	}

	if keyInfo.Role != RoleSystemAdmin {
		t.Errorf("cache KeyInfo.Role = %q, want %q", keyInfo.Role, RoleSystemAdmin)
	}
	if keyInfo.KeyType != keygen.KeyTypeUser {
		t.Errorf("cache KeyInfo.KeyType = %q, want %q", keyInfo.KeyType, keygen.KeyTypeUser)
	}
	if keyInfo.Name != "Bootstrap Admin Key" {
		t.Errorf("cache KeyInfo.Name = %q, want %q", keyInfo.Name, "Bootstrap Admin Key")
	}
	if keyInfo.OrgID == "" {
		t.Error("cache KeyInfo.OrgID is empty")
	}
	if keyInfo.UserID == "" {
		t.Error("cache KeyInfo.UserID is empty")
	}
	if keyInfo.ID == "" {
		t.Error("cache KeyInfo.ID is empty")
	}
}

// TestBootstrap_Idempotent verifies that calling Bootstrap twice with the same
// adminKey results in exactly one org, one user, and one key in the database,
// and that both calls return nil.
func TestBootstrap_Idempotent(t *testing.T) {
	t.Parallel()

	sqlDB := setupBootstrapDB(t, t.Name())
	keyCache := cache.New[string, KeyInfo]()
	cfg := defaultBootstrapCfg(strings.Repeat("e", 32))
	log := discardLogger()

	for i := range 2 {
		if _, err := Bootstrap(context.Background(), sqlDB, db.SQLiteDialect{},
			keyCache, cfg, bootstrapSecret, log); err != nil {
			t.Fatalf("Bootstrap() call %d: %v", i+1, err)
		}
	}

	if got := countRows(t, sqlDB, "organizations"); got != 1 {
		t.Errorf("organizations count = %d, want 1", got)
	}
	if got := countRows(t, sqlDB, "users"); got != 1 {
		t.Errorf("users count = %d, want 1", got)
	}
	if got := countRows(t, sqlDB, "api_keys"); got != 1 {
		t.Errorf("api_keys count = %d, want 1", got)
	}
}

// TestBootstrap_CacheOrgUserIDsMatchDB verifies that the OrgID and UserID
// stored in the cache entry match the IDs written to the database.
func TestBootstrap_CacheOrgUserIDsMatchDB(t *testing.T) {
	t.Parallel()

	sqlDB := setupBootstrapDB(t, t.Name())
	keyCache := cache.New[string, KeyInfo]()
	cfg := defaultBootstrapCfg(strings.Repeat("f", 32))

	if _, err := Bootstrap(context.Background(), sqlDB, db.SQLiteDialect{},
		keyCache, cfg, bootstrapSecret, discardLogger()); err != nil {
		t.Fatalf("Bootstrap(): %v", err)
	}

	var storedHash, dbKeyID, dbOrgID, dbUserID string
	if err := sqlDB.QueryRowContext(context.Background(),
		"SELECT key_hash, id, org_id, user_id FROM api_keys WHERE deleted_at IS NULL").
		Scan(&storedHash, &dbKeyID, &dbOrgID, &dbUserID); err != nil {
		t.Fatalf("query api_keys: %v", err)
	}

	keyInfo, ok := keyCache.Get(storedHash)
	if !ok {
		t.Fatal("key hash not found in cache")
	}

	if keyInfo.ID != dbKeyID {
		t.Errorf("cache ID = %q, DB id = %q", keyInfo.ID, dbKeyID)
	}
	if keyInfo.OrgID != dbOrgID {
		t.Errorf("cache OrgID = %q, DB org_id = %q", keyInfo.OrgID, dbOrgID)
	}
	if keyInfo.UserID != dbUserID {
		t.Errorf("cache UserID = %q, DB user_id = %q", keyInfo.UserID, dbUserID)
	}
}

// TestBootstrap_ExactlyOneCacheEntry verifies that Bootstrap places exactly one
// entry in the cache — no stale entries, no duplicates.
func TestBootstrap_ExactlyOneCacheEntry(t *testing.T) {
	t.Parallel()

	sqlDB := setupBootstrapDB(t, t.Name())
	keyCache := cache.New[string, KeyInfo]()
	cfg := defaultBootstrapCfg(strings.Repeat("g", 32))

	if _, err := Bootstrap(context.Background(), sqlDB, db.SQLiteDialect{},
		keyCache, cfg, bootstrapSecret, discardLogger()); err != nil {
		t.Fatalf("Bootstrap(): %v", err)
	}

	if got := keyCache.Len(); got != 1 {
		t.Errorf("cache.Len() = %d, want 1", got)
	}
}

// TestBootstrap_KeyHintFormat verifies that the key_hint stored in the
// database follows the expected "<first6>...<last4>" format produced by
// keygen.Hint.
func TestBootstrap_KeyHintFormat(t *testing.T) {
	t.Parallel()

	sqlDB := setupBootstrapDB(t, t.Name())
	keyCache := cache.New[string, KeyInfo]()
	cfg := defaultBootstrapCfg(strings.Repeat("h", 32))

	if _, err := Bootstrap(context.Background(), sqlDB, db.SQLiteDialect{},
		keyCache, cfg, bootstrapSecret, discardLogger()); err != nil {
		t.Fatalf("Bootstrap(): %v", err)
	}

	var keyHint string
	if err := sqlDB.QueryRowContext(context.Background(),
		"SELECT key_hint FROM api_keys WHERE deleted_at IS NULL").
		Scan(&keyHint); err != nil {
		t.Fatalf("query key_hint: %v", err)
	}

	// keygen.Hint format: "<first6>...<last4>" for keys longer than 10 chars.
	// A user key starts with "vl_uk_" (6 chars) so the hint begins with "vl_uk_".
	if !strings.HasPrefix(keyHint, "vl_uk") {
		t.Errorf("key_hint = %q, want prefix %q", keyHint, "vl_uk")
	}
	if !strings.Contains(keyHint, "...") {
		t.Errorf("key_hint = %q, expected to contain %q", keyHint, "...")
	}
}

// TestBootstrap_CustomOrgAndEmail verifies that configuring custom org name,
// slug, and admin email writes those values to the database.
func TestBootstrap_CustomOrgAndEmail(t *testing.T) {
	t.Parallel()

	sqlDB := setupBootstrapDB(t, t.Name())
	keyCache := cache.New[string, KeyInfo]()
	cfg := config.SettingsConfig{
		AdminKey: strings.Repeat("i", 32),
		Bootstrap: config.BootstrapConfig{
			OrgName:    "Acme Corp",
			OrgSlug:    "acme-corp",
			AdminEmail: "ops@acme.example",
		},
	}

	if _, err := Bootstrap(context.Background(), sqlDB, db.SQLiteDialect{},
		keyCache, cfg, bootstrapSecret, discardLogger()); err != nil {
		t.Fatalf("Bootstrap(): %v", err)
	}

	var orgName, orgSlug string
	if err := sqlDB.QueryRowContext(context.Background(),
		"SELECT name, slug FROM organizations WHERE deleted_at IS NULL").
		Scan(&orgName, &orgSlug); err != nil {
		t.Fatalf("query organization: %v", err)
	}
	if orgName != "Acme Corp" {
		t.Errorf("org name = %q, want %q", orgName, "Acme Corp")
	}
	if orgSlug != "acme-corp" {
		t.Errorf("org slug = %q, want %q", orgSlug, "acme-corp")
	}

	var email string
	if err := sqlDB.QueryRowContext(context.Background(),
		"SELECT email FROM users WHERE deleted_at IS NULL").Scan(&email); err != nil {
		t.Fatalf("query user: %v", err)
	}
	if email != "ops@acme.example" {
		t.Errorf("user email = %q, want %q", email, "ops@acme.example")
	}
}
