package db

// Regression suite for #172: soft-deleted rows previously held their UNIQUE
// column value hostage forever, because the column mangling that frees the
// value for reuse (DeleteModel/DeleteUser/DeleteOrg/DeleteTeam/DeleteDeployment)
// and migration 0015 (which mangles any already soft-deleted legacy rows) had
// not shipped yet. Each entity below is exercised through the same four
// assertions:
//
//  1. create -> soft-delete -> create again with the same unique value succeeds.
//  2. the deleted row's unique column is stored as value + ":deleted:" + id.
//  3. the active lookup by the original value finds the new row, not the old one.
//  4. deleting the second (new) row also succeeds -- two deleted rows sharing
//     the same original value, but different ids, must not collide with
//     each other either.

import (
	"context"
	"errors"
	"testing"
)

// rawColumnValue reads a single column's current stored value for the row
// identified by id, bypassing all "deleted_at IS NULL" filtering that the
// store's normal getters apply. It is used to assert on the raw, possibly
// tombstone-mangled, value written by a soft-delete.
func rawColumnValue(t *testing.T, d *DB, table, column, id string) string {
	t.Helper()
	var got string
	query := "SELECT " + column + " FROM " + table + " WHERE id = ?"
	if err := d.sql.QueryRowContext(context.Background(), query, id).Scan(&got); err != nil {
		t.Fatalf("rawColumnValue(%s.%s, id=%s): %v", table, column, id, err)
	}
	return got
}

// ---- models.name -----------------------------------------------------------

func TestDeleteModel_FreesNameForReuse(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	first := mustCreateModel(t, d, "reuse-model")
	if err := d.DeleteModel(ctx, first.ID); err != nil {
		t.Fatalf("DeleteModel(first): %v", err)
	}

	second, err := d.CreateModel(ctx, CreateModelParams{
		Name:     "reuse-model",
		Provider: "openai",
		BaseURL:  "https://api.openai.com/v1",
		Source:   "api",
	})
	if err != nil {
		t.Fatalf("CreateModel() reusing name after delete = %v, want nil (#172)", err)
	}
	if second.ID == first.ID {
		t.Error("second.ID == first.ID, want a newly created, distinct row")
	}
	if second.Name != "reuse-model" {
		t.Errorf("second.Name = %q, want %q", second.Name, "reuse-model")
	}
}

func TestDeleteModel_MangledNameHasTombstoneSuffix(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	m := mustCreateModel(t, d, "mangle-model")
	if err := d.DeleteModel(ctx, m.ID); err != nil {
		t.Fatalf("DeleteModel(): %v", err)
	}

	got := rawColumnValue(t, d, "models", "name", m.ID)
	want := "mangle-model:deleted:" + m.ID
	if got != want {
		t.Errorf("stored name after delete = %q, want %q", got, want)
	}
	if stripped := StripTombstone(got, m.ID); stripped != "mangle-model" {
		t.Errorf("StripTombstone(%q, %q) = %q, want %q", got, m.ID, stripped, "mangle-model")
	}
}

func TestGetModelByName_AfterDeleteAndRecreate_FindsNewRow(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	old := mustCreateModel(t, d, "lookup-reuse-model")
	if err := d.DeleteModel(ctx, old.ID); err != nil {
		t.Fatalf("DeleteModel(old): %v", err)
	}
	fresh := mustCreateModel(t, d, "lookup-reuse-model")

	got, err := d.GetModelByName(ctx, "lookup-reuse-model")
	if err != nil {
		t.Fatalf("GetModelByName(): %v", err)
	}
	if got.ID != fresh.ID {
		t.Errorf("GetModelByName().ID = %q, want %q (the new row)", got.ID, fresh.ID)
	}
	if got.ID == old.ID {
		t.Error("GetModelByName() returned the soft-deleted row")
	}
}

func TestDeleteModel_TwoDeletedRowsSameOriginalName_DoNotCollide(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	first := mustCreateModel(t, d, "double-delete-model")
	if err := d.DeleteModel(ctx, first.ID); err != nil {
		t.Fatalf("first DeleteModel(): %v", err)
	}

	second := mustCreateModel(t, d, "double-delete-model")
	if err := d.DeleteModel(ctx, second.ID); err != nil {
		t.Fatalf("second DeleteModel() = %v, want nil (#172: deleting a second row with the same original name must not collide with the first tombstoned row)", err)
	}

	firstName := rawColumnValue(t, d, "models", "name", first.ID)
	secondName := rawColumnValue(t, d, "models", "name", second.ID)
	if firstName == secondName {
		t.Fatalf("both deleted rows share identical mangled name %q, want distinct tombstone suffixes", firstName)
	}
	if want := "double-delete-model:deleted:" + first.ID; firstName != want {
		t.Errorf("first mangled name = %q, want %q", firstName, want)
	}
	if want := "double-delete-model:deleted:" + second.ID; secondName != want {
		t.Errorf("second mangled name = %q, want %q", secondName, want)
	}
}

// ---- users.email ------------------------------------------------------------

func TestDeleteUser_FreesEmailForReuse(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	first := mustCreateUser(t, d, CreateUserParams{
		Email:        "reuse@example.com",
		DisplayName:  "First",
		PasswordHash: testPasswordHash(t),
	})
	if err := d.DeleteUser(ctx, first.ID); err != nil {
		t.Fatalf("DeleteUser(first): %v", err)
	}

	second, err := d.CreateUser(ctx, CreateUserParams{
		Email:        "reuse@example.com",
		DisplayName:  "Second",
		PasswordHash: testPasswordHash(t),
	})
	if err != nil {
		t.Fatalf("CreateUser() reusing email after delete = %v, want nil (#172)", err)
	}
	if second.ID == first.ID {
		t.Error("second.ID == first.ID, want a newly created, distinct row")
	}
	if second.Email != "reuse@example.com" {
		t.Errorf("second.Email = %q, want %q", second.Email, "reuse@example.com")
	}
}

func TestDeleteUser_MangledEmailHasTombstoneSuffix(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	u := mustCreateUser(t, d, CreateUserParams{
		Email:        "mangle@example.com",
		DisplayName:  "Mangle Me",
		PasswordHash: testPasswordHash(t),
	})
	if err := d.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("DeleteUser(): %v", err)
	}

	got := rawColumnValue(t, d, "users", "email", u.ID)
	want := "mangle@example.com:deleted:" + u.ID
	if got != want {
		t.Errorf("stored email after delete = %q, want %q", got, want)
	}
	if stripped := StripTombstone(got, u.ID); stripped != "mangle@example.com" {
		t.Errorf("StripTombstone(%q, %q) = %q, want %q", got, u.ID, stripped, "mangle@example.com")
	}
}

func TestGetUserByEmail_AfterDeleteAndRecreate_FindsNewRow(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	old := mustCreateUser(t, d, CreateUserParams{
		Email:        "lookup-reuse@example.com",
		DisplayName:  "Old",
		PasswordHash: testPasswordHash(t),
	})
	if err := d.DeleteUser(ctx, old.ID); err != nil {
		t.Fatalf("DeleteUser(old): %v", err)
	}
	fresh := mustCreateUser(t, d, CreateUserParams{
		Email:        "lookup-reuse@example.com",
		DisplayName:  "Fresh",
		PasswordHash: testPasswordHash(t),
	})

	got, err := d.GetUserByEmail(ctx, "lookup-reuse@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail(): %v", err)
	}
	if got.ID != fresh.ID {
		t.Errorf("GetUserByEmail().ID = %q, want %q (the new row)", got.ID, fresh.ID)
	}
	if got.ID == old.ID {
		t.Error("GetUserByEmail() returned the soft-deleted row")
	}
}

func TestDeleteUser_TwoDeletedRowsSameOriginalEmail_DoNotCollide(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	first := mustCreateUser(t, d, CreateUserParams{
		Email:        "double-delete@example.com",
		DisplayName:  "First",
		PasswordHash: testPasswordHash(t),
	})
	if err := d.DeleteUser(ctx, first.ID); err != nil {
		t.Fatalf("first DeleteUser(): %v", err)
	}

	second := mustCreateUser(t, d, CreateUserParams{
		Email:        "double-delete@example.com",
		DisplayName:  "Second",
		PasswordHash: testPasswordHash(t),
	})
	if err := d.DeleteUser(ctx, second.ID); err != nil {
		t.Fatalf("second DeleteUser() = %v, want nil (#172: deleting a second row with the same original email must not collide)", err)
	}

	firstEmail := rawColumnValue(t, d, "users", "email", first.ID)
	secondEmail := rawColumnValue(t, d, "users", "email", second.ID)
	if firstEmail == secondEmail {
		t.Fatalf("both deleted rows share identical mangled email %q, want distinct tombstone suffixes", firstEmail)
	}
	if want := "double-delete@example.com:deleted:" + first.ID; firstEmail != want {
		t.Errorf("first mangled email = %q, want %q", firstEmail, want)
	}
	if want := "double-delete@example.com:deleted:" + second.ID; secondEmail != want {
		t.Errorf("second mangled email = %q, want %q", secondEmail, want)
	}
}

// ---- OIDC identity: (auth_provider, external_id) -----------------------------

// TestDeleteUser_OIDCIdentityReusableAfterDelete covers the idx_users_external
// half of #172: before migration 0015 added a "deleted_at IS NULL" predicate to
// that index, a soft-deleted OIDC identity permanently blocked re-provisioning
// the same (auth_provider, external_id) pair for a new user.
func TestDeleteUser_OIDCIdentityReusableAfterDelete(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	extID := "oidc-sub-reuse-172"
	first, err := d.CreateUser(ctx, CreateUserParams{
		Email:        "oidc-first@example.com",
		DisplayName:  "OIDC First",
		AuthProvider: "oidc",
		ExternalID:   &extID,
	})
	if err != nil {
		t.Fatalf("CreateUser(first): %v", err)
	}
	if err := d.DeleteUser(ctx, first.ID); err != nil {
		t.Fatalf("DeleteUser(first): %v", err)
	}

	second, err := d.CreateUser(ctx, CreateUserParams{
		Email:        "oidc-second@example.com",
		DisplayName:  "OIDC Second",
		AuthProvider: "oidc",
		ExternalID:   &extID,
	})
	if err != nil {
		t.Fatalf("CreateUser(second) with the same auth_provider+external_id after delete = %v, want nil (#172)", err)
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

// ---- organizations.slug -------------------------------------------------------

func TestDeleteOrg_FreesSlugForReuse(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	first := mustCreateOrg(t, d, CreateOrgParams{Name: "First Org", Slug: "reuse-org"})
	if err := d.DeleteOrg(ctx, first.ID); err != nil {
		t.Fatalf("DeleteOrg(first): %v", err)
	}

	second, err := d.CreateOrg(ctx, CreateOrgParams{Name: "Second Org", Slug: "reuse-org"})
	if err != nil {
		t.Fatalf("CreateOrg() reusing slug after delete = %v, want nil (#172)", err)
	}
	if second.ID == first.ID {
		t.Error("second.ID == first.ID, want a newly created, distinct row")
	}
	if second.Slug != "reuse-org" {
		t.Errorf("second.Slug = %q, want %q", second.Slug, "reuse-org")
	}
}

func TestDeleteOrg_MangledSlugHasTombstoneSuffix(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Mangle Org", Slug: "mangle-org"})
	if err := d.DeleteOrg(ctx, org.ID); err != nil {
		t.Fatalf("DeleteOrg(): %v", err)
	}

	got := rawColumnValue(t, d, "organizations", "slug", org.ID)
	want := "mangle-org:deleted:" + org.ID
	if got != want {
		t.Errorf("stored slug after delete = %q, want %q", got, want)
	}
	if stripped := StripTombstone(got, org.ID); stripped != "mangle-org" {
		t.Errorf("StripTombstone(%q, %q) = %q, want %q", got, org.ID, stripped, "mangle-org")
	}
}

func TestGetOrgBySlug_AfterDeleteAndRecreate_FindsNewRow(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	old := mustCreateOrg(t, d, CreateOrgParams{Name: "Old Org", Slug: "lookup-reuse-org"})
	if err := d.DeleteOrg(ctx, old.ID); err != nil {
		t.Fatalf("DeleteOrg(old): %v", err)
	}
	fresh := mustCreateOrg(t, d, CreateOrgParams{Name: "Fresh Org", Slug: "lookup-reuse-org"})

	got, err := d.GetOrgBySlug(ctx, "lookup-reuse-org")
	if err != nil {
		t.Fatalf("GetOrgBySlug(): %v", err)
	}
	if got.ID != fresh.ID {
		t.Errorf("GetOrgBySlug().ID = %q, want %q (the new row)", got.ID, fresh.ID)
	}
	if got.ID == old.ID {
		t.Error("GetOrgBySlug() returned the soft-deleted row")
	}
}

func TestDeleteOrg_TwoDeletedRowsSameOriginalSlug_DoNotCollide(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	first := mustCreateOrg(t, d, CreateOrgParams{Name: "First", Slug: "double-delete-org"})
	if err := d.DeleteOrg(ctx, first.ID); err != nil {
		t.Fatalf("first DeleteOrg(): %v", err)
	}

	second := mustCreateOrg(t, d, CreateOrgParams{Name: "Second", Slug: "double-delete-org"})
	if err := d.DeleteOrg(ctx, second.ID); err != nil {
		t.Fatalf("second DeleteOrg() = %v, want nil (#172: deleting a second row with the same original slug must not collide)", err)
	}

	firstSlug := rawColumnValue(t, d, "organizations", "slug", first.ID)
	secondSlug := rawColumnValue(t, d, "organizations", "slug", second.ID)
	if firstSlug == secondSlug {
		t.Fatalf("both deleted rows share identical mangled slug %q, want distinct tombstone suffixes", firstSlug)
	}
	if want := "double-delete-org:deleted:" + first.ID; firstSlug != want {
		t.Errorf("first mangled slug = %q, want %q", firstSlug, want)
	}
	if want := "double-delete-org:deleted:" + second.ID; secondSlug != want {
		t.Errorf("second mangled slug = %q, want %q", secondSlug, want)
	}
}

// ---- teams.slug (scoped to org_id) --------------------------------------------

func TestDeleteTeam_FreesSlugForReuse(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Team Reuse Org", Slug: "team-reuse-org"})

	first := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "First Team", Slug: "reuse-team"})
	if err := d.DeleteTeam(ctx, first.ID); err != nil {
		t.Fatalf("DeleteTeam(first): %v", err)
	}

	second, err := d.CreateTeam(ctx, CreateTeamParams{OrgID: org.ID, Name: "Second Team", Slug: "reuse-team"})
	if err != nil {
		t.Fatalf("CreateTeam() reusing slug after delete = %v, want nil (#172)", err)
	}
	if second.ID == first.ID {
		t.Error("second.ID == first.ID, want a newly created, distinct row")
	}
	if second.Slug != "reuse-team" {
		t.Errorf("second.Slug = %q, want %q", second.Slug, "reuse-team")
	}
}

func TestDeleteTeam_MangledSlugHasTombstoneSuffix(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Mangle Team Org", Slug: "mangle-team-org"})
	team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Mangle Team", Slug: "mangle-team"})
	if err := d.DeleteTeam(ctx, team.ID); err != nil {
		t.Fatalf("DeleteTeam(): %v", err)
	}

	got := rawColumnValue(t, d, "teams", "slug", team.ID)
	want := "mangle-team:deleted:" + team.ID
	if got != want {
		t.Errorf("stored slug after delete = %q, want %q", got, want)
	}
	if stripped := StripTombstone(got, team.ID); stripped != "mangle-team" {
		t.Errorf("StripTombstone(%q, %q) = %q, want %q", got, team.ID, stripped, "mangle-team")
	}
}

func TestGetTeamByName_AfterDeleteAndRecreateWithSameSlug_FindsNewRow(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Lookup Reuse Team Org", Slug: "lookup-reuse-team-org"})

	old := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Old Team", Slug: "lookup-reuse-team"})
	if err := d.DeleteTeam(ctx, old.ID); err != nil {
		t.Fatalf("DeleteTeam(old): %v", err)
	}
	fresh, err := d.CreateTeam(ctx, CreateTeamParams{OrgID: org.ID, Name: "Old Team", Slug: "lookup-reuse-team"})
	if err != nil {
		t.Fatalf("CreateTeam(fresh) reusing slug: %v", err)
	}

	// GetTeamByName looks up by (org_id, name), which is not the mangled
	// column, but the row's continued reachability by its active lookup
	// confirms the delete/recreate cycle left a healthy, findable new row.
	got, err := d.GetTeamByName(ctx, org.ID, "Old Team")
	if err != nil {
		t.Fatalf("GetTeamByName(): %v", err)
	}
	if got.ID != fresh.ID {
		t.Errorf("GetTeamByName().ID = %q, want %q (the new row)", got.ID, fresh.ID)
	}
	if got.ID == old.ID {
		t.Error("GetTeamByName() returned the soft-deleted row")
	}
	if got.Slug != "lookup-reuse-team" {
		t.Errorf("GetTeamByName().Slug = %q, want %q", got.Slug, "lookup-reuse-team")
	}
}

func TestDeleteTeam_TwoDeletedRowsSameOriginalSlug_DoNotCollide(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Double Delete Team Org", Slug: "double-delete-team-org"})

	first := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "First", Slug: "double-delete-team"})
	if err := d.DeleteTeam(ctx, first.ID); err != nil {
		t.Fatalf("first DeleteTeam(): %v", err)
	}

	second := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "Second", Slug: "double-delete-team"})
	if err := d.DeleteTeam(ctx, second.ID); err != nil {
		t.Fatalf("second DeleteTeam() = %v, want nil (#172: deleting a second row with the same original slug must not collide)", err)
	}

	firstSlug := rawColumnValue(t, d, "teams", "slug", first.ID)
	secondSlug := rawColumnValue(t, d, "teams", "slug", second.ID)
	if firstSlug == secondSlug {
		t.Fatalf("both deleted rows share identical mangled slug %q, want distinct tombstone suffixes", firstSlug)
	}
	if want := "double-delete-team:deleted:" + first.ID; firstSlug != want {
		t.Errorf("first mangled slug = %q, want %q", firstSlug, want)
	}
	if want := "double-delete-team:deleted:" + second.ID; secondSlug != want {
		t.Errorf("second mangled slug = %q, want %q", secondSlug, want)
	}
}

// ---- model_deployments.name (scoped to model_id) -------------------------------

func TestDeleteDeployment_FreesNameForReuse(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	m := mustCreateModel(t, d, "deployment-reuse-model")

	first := mustCreateDeployment(t, d, CreateDeploymentParams{
		ModelID: m.ID, Name: "reuse-dep", Provider: "openai", BaseURL: "https://api.openai.com/v1", Weight: 1,
	})
	if err := d.DeleteDeployment(ctx, first.ID); err != nil {
		t.Fatalf("DeleteDeployment(first): %v", err)
	}

	second, err := d.CreateDeployment(ctx, CreateDeploymentParams{
		ModelID: m.ID, Name: "reuse-dep", Provider: "openai", BaseURL: "https://api.openai.com/v1", Weight: 1,
	})
	if err != nil {
		t.Fatalf("CreateDeployment() reusing name after delete = %v, want nil (#172)", err)
	}
	if second.ID == first.ID {
		t.Error("second.ID == first.ID, want a newly created, distinct row")
	}
	if second.Name != "reuse-dep" {
		t.Errorf("second.Name = %q, want %q", second.Name, "reuse-dep")
	}
}

func TestDeleteDeployment_MangledNameHasTombstoneSuffix(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	m := mustCreateModel(t, d, "deployment-mangle-model")
	dep := mustCreateDeployment(t, d, CreateDeploymentParams{
		ModelID: m.ID, Name: "mangle-dep", Provider: "openai", BaseURL: "https://api.openai.com/v1", Weight: 1,
	})
	if err := d.DeleteDeployment(ctx, dep.ID); err != nil {
		t.Fatalf("DeleteDeployment(): %v", err)
	}

	got := rawColumnValue(t, d, "model_deployments", "name", dep.ID)
	want := "mangle-dep:deleted:" + dep.ID
	if got != want {
		t.Errorf("stored name after delete = %q, want %q", got, want)
	}
	if stripped := StripTombstone(got, dep.ID); stripped != "mangle-dep" {
		t.Errorf("StripTombstone(%q, %q) = %q, want %q", got, dep.ID, stripped, "mangle-dep")
	}
}

func TestListDeployments_AfterDeleteAndRecreateWithSameName_FindsNewRow(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	m := mustCreateModel(t, d, "deployment-lookup-reuse-model")

	old := mustCreateDeployment(t, d, CreateDeploymentParams{
		ModelID: m.ID, Name: "lookup-reuse-dep", Provider: "openai", BaseURL: "https://api.openai.com/v1", Weight: 1,
	})
	if err := d.DeleteDeployment(ctx, old.ID); err != nil {
		t.Fatalf("DeleteDeployment(old): %v", err)
	}
	fresh := mustCreateDeployment(t, d, CreateDeploymentParams{
		ModelID: m.ID, Name: "lookup-reuse-dep", Provider: "openai", BaseURL: "https://api.openai.com/v1", Weight: 1,
	})

	deps, err := d.ListDeployments(ctx, m.ID)
	if err != nil {
		t.Fatalf("ListDeployments(): %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("ListDeployments() len = %d, want 1 (only the active, renamed-back row)", len(deps))
	}
	if deps[0].ID != fresh.ID {
		t.Errorf("ListDeployments()[0].ID = %q, want %q (the new row)", deps[0].ID, fresh.ID)
	}
	if deps[0].ID == old.ID {
		t.Error("ListDeployments() returned the soft-deleted row")
	}
	if deps[0].Name != "lookup-reuse-dep" {
		t.Errorf("ListDeployments()[0].Name = %q, want %q", deps[0].Name, "lookup-reuse-dep")
	}

	got, err := d.GetDeployment(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("GetDeployment(fresh): %v", err)
	}
	if got.Name != "lookup-reuse-dep" {
		t.Errorf("GetDeployment(fresh).Name = %q, want %q", got.Name, "lookup-reuse-dep")
	}
}

func TestDeleteDeployment_TwoDeletedRowsSameOriginalName_DoNotCollide(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	m := mustCreateModel(t, d, "deployment-double-delete-model")

	first := mustCreateDeployment(t, d, CreateDeploymentParams{
		ModelID: m.ID, Name: "double-delete-dep", Provider: "openai", BaseURL: "https://api.openai.com/v1", Weight: 1,
	})
	if err := d.DeleteDeployment(ctx, first.ID); err != nil {
		t.Fatalf("first DeleteDeployment(): %v", err)
	}

	second := mustCreateDeployment(t, d, CreateDeploymentParams{
		ModelID: m.ID, Name: "double-delete-dep", Provider: "openai", BaseURL: "https://api.openai.com/v1", Weight: 1,
	})
	if err := d.DeleteDeployment(ctx, second.ID); err != nil {
		t.Fatalf("second DeleteDeployment() = %v, want nil (#172: deleting a second row with the same original name must not collide)", err)
	}

	firstName := rawColumnValue(t, d, "model_deployments", "name", first.ID)
	secondName := rawColumnValue(t, d, "model_deployments", "name", second.ID)
	if firstName == secondName {
		t.Fatalf("both deleted rows share identical mangled name %q, want distinct tombstone suffixes", firstName)
	}
	if want := "double-delete-dep:deleted:" + first.ID; firstName != want {
		t.Errorf("first mangled name = %q, want %q", firstName, want)
	}
	if want := "double-delete-dep:deleted:" + second.ID; secondName != want {
		t.Errorf("second mangled name = %q, want %q", secondName, want)
	}
}

// ---- sanity: not-yet-deleted errors are unaffected ----------------------------

// TestDeleteModel_AlreadyDeleted_StillReturnsErrNotFound guards against a
// regression where the mangling UPDATE's "WHERE deleted_at IS NULL" clause
// might accidentally be dropped, which would let a double-delete silently
// re-mangle an already-tombstoned name a second time instead of returning
// ErrNotFound.
func TestDeleteModel_AlreadyDeleted_StillReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	m := mustCreateModel(t, d, "already-deleted-model")
	if err := d.DeleteModel(ctx, m.ID); err != nil {
		t.Fatalf("first DeleteModel(): %v", err)
	}
	nameAfterFirstDelete := rawColumnValue(t, d, "models", "name", m.ID)

	err := d.DeleteModel(ctx, m.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("second DeleteModel() error = %v, want ErrNotFound", err)
	}

	nameAfterSecondAttempt := rawColumnValue(t, d, "models", "name", m.ID)
	if nameAfterSecondAttempt != nameAfterFirstDelete {
		t.Errorf("name changed after a no-op double delete: %q -> %q, want unchanged", nameAfterFirstDelete, nameAfterSecondAttempt)
	}
}
