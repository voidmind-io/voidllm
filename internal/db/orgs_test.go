package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
)

// openMigratedDB opens an isolated in-memory SQLite DB, runs all migrations,
// and registers cleanup. Each test must pass a unique name so that the
// in-memory database is not shared across parallel tests.
func openMigratedDB(t *testing.T) *DB {
	t.Helper()

	// Replace characters that SQLite rejects in URI filenames.
	safeName := strings.NewReplacer("/", "_", " ", "_", "#", "_").Replace(t.Name())

	cfg := config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             "file:" + safeName + "?mode=memory&cache=private",
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	}
	ctx := context.Background()
	d, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if err := RunMigrations(ctx, d.SQL(), d.Dialect()); err != nil {
		t.Fatalf("RunMigrations() error = %v", err)
	}
	return d
}

// ptr returns a pointer to v. Convenience for building params in table tests.
func ptr[T any](v T) *T { return &v }

// mustCreateOrg creates an org and fails the test if an error occurs.
func mustCreateOrg(t *testing.T, d *DB, params CreateOrgParams) *Org {
	t.Helper()
	org, err := d.CreateOrg(context.Background(), params)
	if err != nil {
		t.Fatalf("mustCreateOrg(%q): %v", params.Slug, err)
	}
	return org
}

// ---- CreateOrg ---------------------------------------------------------------

func TestCreateOrg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		params    CreateOrgParams
		wantErr   error
		checkFunc func(t *testing.T, got *Org, params CreateOrgParams)
	}{
		{
			name: "valid params returns org with fields set",
			params: CreateOrgParams{
				Name: "Acme Corp",
				Slug: "acme-corp",
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *Org, params CreateOrgParams) {
				t.Helper()
				if got.ID == "" {
					t.Error("ID is empty, want non-empty UUID")
				}
				if got.Name != params.Name {
					t.Errorf("Name = %q, want %q", got.Name, params.Name)
				}
				if got.Slug != params.Slug {
					t.Errorf("Slug = %q, want %q", got.Slug, params.Slug)
				}
				if got.CreatedAt == "" {
					t.Error("CreatedAt is empty, want a timestamp")
				}
				if got.UpdatedAt == "" {
					t.Error("UpdatedAt is empty, want a timestamp")
				}
				if got.DeletedAt != nil {
					t.Errorf("DeletedAt = %v, want nil", got.DeletedAt)
				}
			},
		},
		{
			name: "with timezone populates timezone field",
			params: CreateOrgParams{
				Name:     "Europe Org",
				Slug:     "europe-org",
				Timezone: ptr("Europe/Berlin"),
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *Org, params CreateOrgParams) {
				t.Helper()
				if got.Timezone == nil {
					t.Fatal("Timezone = nil, want non-nil")
				}
				if *got.Timezone != "Europe/Berlin" {
					t.Errorf("Timezone = %q, want %q", *got.Timezone, "Europe/Berlin")
				}
			},
		},
		{
			name: "without timezone leaves timezone nil",
			params: CreateOrgParams{
				Name: "No TZ Org",
				Slug: "no-tz-org",
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *Org, _ CreateOrgParams) {
				t.Helper()
				if got.Timezone != nil {
					t.Errorf("Timezone = %v, want nil", *got.Timezone)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			got, err := d.CreateOrg(context.Background(), tc.params)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateOrg() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, got, tc.params)
			}
		})
	}
}

func TestCreateOrg_DuplicateSlug(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	mustCreateOrg(t, d, CreateOrgParams{Name: "First", Slug: "dup-slug"})

	_, err := d.CreateOrg(ctx, CreateOrgParams{Name: "Second", Slug: "dup-slug"})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("CreateOrg() duplicate slug error = %v, want ErrConflict", err)
	}
}

// ---- GetOrg ------------------------------------------------------------------

func TestGetOrg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string // returns the ID to look up
		wantErr error
	}{
		{
			name: "existing org returns correct data",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{
					Name:     "Lookup Org",
					Slug:     "lookup-org",
					Timezone: ptr("America/New_York"),
				})
				return org.ID
			},
			wantErr: nil,
		},
		{
			name: "non-existent ID returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				return "00000000-0000-0000-0000-000000000000"
			},
			wantErr: ErrNotFound,
		},
		{
			name: "soft-deleted org returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Gone Org", Slug: "gone-org"})
				if err := d.DeleteOrg(context.Background(), org.ID); err != nil {
					t.Fatalf("DeleteOrg(): %v", err)
				}
				return org.ID
			},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			id := tc.setup(t, d)
			got, err := d.GetOrg(context.Background(), id)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetOrg() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				if got == nil {
					t.Fatal("GetOrg() returned nil, want non-nil Org")
				}
				if got.ID != id {
					t.Errorf("GetOrg().ID = %q, want %q", got.ID, id)
				}
				if got.Name != "Lookup Org" {
					t.Errorf("GetOrg().Name = %q, want %q", got.Name, "Lookup Org")
				}
				if got.Timezone == nil || *got.Timezone != "America/New_York" {
					t.Errorf("GetOrg().Timezone = %v, want %q", got.Timezone, "America/New_York")
				}
			}
		})
	}
}

// ---- ListOrgs ----------------------------------------------------------------

func TestListOrgs_EmptyDB(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)

	orgs, err := d.ListOrgs(context.Background(), "", 100, false)
	if err != nil {
		t.Fatalf("ListOrgs() error = %v, want nil", err)
	}
	// Must return an empty non-nil slice equivalent — zero length is fine,
	// but a nil panic would indicate a broken caller contract.
	if len(orgs) != 0 {
		t.Errorf("ListOrgs() len = %d, want 0", len(orgs))
	}
}

func TestListOrgs_ReturnsAllOrgs(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	slugs := []string{"list-org-a", "list-org-b", "list-org-c"}
	for _, s := range slugs {
		mustCreateOrg(t, d, CreateOrgParams{Name: s, Slug: s})
	}

	orgs, err := d.ListOrgs(ctx, "", 100, false)
	if err != nil {
		t.Fatalf("ListOrgs() error = %v, want nil", err)
	}
	if len(orgs) != 3 {
		t.Errorf("ListOrgs() len = %d, want 3", len(orgs))
	}
}

func TestListOrgs_CursorPagination(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	// Create 5 orgs. UUIDs v7 are time-sortable, so insertion order == ID order
	// as long as they are created sequentially within the same test.
	for i := range 5 {
		mustCreateOrg(t, d, CreateOrgParams{
			Name: "Paged Org",
			Slug: "paged-org-" + string(rune('a'+i)),
		})
	}

	// Page 1: first 2.
	page1, err := d.ListOrgs(ctx, "", 2, false)
	if err != nil {
		t.Fatalf("ListOrgs page1 error = %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}

	// Page 2: next 2 using last ID from page 1 as cursor.
	page2, err := d.ListOrgs(ctx, page1[len(page1)-1].ID, 2, false)
	if err != nil {
		t.Fatalf("ListOrgs page2 error = %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2))
	}

	// Page 3: last 1 using last ID from page 2 as cursor.
	page3, err := d.ListOrgs(ctx, page2[len(page2)-1].ID, 2, false)
	if err != nil {
		t.Fatalf("ListOrgs page3 error = %v", err)
	}
	if len(page3) != 1 {
		t.Fatalf("page3 len = %d, want 1", len(page3))
	}

	// All IDs across pages must be distinct and in ascending order.
	all := append(append(page1, page2...), page3...)
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Errorf("IDs not in ascending order: all[%d].ID=%q <= all[%d].ID=%q",
				i, all[i].ID, i-1, all[i-1].ID)
		}
	}
}

func TestListOrgs_ExcludesSoftDeleted(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	live := mustCreateOrg(t, d, CreateOrgParams{Name: "Live", Slug: "live-org"})
	gone := mustCreateOrg(t, d, CreateOrgParams{Name: "Gone", Slug: "gone-org-list"})

	if err := d.DeleteOrg(ctx, gone.ID); err != nil {
		t.Fatalf("DeleteOrg(): %v", err)
	}

	orgs, err := d.ListOrgs(ctx, "", 100, false)
	if err != nil {
		t.Fatalf("ListOrgs() error = %v", err)
	}
	if len(orgs) != 1 {
		t.Fatalf("ListOrgs(includeDeleted=false) len = %d, want 1", len(orgs))
	}
	if orgs[0].ID != live.ID {
		t.Errorf("ListOrgs returned org ID %q, want %q", orgs[0].ID, live.ID)
	}
}

func TestListOrgs_IncludesSoftDeleted(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	mustCreateOrg(t, d, CreateOrgParams{Name: "Live", Slug: "incl-live-org"})
	gone := mustCreateOrg(t, d, CreateOrgParams{Name: "Gone", Slug: "incl-gone-org"})

	if err := d.DeleteOrg(ctx, gone.ID); err != nil {
		t.Fatalf("DeleteOrg(): %v", err)
	}

	orgs, err := d.ListOrgs(ctx, "", 100, true)
	if err != nil {
		t.Fatalf("ListOrgs() error = %v", err)
	}
	if len(orgs) != 2 {
		t.Fatalf("ListOrgs(includeDeleted=true) len = %d, want 2", len(orgs))
	}

	var foundDeleted bool
	for _, o := range orgs {
		if o.ID == gone.ID {
			foundDeleted = true
			if o.DeletedAt == nil {
				t.Error("deleted org has nil DeletedAt, want a timestamp")
			}
		}
	}
	if !foundDeleted {
		t.Error("deleted org not found in ListOrgs(includeDeleted=true) results")
	}
}

// ---- UpdateOrg ---------------------------------------------------------------

func TestUpdateOrg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, d *DB) *Org
		params    UpdateOrgParams
		wantErr   error
		checkFunc func(t *testing.T, original, got *Org)
	}{
		{
			name: "update name only leaves slug unchanged",
			setup: func(t *testing.T, d *DB) *Org {
				t.Helper()
				return mustCreateOrg(t, d, CreateOrgParams{Name: "Original Name", Slug: "update-name-slug"})
			},
			params:  UpdateOrgParams{Name: ptr("New Name")},
			wantErr: nil,
			checkFunc: func(t *testing.T, original, got *Org) {
				t.Helper()
				if got.Name != "New Name" {
					t.Errorf("Name = %q, want %q", got.Name, "New Name")
				}
				if got.Slug != original.Slug {
					t.Errorf("Slug = %q, want %q (unchanged)", got.Slug, original.Slug)
				}
			},
		},
		{
			name: "update slug only leaves name unchanged",
			setup: func(t *testing.T, d *DB) *Org {
				t.Helper()
				return mustCreateOrg(t, d, CreateOrgParams{Name: "Keep This Name", Slug: "old-slug"})
			},
			params:  UpdateOrgParams{Slug: ptr("new-slug")},
			wantErr: nil,
			checkFunc: func(t *testing.T, original, got *Org) {
				t.Helper()
				if got.Slug != "new-slug" {
					t.Errorf("Slug = %q, want %q", got.Slug, "new-slug")
				}
				if got.Name != original.Name {
					t.Errorf("Name = %q, want %q (unchanged)", got.Name, original.Name)
				}
			},
		},
		{
			name: "no fields set returns org unchanged",
			setup: func(t *testing.T, d *DB) *Org {
				t.Helper()
				return mustCreateOrg(t, d, CreateOrgParams{Name: "Stable Org", Slug: "stable-org"})
			},
			params:  UpdateOrgParams{},
			wantErr: nil,
			checkFunc: func(t *testing.T, original, got *Org) {
				t.Helper()
				if got.Name != original.Name {
					t.Errorf("Name = %q, want %q", got.Name, original.Name)
				}
				if got.Slug != original.Slug {
					t.Errorf("Slug = %q, want %q", got.Slug, original.Slug)
				}
			},
		},
		{
			name: "non-existent ID returns ErrNotFound",
			setup: func(t *testing.T, d *DB) *Org {
				return &Org{ID: "00000000-0000-0000-0000-000000000000"}
			},
			params:  UpdateOrgParams{Name: ptr("Ghost")},
			wantErr: ErrNotFound,
		},
		{
			name: "soft-deleted org returns ErrNotFound",
			setup: func(t *testing.T, d *DB) *Org {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Deleted Org", Slug: "deleted-org-upd"})
				if err := d.DeleteOrg(context.Background(), org.ID); err != nil {
					t.Fatalf("DeleteOrg(): %v", err)
				}
				return org
			},
			params:  UpdateOrgParams{Name: ptr("Still Gone")},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			original := tc.setup(t, d)
			got, err := d.UpdateOrg(context.Background(), original.ID, tc.params)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("UpdateOrg() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, original, got)
			}
		})
	}
}

func TestUpdateOrg_AllFields(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	org := mustCreateOrg(t, d, CreateOrgParams{
		Name:              "All Fields Org",
		Slug:              "all-fields-org",
		Timezone:          ptr("UTC"),
		DailyTokenLimit:   1000,
		MonthlyTokenLimit: 10000,
		RequestsPerMinute: 60,
		RequestsPerDay:    1440,
	})

	got, err := d.UpdateOrg(ctx, org.ID, UpdateOrgParams{
		Name:              ptr("Updated Name"),
		Slug:              ptr("updated-slug"),
		Timezone:          ptr("Asia/Tokyo"),
		DailyTokenLimit:   ptr(int64(2000)),
		MonthlyTokenLimit: ptr(int64(20000)),
		RequestsPerMinute: ptr(120),
		RequestsPerDay:    ptr(2880),
	})
	if err != nil {
		t.Fatalf("UpdateOrg() error = %v, want nil", err)
	}

	if got.Name != "Updated Name" {
		t.Errorf("Name = %q, want %q", got.Name, "Updated Name")
	}
	if got.Slug != "updated-slug" {
		t.Errorf("Slug = %q, want %q", got.Slug, "updated-slug")
	}
	if got.Timezone == nil || *got.Timezone != "Asia/Tokyo" {
		t.Errorf("Timezone = %v, want %q", got.Timezone, "Asia/Tokyo")
	}
	if got.DailyTokenLimit != 2000 {
		t.Errorf("DailyTokenLimit = %d, want 2000", got.DailyTokenLimit)
	}
	if got.MonthlyTokenLimit != 20000 {
		t.Errorf("MonthlyTokenLimit = %d, want 20000", got.MonthlyTokenLimit)
	}
	if got.RequestsPerMinute != 120 {
		t.Errorf("RequestsPerMinute = %d, want 120", got.RequestsPerMinute)
	}
	if got.RequestsPerDay != 2880 {
		t.Errorf("RequestsPerDay = %d, want 2880", got.RequestsPerDay)
	}
}

func TestUpdateOrg_DuplicateSlug(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	mustCreateOrg(t, d, CreateOrgParams{Name: "Alpha", Slug: "alpha-slug"})
	beta := mustCreateOrg(t, d, CreateOrgParams{Name: "Beta", Slug: "beta-slug"})

	_, err := d.UpdateOrg(ctx, beta.ID, UpdateOrgParams{Slug: ptr("alpha-slug")})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("UpdateOrg() duplicate slug error = %v, want ErrConflict", err)
	}
}

// ---- DeleteOrg ---------------------------------------------------------------

func TestDeleteOrg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string
		wantErr error
	}{
		{
			name: "delete existing org makes GetOrg return ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				return mustCreateOrg(t, d, CreateOrgParams{Name: "To Delete", Slug: "to-delete"}).ID
			},
			wantErr: nil,
		},
		{
			name: "delete non-existent ID returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				return "00000000-0000-0000-0000-000000000000"
			},
			wantErr: ErrNotFound,
		},
		{
			name: "delete already-deleted org returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Double Delete", Slug: "double-delete"})
				if err := d.DeleteOrg(context.Background(), org.ID); err != nil {
					t.Fatalf("first DeleteOrg(): %v", err)
				}
				return org.ID
			},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			id := tc.setup(t, d)
			err := d.DeleteOrg(context.Background(), id)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("DeleteOrg() error = %v, wantErr %v", err, tc.wantErr)
			}

			// On success, verify GetOrg now returns ErrNotFound.
			if tc.wantErr == nil {
				_, getErr := d.GetOrg(context.Background(), id)
				if !errors.Is(getErr, ErrNotFound) {
					t.Errorf("GetOrg() after DeleteOrg() error = %v, want ErrNotFound", getErr)
				}
			}
		})
	}
}
