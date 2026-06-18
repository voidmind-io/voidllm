package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// ---- helpers -----------------------------------------------------------------

// seedOrgForLabels creates an org and returns its ID and name.
func seedOrgForLabels(t *testing.T, d *DB, name, slug string) (id, label string) {
	t.Helper()
	org := mustCreateOrg(t, d, CreateOrgParams{Name: name, Slug: slug})
	return org.ID, org.Name
}

// seedUserForLabels creates a user and returns its ID and display_name.
func seedUserForLabels(t *testing.T, d *DB, email, displayName string) (id, label string) {
	t.Helper()
	user := mustCreateUser(t, d, CreateUserParams{Email: email, DisplayName: displayName})
	return user.ID, user.DisplayName
}

// seedTeamForLabels creates a team inside org and returns its ID and name.
func seedTeamForLabels(t *testing.T, d *DB, orgID, name, slug string) (id, label string) {
	t.Helper()
	team := mustCreateTeam(t, d, CreateTeamParams{OrgID: orgID, Name: name, Slug: slug})
	return team.ID, team.Name
}

// seedCreatorUser creates a minimal user to satisfy api_keys.created_by FK.
// It uses the email as the unique discriminator so multiple calls per test
// with the same email are idempotent at the DB level.
func seedCreatorUser(t *testing.T, d *DB, email string) string {
	t.Helper()
	user := mustCreateUser(t, d, CreateUserParams{Email: email, DisplayName: "creator"})
	return user.ID
}

// seedAPIKeyForLabels creates an API key and returns its ID, plus the expected
// label (name if non-empty, else hint). createdBy must be a valid user ID.
func seedAPIKeyForLabels(t *testing.T, d *DB, orgID, keyName, createdBy string) (id, label string) {
	t.Helper()
	plaintext := "vl_uk_deadbeefdeadbeefdeadbeefdeadbeefdeadbeef00"
	params := CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext+orgID+keyName, testHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeUser,
		Name:      keyName,
		OrgID:     orgID,
		CreatedBy: createdBy,
	}
	key := mustCreateAPIKey(t, d, params)
	if keyName != "" {
		label = keyName
	} else {
		label = keygen.Hint(plaintext)
	}
	return key.ID, label
}

// ---- ResolveGroupLabels — non-resolvable dimensions --------------------------

func TestResolveGroupLabels_NonResolvableDimensions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		groupBy string
	}{
		{name: "empty groupBy", groupBy: ""},
		{name: "model", groupBy: "model"},
		{name: "day", groupBy: "day"},
		{name: "hour", groupBy: "hour"},
		{name: "server", groupBy: "server"},
		{name: "tool", groupBy: "tool"},
		{name: "status", groupBy: "status"},
		{name: "unknown value", groupBy: "unknown_xyz"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := openMigratedDB(t)

			got, err := d.ResolveGroupLabels(context.Background(), tc.groupBy, []string{"any-id"})
			if err != nil {
				t.Fatalf("ResolveGroupLabels(%q) error = %v, want nil", tc.groupBy, err)
			}
			if got == nil {
				t.Fatalf("ResolveGroupLabels(%q) returned nil map, want empty non-nil map", tc.groupBy)
			}
			if len(got) != 0 {
				t.Errorf("ResolveGroupLabels(%q) len = %d, want 0; got: %v", tc.groupBy, len(got), got)
			}
		})
	}
}

// ---- ResolveGroupLabels — empty / blank id inputs ----------------------------

func TestResolveGroupLabels_EmptyIDsSlice(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)

	for _, groupBy := range []string{"key", "user", "team", "org"} {
		got, err := d.ResolveGroupLabels(context.Background(), groupBy, []string{})
		if err != nil {
			t.Fatalf("groupBy=%q empty ids error = %v, want nil", groupBy, err)
		}
		if got == nil {
			t.Fatalf("groupBy=%q returned nil map, want empty non-nil map", groupBy)
		}
		if len(got) != 0 {
			t.Errorf("groupBy=%q len = %d, want 0", groupBy, len(got))
		}
	}
}

func TestResolveGroupLabels_OnlyEmptyStringIDs(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)

	for _, groupBy := range []string{"key", "user", "team", "org"} {
		got, err := d.ResolveGroupLabels(context.Background(), groupBy, []string{"", "", ""})
		if err != nil {
			t.Fatalf("groupBy=%q all-empty ids error = %v, want nil", groupBy, err)
		}
		if got == nil {
			t.Fatalf("groupBy=%q returned nil map, want empty non-nil map", groupBy)
		}
		if len(got) != 0 {
			t.Errorf("groupBy=%q len = %d, want 0", groupBy, len(got))
		}
	}
}

// ---- ResolveGroupLabels — org dimension --------------------------------------

func TestResolveGroupLabels_Org(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) (ids []string, wantLabels map[string]string)
		wantErr bool
	}{
		{
			name: "single org resolves to name",
			setup: func(t *testing.T, d *DB) ([]string, map[string]string) {
				t.Helper()
				id, label := seedOrgForLabels(t, d, "Acme Corp", "acme-corp-label-1")
				return []string{id}, map[string]string{id: label}
			},
		},
		{
			name: "multiple orgs resolve correctly",
			setup: func(t *testing.T, d *DB) ([]string, map[string]string) {
				t.Helper()
				id1, label1 := seedOrgForLabels(t, d, "Alpha Org", "alpha-org-label-1")
				id2, label2 := seedOrgForLabels(t, d, "Beta Org", "beta-org-label-1")
				return []string{id1, id2}, map[string]string{id1: label1, id2: label2}
			},
		},
		{
			name: "non-existent id is absent from result",
			setup: func(t *testing.T, d *DB) ([]string, map[string]string) {
				t.Helper()
				id1, label1 := seedOrgForLabels(t, d, "Real Org", "real-org-label-1")
				ghost := "00000000-0000-0000-0000-000000000099"
				return []string{id1, ghost}, map[string]string{id1: label1}
			},
		},
		{
			name: "soft-deleted org still resolves",
			setup: func(t *testing.T, d *DB) ([]string, map[string]string) {
				t.Helper()
				id, label := seedOrgForLabels(t, d, "Deleted Org", "deleted-org-label-1")
				if err := d.DeleteOrg(context.Background(), id); err != nil {
					t.Fatalf("DeleteOrg: %v", err)
				}
				return []string{id}, map[string]string{id: label}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := openMigratedDB(t)
			ids, wantLabels := tc.setup(t, d)

			got, err := d.ResolveGroupLabels(context.Background(), "org", ids)
			if tc.wantErr {
				if err == nil {
					t.Fatal("ResolveGroupLabels() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveGroupLabels() error = %v, want nil", err)
			}
			if len(got) != len(wantLabels) {
				t.Fatalf("len(got) = %d, want %d; got: %v", len(got), len(wantLabels), got)
			}
			for id, wantLabel := range wantLabels {
				if got[id] != wantLabel {
					t.Errorf("got[%q] = %q, want %q", id, got[id], wantLabel)
				}
			}
		})
	}
}

// ---- ResolveGroupLabels — user dimension -------------------------------------

func TestResolveGroupLabels_User(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, d *DB) (ids []string, wantLabels map[string]string)
	}{
		{
			name: "single user resolves to display_name",
			setup: func(t *testing.T, d *DB) ([]string, map[string]string) {
				t.Helper()
				id, label := seedUserForLabels(t, d, "alice@example.com", "Alice Smith")
				return []string{id}, map[string]string{id: label}
			},
		},
		{
			name: "multiple users resolve correctly",
			setup: func(t *testing.T, d *DB) ([]string, map[string]string) {
				t.Helper()
				id1, label1 := seedUserForLabels(t, d, "bob@example.com", "Bob Jones")
				id2, label2 := seedUserForLabels(t, d, "carol@example.com", "Carol White")
				return []string{id1, id2}, map[string]string{id1: label1, id2: label2}
			},
		},
		{
			name: "non-existent user id is absent",
			setup: func(t *testing.T, d *DB) ([]string, map[string]string) {
				t.Helper()
				id, label := seedUserForLabels(t, d, "dave@example.com", "Dave Brown")
				ghost := "00000000-0000-0000-0000-000000000088"
				return []string{id, ghost}, map[string]string{id: label}
			},
		},
		{
			name: "soft-deleted user still resolves",
			setup: func(t *testing.T, d *DB) ([]string, map[string]string) {
				t.Helper()
				id, label := seedUserForLabels(t, d, "eve@example.com", "Eve Ghost")
				if err := d.DeleteUser(context.Background(), id); err != nil {
					t.Fatalf("DeleteUser: %v", err)
				}
				return []string{id}, map[string]string{id: label}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := openMigratedDB(t)
			ids, wantLabels := tc.setup(t, d)

			got, err := d.ResolveGroupLabels(context.Background(), "user", ids)
			if err != nil {
				t.Fatalf("ResolveGroupLabels(user) error = %v, want nil", err)
			}
			if len(got) != len(wantLabels) {
				t.Fatalf("len(got) = %d, want %d; got: %v", len(got), len(wantLabels), got)
			}
			for id, wantLabel := range wantLabels {
				if got[id] != wantLabel {
					t.Errorf("got[%q] = %q, want %q", id, got[id], wantLabel)
				}
			}
		})
	}
}

// ---- ResolveGroupLabels — team dimension -------------------------------------

func TestResolveGroupLabels_Team(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, d *DB) (ids []string, wantLabels map[string]string)
	}{
		{
			name: "single team resolves to name",
			setup: func(t *testing.T, d *DB) ([]string, map[string]string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-tm-single"})
				id, label := seedTeamForLabels(t, d, org.ID, "Engineering", "engineering-tm-single")
				return []string{id}, map[string]string{id: label}
			},
		},
		{
			name: "multiple teams resolve correctly",
			setup: func(t *testing.T, d *DB) ([]string, map[string]string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-tm-multi"})
				id1, label1 := seedTeamForLabels(t, d, org.ID, "Backend", "backend-tm-multi")
				id2, label2 := seedTeamForLabels(t, d, org.ID, "Frontend", "frontend-tm-multi")
				return []string{id1, id2}, map[string]string{id1: label1, id2: label2}
			},
		},
		{
			name: "non-existent team id is absent",
			setup: func(t *testing.T, d *DB) ([]string, map[string]string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-tm-ghost"})
				id, label := seedTeamForLabels(t, d, org.ID, "Platform", "platform-tm-ghost")
				ghost := "00000000-0000-0000-0000-000000000077"
				return []string{id, ghost}, map[string]string{id: label}
			},
		},
		{
			name: "soft-deleted team still resolves",
			setup: func(t *testing.T, d *DB) ([]string, map[string]string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-tm-deleted"})
				id, label := seedTeamForLabels(t, d, org.ID, "Disbanded Team", "disbanded-tm-deleted")
				if err := d.DeleteTeam(context.Background(), id); err != nil {
					t.Fatalf("DeleteTeam: %v", err)
				}
				return []string{id}, map[string]string{id: label}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := openMigratedDB(t)
			ids, wantLabels := tc.setup(t, d)

			got, err := d.ResolveGroupLabels(context.Background(), "team", ids)
			if err != nil {
				t.Fatalf("ResolveGroupLabels(team) error = %v, want nil", err)
			}
			if len(got) != len(wantLabels) {
				t.Fatalf("len(got) = %d, want %d; got: %v", len(got), len(wantLabels), got)
			}
			for id, wantLabel := range wantLabels {
				if got[id] != wantLabel {
					t.Errorf("got[%q] = %q, want %q", id, got[id], wantLabel)
				}
			}
		})
	}
}

// ---- ResolveGroupLabels — key dimension --------------------------------------

func TestResolveGroupLabels_Key_WithName(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-key-named"})
	creatorID := seedCreatorUser(t, d, "creator-key-named@example.com")

	// Key with a non-empty name — label should be the name.
	plaintext := "vl_uk_aabbccddaabbccddaabbccddaabbccddaabbccddaabb"
	params := CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, testHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeUser,
		Name:      "My Named Key",
		OrgID:     org.ID,
		CreatedBy: creatorID,
	}
	key := mustCreateAPIKey(t, d, params)

	got, err := d.ResolveGroupLabels(context.Background(), "key", []string{key.ID})
	if err != nil {
		t.Fatalf("ResolveGroupLabels(key) error = %v, want nil", err)
	}
	if got[key.ID] != "My Named Key" {
		t.Errorf("label = %q, want %q (key name)", got[key.ID], "My Named Key")
	}
}

func TestResolveGroupLabels_Key_WithoutName_FallsBackToHint(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-key-unnamed"})
	creatorID := seedCreatorUser(t, d, "creator-key-unnamed@example.com")

	plaintext := "vl_uk_11223344112233441122334411223344112233441122"
	hint := keygen.Hint(plaintext)
	params := CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, testHMACSecret),
		KeyHint:   hint,
		KeyType:   keygen.KeyTypeUser,
		Name:      "", // empty name
		OrgID:     org.ID,
		CreatedBy: creatorID,
	}
	key := mustCreateAPIKey(t, d, params)

	got, err := d.ResolveGroupLabels(context.Background(), "key", []string{key.ID})
	if err != nil {
		t.Fatalf("ResolveGroupLabels(key) error = %v, want nil", err)
	}
	if got[key.ID] != hint {
		t.Errorf("label = %q, want hint %q (name was empty)", got[key.ID], hint)
	}
}

func TestResolveGroupLabels_Key_SoftDeleted_StillResolves(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-key-softdel"})
	creatorID := seedCreatorUser(t, d, "creator-key-softdel@example.com")

	plaintext := "vl_uk_ffeeddccffeeddccffeeddccffeeddccffeeddccffee"
	params := CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, testHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeUser,
		Name:      "Ghost Key",
		OrgID:     org.ID,
		CreatedBy: creatorID,
	}
	key := mustCreateAPIKey(t, d, params)

	if err := d.DeleteAPIKey(context.Background(), key.ID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}

	got, err := d.ResolveGroupLabels(context.Background(), "key", []string{key.ID})
	if err != nil {
		t.Fatalf("ResolveGroupLabels(key) soft-deleted error = %v, want nil", err)
	}
	if got[key.ID] != "Ghost Key" {
		t.Errorf("label = %q, want %q (soft-deleted key must still resolve)", got[key.ID], "Ghost Key")
	}
}

func TestResolveGroupLabels_Key_TableDriven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		keyName     string
		wantLabelFn func(hint string) string
	}{
		{
			name:        "named key returns key name",
			keyName:     "Production Key",
			wantLabelFn: func(_ string) string { return "Production Key" },
		},
		{
			name:        "unnamed key returns key hint",
			keyName:     "",
			wantLabelFn: func(hint string) string { return hint },
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := openMigratedDB(t)
			org := mustCreateOrg(t, d, CreateOrgParams{
				Name: "Org",
				Slug: fmt.Sprintf("org-key-td-%d", i),
			})
			creatorID := seedCreatorUser(t, d, fmt.Sprintf("creator-key-td-%d@example.com", i))

			// Use a unique plaintext per sub-test to avoid hash collisions.
			plaintext := fmt.Sprintf("vl_uk_%048d", i)
			hint := keygen.Hint(plaintext)

			params := CreateAPIKeyParams{
				KeyHash:   keygen.Hash(plaintext, testHMACSecret),
				KeyHint:   hint,
				KeyType:   keygen.KeyTypeUser,
				Name:      tc.keyName,
				OrgID:     org.ID,
				CreatedBy: creatorID,
			}
			key := mustCreateAPIKey(t, d, params)

			got, err := d.ResolveGroupLabels(context.Background(), "key", []string{key.ID})
			if err != nil {
				t.Fatalf("ResolveGroupLabels(key) error = %v, want nil", err)
			}
			wantLabel := tc.wantLabelFn(hint)
			if got[key.ID] != wantLabel {
				t.Errorf("label = %q, want %q", got[key.ID], wantLabel)
			}
		})
	}
}

// ---- ResolveGroupLabels — service_account dimension -------------------------

func TestResolveGroupLabels_ServiceAccount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, d *DB) (ids []string, wantLabels map[string]string)
	}{
		{
			name: "single service_account resolves to name",
			setup: func(t *testing.T, d *DB) ([]string, map[string]string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-sa-single"})
				creator := mustCreateUser(t, d, CreateUserParams{
					Email:        "sa-creator-single@example.com",
					DisplayName:  "Creator",
					PasswordHash: testPasswordHash(t),
					AuthProvider: "local",
				})
				sa := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name:      "Deploy Bot",
					OrgID:     org.ID,
					CreatedBy: creator.ID,
				})
				return []string{sa.ID}, map[string]string{sa.ID: "Deploy Bot"}
			},
		},
		{
			name: "multiple service_accounts resolve correctly",
			setup: func(t *testing.T, d *DB) ([]string, map[string]string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-sa-multi"})
				creator := mustCreateUser(t, d, CreateUserParams{
					Email:        "sa-creator-multi@example.com",
					DisplayName:  "Creator",
					PasswordHash: testPasswordHash(t),
					AuthProvider: "local",
				})
				sa1 := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name:      "CI Runner",
					OrgID:     org.ID,
					CreatedBy: creator.ID,
				})
				sa2 := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name:      "Release Bot",
					OrgID:     org.ID,
					CreatedBy: creator.ID,
				})
				return []string{sa1.ID, sa2.ID}, map[string]string{sa1.ID: "CI Runner", sa2.ID: "Release Bot"}
			},
		},
		{
			name: "non-existent service_account id is absent",
			setup: func(t *testing.T, d *DB) ([]string, map[string]string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-sa-ghost"})
				creator := mustCreateUser(t, d, CreateUserParams{
					Email:        "sa-creator-ghost@example.com",
					DisplayName:  "Creator",
					PasswordHash: testPasswordHash(t),
					AuthProvider: "local",
				})
				sa := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name:      "Real SA",
					OrgID:     org.ID,
					CreatedBy: creator.ID,
				})
				ghost := "00000000-0000-0000-0000-000000000066"
				return []string{sa.ID, ghost}, map[string]string{sa.ID: "Real SA"}
			},
		},
		{
			name: "soft-deleted service_account still resolves",
			setup: func(t *testing.T, d *DB) ([]string, map[string]string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "org-sa-deleted"})
				creator := mustCreateUser(t, d, CreateUserParams{
					Email:        "sa-creator-deleted@example.com",
					DisplayName:  "Creator",
					PasswordHash: testPasswordHash(t),
					AuthProvider: "local",
				})
				sa := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name:      "Decommissioned Bot",
					OrgID:     org.ID,
					CreatedBy: creator.ID,
				})
				if err := d.DeleteServiceAccount(context.Background(), sa.ID); err != nil {
					t.Fatalf("DeleteServiceAccount: %v", err)
				}
				return []string{sa.ID}, map[string]string{sa.ID: "Decommissioned Bot"}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := openMigratedDB(t)
			ids, wantLabels := tc.setup(t, d)

			got, err := d.ResolveGroupLabels(context.Background(), "service_account", ids)
			if err != nil {
				t.Fatalf("ResolveGroupLabels(service_account) error = %v, want nil", err)
			}
			if len(got) != len(wantLabels) {
				t.Fatalf("len(got) = %d, want %d; got: %v", len(got), len(wantLabels), got)
			}
			for id, wantLabel := range wantLabels {
				if got[id] != wantLabel {
					t.Errorf("got[%q] = %q, want %q", id, got[id], wantLabel)
				}
			}
		})
	}
}

// ---- ResolveGroupLabels — de-duplication -------------------------------------

func TestResolveGroupLabels_Deduplication(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	id, _ := seedOrgForLabels(t, d, "Dedup Org", "dedup-org-label")

	// Pass the same id three times — result must still have exactly one entry.
	got, err := d.ResolveGroupLabels(context.Background(), "org", []string{id, id, id})
	if err != nil {
		t.Fatalf("ResolveGroupLabels error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Errorf("len(got) = %d, want 1 (same id passed three times)", len(got))
	}
}

// ---- ResolveGroupLabels — mixed empty + valid IDs ----------------------------

func TestResolveGroupLabels_EmptyStringIDsFiltered(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	id, label := seedOrgForLabels(t, d, "Mixed Org", "mixed-org-label")

	// Pass empty strings mixed with a valid id.
	got, err := d.ResolveGroupLabels(context.Background(), "org", []string{"", id, ""})
	if err != nil {
		t.Fatalf("ResolveGroupLabels error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[id] != label {
		t.Errorf("got[%q] = %q, want %q", id, got[id], label)
	}
}

// ---- ResolveGroupLabels — all four resolvable dimensions, table-driven -------

func TestResolveGroupLabels_AllResolvableDimensions(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()

	// Seed one entity of each type.
	orgID, orgLabel := seedOrgForLabels(t, d, "Dimension Org", "dimension-org-label")
	userID, userLabel := seedUserForLabels(t, d, "dimension@example.com", "Dimension User")
	teamID, teamLabel := seedTeamForLabels(t, d, orgID, "Dimension Team", "dimension-team-label")

	creatorID := seedCreatorUser(t, d, "dimension-creator@example.com")
	plaintext := "vl_uk_99887766998877669988776699887766998877669988"
	keyParams := CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, testHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeUser,
		Name:      "Dimension Key",
		OrgID:     orgID,
		CreatedBy: creatorID,
	}
	key := mustCreateAPIKey(t, d, keyParams)
	keyLabel := "Dimension Key"

	tests := []struct {
		groupBy   string
		id        string
		wantLabel string
	}{
		{groupBy: "org", id: orgID, wantLabel: orgLabel},
		{groupBy: "user", id: userID, wantLabel: userLabel},
		{groupBy: "team", id: teamID, wantLabel: teamLabel},
		{groupBy: "key", id: key.ID, wantLabel: keyLabel},
	}

	for _, tc := range tests {
		t.Run("groupBy="+tc.groupBy, func(t *testing.T) {
			t.Parallel()

			got, err := d.ResolveGroupLabels(ctx, tc.groupBy, []string{tc.id})
			if err != nil {
				t.Fatalf("ResolveGroupLabels(%q) error = %v, want nil", tc.groupBy, err)
			}
			if got[tc.id] != tc.wantLabel {
				t.Errorf("label = %q, want %q", got[tc.id], tc.wantLabel)
			}
		})
	}
}

// ---- ResolveOrgActorLabels ---------------------------------------------------

// TestResolveOrgActorLabels exercises the org-scoped resolution used by the
// org_admin audit log path.
func TestResolveOrgActorLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, d *DB) (orgID string, userIDs, saIDs []string, wantLabels map[string]string)
	}{
		{
			name: "member user resolves to display_name",
			setup: func(t *testing.T, d *DB) (string, []string, []string, map[string]string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "orgl-member-user"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "orgl-member@example.com", DisplayName: "Member"})
				mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: user.ID, Role: "member"})
				return org.ID, []string{user.ID}, nil, map[string]string{user.ID: "Member"}
			},
		},
		{
			name: "non-member user is absent",
			setup: func(t *testing.T, d *DB) (string, []string, []string, map[string]string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "orgl-nonmember-user"})
				// User exists but has no membership in org.
				outsider := mustCreateUser(t, d, CreateUserParams{Email: "orgl-outsider@example.com", DisplayName: "Outsider"})
				return org.ID, []string{outsider.ID}, nil, map[string]string{}
			},
		},
		{
			name: "SA in org resolves to name",
			setup: func(t *testing.T, d *DB) (string, []string, []string, map[string]string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "orgl-sa-in-org"})
				creator := mustCreateUser(t, d, CreateUserParams{Email: "orgl-sa-creator@example.com", DisplayName: "Creator"})
				sa := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name:      "In-Org SA",
					OrgID:     org.ID,
					CreatedBy: creator.ID,
				})
				return org.ID, nil, []string{sa.ID}, map[string]string{sa.ID: "In-Org SA"}
			},
		},
		{
			name: "SA in different org is absent",
			setup: func(t *testing.T, d *DB) (string, []string, []string, map[string]string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Target Org", Slug: "orgl-target-org"})
				otherOrg := mustCreateOrg(t, d, CreateOrgParams{Name: "Other Org", Slug: "orgl-other-org"})
				creator := mustCreateUser(t, d, CreateUserParams{Email: "orgl-other-creator@example.com", DisplayName: "Creator"})
				sa := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name:      "Other-Org SA",
					OrgID:     otherOrg.ID,
					CreatedBy: creator.ID,
				})
				return org.ID, nil, []string{sa.ID}, map[string]string{}
			},
		},
		{
			name: "empty inputs return empty non-nil map",
			setup: func(t *testing.T, d *DB) (string, []string, []string, map[string]string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "orgl-empty-inputs"})
				return org.ID, nil, nil, map[string]string{}
			},
		},
		{
			name: "member user and SA in same org both resolve",
			setup: func(t *testing.T, d *DB) (string, []string, []string, map[string]string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "orgl-mixed-both"})
				user := mustCreateUser(t, d, CreateUserParams{Email: "orgl-both-user@example.com", DisplayName: "Both User"})
				mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: user.ID, Role: "org_admin"})
				creator := mustCreateUser(t, d, CreateUserParams{Email: "orgl-both-creator@example.com", DisplayName: "Creator"})
				sa := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
					Name:      "Both SA",
					OrgID:     org.ID,
					CreatedBy: creator.ID,
				})
				return org.ID, []string{user.ID}, []string{sa.ID}, map[string]string{
					user.ID: "Both User",
					sa.ID:   "Both SA",
				}
			},
		},
		{
			name: "cross-org user mixed with member — only member resolves",
			setup: func(t *testing.T, d *DB) (string, []string, []string, map[string]string) {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Org", Slug: "orgl-cross-org-mixed"})
				member := mustCreateUser(t, d, CreateUserParams{Email: "orgl-cross-member@example.com", DisplayName: "Member"})
				mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: member.ID, Role: "member"})
				outsider := mustCreateUser(t, d, CreateUserParams{Email: "orgl-cross-outsider@example.com", DisplayName: "Outsider"})
				return org.ID, []string{member.ID, outsider.ID}, nil, map[string]string{member.ID: "Member"}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := openMigratedDB(t)
			orgID, userIDs, saIDs, wantLabels := tc.setup(t, d)

			got, err := d.ResolveOrgActorLabels(context.Background(), orgID, userIDs, saIDs)
			if err != nil {
				t.Fatalf("ResolveOrgActorLabels() error = %v, want nil", err)
			}
			if got == nil {
				t.Fatal("ResolveOrgActorLabels() returned nil map, want non-nil")
			}
			if len(got) != len(wantLabels) {
				t.Fatalf("len(got) = %d, want %d; got: %v", len(got), len(wantLabels), got)
			}
			for id, wantLabel := range wantLabels {
				if got[id] != wantLabel {
					t.Errorf("got[%q] = %q, want %q", id, got[id], wantLabel)
				}
			}
		})
	}
}

// ---- ResolveGroupLabels — chunking -------------------------------------------

// TestResolveGroupLabels_Chunking seeds more than labelResolveChunkSize (500)
// users and asserts that all of them are resolved in a single call. This proves
// the chunking loop works correctly and does not drop entities at chunk
// boundaries.
func TestResolveGroupLabels_Chunking(t *testing.T) {
	t.Parallel()

	const count = 600 // deliberately exceeds the 500-ID chunk size

	d := openMigratedDB(t)
	ctx := context.Background()

	ids := make([]string, 0, count)
	wantLabels := make(map[string]string, count)

	for i := range count {
		user := mustCreateUser(t, d, CreateUserParams{
			Email:       fmt.Sprintf("chunk-user-%04d@example.com", i),
			DisplayName: fmt.Sprintf("Chunk User %04d", i),
		})
		ids = append(ids, user.ID)
		wantLabels[user.ID] = user.DisplayName
	}

	got, err := d.ResolveGroupLabels(ctx, "user", ids)
	if err != nil {
		t.Fatalf("ResolveGroupLabels() error = %v, want nil", err)
	}
	if len(got) != count {
		t.Fatalf("len(got) = %d, want %d (chunking must not drop entries)", len(got), count)
	}
	for id, wantLabel := range wantLabels {
		if got[id] != wantLabel {
			t.Errorf("got[%q] = %q, want %q", id, got[id], wantLabel)
		}
	}
}
