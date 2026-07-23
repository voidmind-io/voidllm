package db

// Migration 0015 pre-seeded data test.
//
// The chosen approach, and why: RunMigrations (see migrate.go) always applies
// every embedded "*.up.sql" file in order and has no "stop at migration N"
// mode, and openMigratedDB (the shared test harness used across this package,
// see orgs_test.go) always runs it to completion. So there is no way to open a
// test database frozen just *before* 0015 runs in order to seed genuine
// pre-fix rows and then watch 0015 mangle them live during RunMigrations
// itself -- by the time any test gets a *DB, 0015 has already executed against
// an empty schema and had nothing to mangle.
//
// Rather than fabricate a green test that never actually exercises 0015's SQL
// (e.g. asserting today's fixed behavior only through the store's Delete*
// methods, which is already covered by soft_delete_collision_test.go and
// proves the *ongoing* fix but not the *backfill* for existing installs), the
// tests below take the honest option available in this harness: they seed
// rows shaped exactly like a pre-#172 installation would have had -- a
// soft-deleted row whose unique column still holds its original, unmangled
// value, and (for the OIDC case) a unique index without the deleted_at
// predicate -- directly via raw SQL against an already-fully-migrated
// database, confirm that shape reproduces the original bug, then re-execute
// migration 0015's own up.sql content (read from disk, not copy-pasted, so
// this test cannot silently drift from the real migration) against that
// seeded data and confirm it fixes it. This exercises 0015's actual
// UPDATE/index-rebuild logic against representative legacy data.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
)

// migration0015UpSQL reads the exact SQL executed by
// migrations/0015_soft_delete_unique_collision.up.sql from disk.
func migration0015UpSQL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("migrations/0015_soft_delete_unique_collision.up.sql")
	if err != nil {
		t.Fatalf("read migration 0015 up.sql: %v", err)
	}
	return string(b)
}

// applyMigration0015SQL re-executes migration 0015's up.sql content against
// an already-fully-migrated test database, inside a transaction, mirroring
// how the real migration runner applies it (see applyMigration in migrate.go).
func applyMigration0015SQL(t *testing.T, d *DB) {
	t.Helper()
	ctx := context.Background()
	err := d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx, migration0015UpSQL(t))
		return execErr
	})
	if err != nil {
		t.Fatalf("re-apply migration 0015 SQL: %v", err)
	}
}

// TestMigration0015_MangleLegacyDeletedRows_Models seeds a soft-deleted models
// row with an unmangled name -- exactly the shape left behind by any
// installation that soft-deleted a model before #172 was fixed -- and
// verifies that migration 0015's UPDATE statement mangles it and frees the
// name for reuse.
func TestMigration0015_MangleLegacyDeletedRows_Models(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	legacyID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	_, err = d.sql.ExecContext(ctx,
		"INSERT INTO models (id, name, provider, base_url, is_active, source, created_at, updated_at, deleted_at) "+
			"VALUES (?, ?, 'openai', 'https://api.openai.com/v1', 1, 'api', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)",
		legacyID.String(), "legacy-conflict-model",
	)
	if err != nil {
		t.Fatalf("seed legacy soft-deleted row: %v", err)
	}

	// Sanity check: reproduce the pre-0015 bug. models.name is an inline
	// UNIQUE constraint (not a partial index), so a soft-deleted row with an
	// unmangled name still blocks creating a new active row with that name.
	if _, err := d.CreateModel(ctx, CreateModelParams{
		Name: "legacy-conflict-model", Provider: "openai", BaseURL: "https://api.openai.com/v1", Source: "api",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("CreateModel() before migration 0015 mangles the legacy row = %v, want ErrConflict (sanity check)", err)
	}

	applyMigration0015SQL(t, d)

	got := rawColumnValue(t, d, "models", "name", legacyID.String())
	want := "legacy-conflict-model:deleted:" + legacyID.String()
	if got != want {
		t.Errorf("legacy row name after migration 0015 = %q, want %q", got, want)
	}

	if _, err := d.CreateModel(ctx, CreateModelParams{
		Name: "legacy-conflict-model", Provider: "openai", BaseURL: "https://api.openai.com/v1", Source: "api",
	}); err != nil {
		t.Errorf("CreateModel() after migration 0015 mangled the legacy row = %v, want nil", err)
	}
}

// TestMigration0015_MangleLegacyDeletedRows_Users mirrors the models case for
// users.email.
func TestMigration0015_MangleLegacyDeletedRows_Users(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	legacyID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	_, err = d.sql.ExecContext(ctx,
		"INSERT INTO users (id, email, display_name, auth_provider, is_system_admin, created_at, updated_at, deleted_at) "+
			"VALUES (?, ?, 'Legacy User', 'local', 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)",
		legacyID.String(), "legacy-conflict@example.com",
	)
	if err != nil {
		t.Fatalf("seed legacy soft-deleted row: %v", err)
	}

	_, err = d.CreateUser(ctx, CreateUserParams{
		Email:        "legacy-conflict@example.com",
		DisplayName:  "New User",
		PasswordHash: testPasswordHash(t),
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("CreateUser() before migration 0015 mangles the legacy row = %v, want ErrConflict (sanity check)", err)
	}

	applyMigration0015SQL(t, d)

	got := rawColumnValue(t, d, "users", "email", legacyID.String())
	want := "legacy-conflict@example.com:deleted:" + legacyID.String()
	if got != want {
		t.Errorf("legacy row email after migration 0015 = %q, want %q", got, want)
	}

	if _, err := d.CreateUser(ctx, CreateUserParams{
		Email:        "legacy-conflict@example.com",
		DisplayName:  "New User",
		PasswordHash: testPasswordHash(t),
	}); err != nil {
		t.Errorf("CreateUser() after migration 0015 mangled the legacy row = %v, want nil", err)
	}
}

// TestMigration0015_MangleLegacyDeletedRows_Teams mirrors the models case for
// teams.slug, which is scoped to (org_id, slug) rather than a single global
// column.
func TestMigration0015_MangleLegacyDeletedRows_Teams(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Legacy Team Org", Slug: "legacy-team-org"})

	legacyID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	_, err = d.sql.ExecContext(ctx,
		"INSERT INTO teams (id, org_id, name, slug, created_at, updated_at, deleted_at) "+
			"VALUES (?, ?, 'Legacy Team', ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)",
		legacyID.String(), org.ID, "legacy-conflict-team",
	)
	if err != nil {
		t.Fatalf("seed legacy soft-deleted row: %v", err)
	}

	_, err = d.CreateTeam(ctx, CreateTeamParams{OrgID: org.ID, Name: "New Team", Slug: "legacy-conflict-team"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("CreateTeam() before migration 0015 mangles the legacy row = %v, want ErrConflict (sanity check)", err)
	}

	applyMigration0015SQL(t, d)

	got := rawColumnValue(t, d, "teams", "slug", legacyID.String())
	want := "legacy-conflict-team:deleted:" + legacyID.String()
	if got != want {
		t.Errorf("legacy row slug after migration 0015 = %q, want %q", got, want)
	}

	if _, err := d.CreateTeam(ctx, CreateTeamParams{OrgID: org.ID, Name: "New Team", Slug: "legacy-conflict-team"}); err != nil {
		t.Errorf("CreateTeam() after migration 0015 mangled the legacy row = %v, want nil", err)
	}
}

// TestMigration0015_RebuildsUserExternalIndexForLegacyInstalls covers the
// idx_users_external half of migration 0015. Unlike the mangled-column cases
// above, idx_users_external is a *named* index rather than an inline
// constraint, so a legacy install's index (without the "deleted_at IS NULL"
// predicate added by 0015) is reproduced here by dropping the current,
// already-correct index and recreating it in its original 0001 shape, then
// confirming migration 0015's SQL restores the fix.
func TestMigration0015_RebuildsUserExternalIndexForLegacyInstalls(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	// Roll back idx_users_external to its pre-0015 (0001-original) shape.
	err := d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx,
			"DROP INDEX idx_users_external; "+
				"CREATE UNIQUE INDEX idx_users_external ON users (auth_provider, external_id) WHERE external_id IS NOT NULL;")
		return execErr
	})
	if err != nil {
		t.Fatalf("recreate legacy idx_users_external: %v", err)
	}

	extID := "legacy-oidc-sub-172"
	first, err := d.CreateUser(ctx, CreateUserParams{
		Email:        "legacy-oidc-first@example.com",
		DisplayName:  "Legacy OIDC First",
		AuthProvider: "oidc",
		ExternalID:   &extID,
	})
	if err != nil {
		t.Fatalf("CreateUser(first): %v", err)
	}
	if err := d.DeleteUser(ctx, first.ID); err != nil {
		t.Fatalf("DeleteUser(first): %v", err)
	}

	// Sanity check: reproduce the pre-0015 bug. The legacy full-column
	// unique index still blocks re-provisioning the same OIDC identity.
	_, err = d.CreateUser(ctx, CreateUserParams{
		Email:        "legacy-oidc-second@example.com",
		DisplayName:  "Legacy OIDC Second",
		AuthProvider: "oidc",
		ExternalID:   &extID,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("CreateUser() under the legacy index = %v, want ErrConflict (sanity check)", err)
	}

	applyMigration0015SQL(t, d)

	second, err := d.CreateUser(ctx, CreateUserParams{
		Email:        "legacy-oidc-second@example.com",
		DisplayName:  "Legacy OIDC Second",
		AuthProvider: "oidc",
		ExternalID:   &extID,
	})
	if err != nil {
		t.Fatalf("CreateUser() after migration 0015 rebuilt idx_users_external = %v, want nil", err)
	}
	if second.ID == first.ID {
		t.Error("second.ID == first.ID, want a newly created, distinct row")
	}

	got, err := d.GetUserByExternalID(ctx, "oidc", extID)
	if err != nil {
		t.Fatalf("GetUserByExternalID(): %v", err)
	}
	if got.ID != second.ID {
		t.Errorf("GetUserByExternalID().ID = %q, want %q (the new row)", got.ID, second.ID)
	}
}
