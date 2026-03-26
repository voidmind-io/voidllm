package db

import (
	"context"
	"errors"
	"testing"
)

// mustCreateModel creates a model and fails the test if an error occurs.
func mustCreateModel(t *testing.T, d *DB, name string) *Model {
	t.Helper()
	m, err := d.CreateModel(context.Background(), CreateModelParams{
		Name:     name,
		Provider: "openai",
		BaseURL:  "https://api.openai.com/v1",
		Source:   "api",
	})
	if err != nil {
		t.Fatalf("mustCreateModel(%q): %v", name, err)
	}
	return m
}

// mustCreateDeployment creates a deployment and fails the test if an error occurs.
func mustCreateDeployment(t *testing.T, d *DB, params CreateDeploymentParams) *Deployment {
	t.Helper()
	dep, err := d.CreateDeployment(context.Background(), params)
	if err != nil {
		t.Fatalf("mustCreateDeployment(%q): %v", params.Name, err)
	}
	return dep
}

// ---- CreateDeployment -------------------------------------------------------

func TestCreateDeployment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, d *DB) CreateDeploymentParams
		wantErr   error
		checkFunc func(t *testing.T, got *Deployment, params CreateDeploymentParams)
	}{
		{
			name: "creates deployment with all fields set",
			setup: func(t *testing.T, d *DB) CreateDeploymentParams {
				t.Helper()
				m := mustCreateModel(t, d, "gpt-4-create")
				enc := "encryptedkey"
				return CreateDeploymentParams{
					ModelID:         m.ID,
					Name:            "primary",
					Provider:        "openai",
					BaseURL:         "https://api.openai.com/v1",
					APIKeyEncrypted: &enc,
					AzureDeployment: "gpt4dep",
					AzureAPIVersion: "2024-02-01",
					Weight:          5,
					Priority:        1,
				}
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *Deployment, params CreateDeploymentParams) {
				t.Helper()
				if got.ID == "" {
					t.Error("ID is empty, want non-empty UUID")
				}
				if got.ModelID != params.ModelID {
					t.Errorf("ModelID = %q, want %q", got.ModelID, params.ModelID)
				}
				if got.Name != params.Name {
					t.Errorf("Name = %q, want %q", got.Name, params.Name)
				}
				if got.Provider != params.Provider {
					t.Errorf("Provider = %q, want %q", got.Provider, params.Provider)
				}
				if got.BaseURL != params.BaseURL {
					t.Errorf("BaseURL = %q, want %q", got.BaseURL, params.BaseURL)
				}
				if got.APIKeyEncrypted == nil || *got.APIKeyEncrypted != "encryptedkey" {
					t.Errorf("APIKeyEncrypted = %v, want %q", got.APIKeyEncrypted, "encryptedkey")
				}
				if got.AzureDeployment != params.AzureDeployment {
					t.Errorf("AzureDeployment = %q, want %q", got.AzureDeployment, params.AzureDeployment)
				}
				if got.AzureAPIVersion != params.AzureAPIVersion {
					t.Errorf("AzureAPIVersion = %q, want %q", got.AzureAPIVersion, params.AzureAPIVersion)
				}
				if got.Weight != params.Weight {
					t.Errorf("Weight = %d, want %d", got.Weight, params.Weight)
				}
				if got.Priority != params.Priority {
					t.Errorf("Priority = %d, want %d", got.Priority, params.Priority)
				}
				if !got.IsActive {
					t.Error("IsActive = false, want true")
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
			name: "weight below 1 is clamped to 1",
			setup: func(t *testing.T, d *DB) CreateDeploymentParams {
				t.Helper()
				m := mustCreateModel(t, d, "gpt-4-weight")
				return CreateDeploymentParams{
					ModelID:  m.ID,
					Name:     "low-weight",
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Weight:   0,
				}
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *Deployment, _ CreateDeploymentParams) {
				t.Helper()
				if got.Weight != 1 {
					t.Errorf("Weight = %d, want 1 (clamped from 0)", got.Weight)
				}
			},
		},
		{
			name: "nil api key is stored as nil",
			setup: func(t *testing.T, d *DB) CreateDeploymentParams {
				t.Helper()
				m := mustCreateModel(t, d, "gpt-4-nokey")
				return CreateDeploymentParams{
					ModelID:         m.ID,
					Name:            "no-key",
					Provider:        "openai",
					BaseURL:         "https://api.openai.com/v1",
					APIKeyEncrypted: nil,
					Weight:          1,
				}
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, got *Deployment, _ CreateDeploymentParams) {
				t.Helper()
				if got.APIKeyEncrypted != nil {
					t.Errorf("APIKeyEncrypted = %v, want nil", got.APIKeyEncrypted)
				}
			},
		},
		{
			name: "duplicate name on same model returns ErrConflict",
			setup: func(t *testing.T, d *DB) CreateDeploymentParams {
				t.Helper()
				m := mustCreateModel(t, d, "gpt-4-dup")
				mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID:  m.ID,
					Name:     "dup-name",
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Weight:   1,
				})
				return CreateDeploymentParams{
					ModelID:  m.ID,
					Name:     "dup-name",
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Weight:   1,
				}
			},
			wantErr: ErrConflict,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			params := tc.setup(t, d)
			got, err := d.CreateDeployment(context.Background(), params)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateDeployment() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, got, params)
			}
		})
	}
}

// ---- GetDeployment ----------------------------------------------------------

func TestGetDeployment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string // returns the ID to look up
		wantErr error
	}{
		{
			name: "existing deployment returns correct data",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				m := mustCreateModel(t, d, "gpt-4-get")
				dep := mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID:  m.ID,
					Name:     "get-dep",
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Weight:   2,
					Priority: 3,
				})
				return dep.ID
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
			name: "soft-deleted deployment returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				m := mustCreateModel(t, d, "gpt-4-get-deleted")
				dep := mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID:  m.ID,
					Name:     "deleted-dep",
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Weight:   1,
				})
				if err := d.DeleteDeployment(context.Background(), dep.ID); err != nil {
					t.Fatalf("DeleteDeployment(): %v", err)
				}
				return dep.ID
			},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			id := tc.setup(t, d)
			got, err := d.GetDeployment(context.Background(), id)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetDeployment() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				if got == nil {
					t.Fatal("GetDeployment() returned nil, want non-nil Deployment")
				}
				if got.ID != id {
					t.Errorf("GetDeployment().ID = %q, want %q", got.ID, id)
				}
				if got.Name != "get-dep" {
					t.Errorf("GetDeployment().Name = %q, want %q", got.Name, "get-dep")
				}
				if got.Weight != 2 {
					t.Errorf("GetDeployment().Weight = %d, want 2", got.Weight)
				}
				if got.Priority != 3 {
					t.Errorf("GetDeployment().Priority = %d, want 3", got.Priority)
				}
			}
		})
	}
}

// ---- ListDeployments --------------------------------------------------------

func TestListDeployments_OrderByPriority(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	m := mustCreateModel(t, d, "gpt-4-list")

	// Create three deployments with descending priority values so that ordering
	// by priority ASC produces a different order than insertion order.
	mustCreateDeployment(t, d, CreateDeploymentParams{
		ModelID:  m.ID,
		Name:     "high-priority",
		Provider: "openai",
		BaseURL:  "https://api.openai.com/v1",
		Weight:   1,
		Priority: 0,
	})
	mustCreateDeployment(t, d, CreateDeploymentParams{
		ModelID:  m.ID,
		Name:     "low-priority",
		Provider: "openai",
		BaseURL:  "https://api.openai.com/v1",
		Weight:   1,
		Priority: 10,
	})
	mustCreateDeployment(t, d, CreateDeploymentParams{
		ModelID:  m.ID,
		Name:     "mid-priority",
		Provider: "openai",
		BaseURL:  "https://api.openai.com/v1",
		Weight:   1,
		Priority: 5,
	})

	deps, err := d.ListDeployments(ctx, m.ID)
	if err != nil {
		t.Fatalf("ListDeployments() error = %v", err)
	}
	if len(deps) != 3 {
		t.Fatalf("ListDeployments() len = %d, want 3", len(deps))
	}

	// Verify priority ascending order.
	for i := 1; i < len(deps); i++ {
		if deps[i].Priority < deps[i-1].Priority {
			t.Errorf("deployments not in priority ASC order: deps[%d].Priority=%d < deps[%d].Priority=%d",
				i, deps[i].Priority, i-1, deps[i-1].Priority)
		}
	}

	if deps[0].Name != "high-priority" {
		t.Errorf("deps[0].Name = %q, want %q", deps[0].Name, "high-priority")
	}
	if deps[2].Name != "low-priority" {
		t.Errorf("deps[2].Name = %q, want %q", deps[2].Name, "low-priority")
	}
}

func TestListDeployments_ExcludesSoftDeleted(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	m := mustCreateModel(t, d, "gpt-4-list-del")

	live := mustCreateDeployment(t, d, CreateDeploymentParams{
		ModelID:  m.ID,
		Name:     "live-dep",
		Provider: "openai",
		BaseURL:  "https://api.openai.com/v1",
		Weight:   1,
	})
	gone := mustCreateDeployment(t, d, CreateDeploymentParams{
		ModelID:  m.ID,
		Name:     "gone-dep",
		Provider: "openai",
		BaseURL:  "https://api.openai.com/v1",
		Weight:   1,
	})

	if err := d.DeleteDeployment(ctx, gone.ID); err != nil {
		t.Fatalf("DeleteDeployment(): %v", err)
	}

	deps, err := d.ListDeployments(ctx, m.ID)
	if err != nil {
		t.Fatalf("ListDeployments() error = %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("ListDeployments() len = %d, want 1", len(deps))
	}
	if deps[0].ID != live.ID {
		t.Errorf("ListDeployments() returned ID %q, want %q", deps[0].ID, live.ID)
	}
}

func TestListDeployments_EmptyModel(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)

	m := mustCreateModel(t, d, "gpt-4-empty-list")

	deps, err := d.ListDeployments(context.Background(), m.ID)
	if err != nil {
		t.Fatalf("ListDeployments() error = %v", err)
	}
	if len(deps) != 0 {
		t.Errorf("ListDeployments() len = %d, want 0", len(deps))
	}
}

// ---- ListActiveDeployments --------------------------------------------------

func TestListActiveDeployments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, d *DB, modelID string)
		wantCount int
	}{
		{
			name: "returns only active deployments",
			setup: func(t *testing.T, d *DB, modelID string) {
				t.Helper()
				mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID:  modelID,
					Name:     "active-dep",
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Weight:   1,
				})
				inactive := mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID:  modelID,
					Name:     "inactive-dep",
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Weight:   1,
				})
				// Deactivate by setting is_active = 0 directly via UpdateDeployment is not
				// possible from this API, but we can soft-delete and confirm it is excluded.
				// Instead, use a raw SQL update to deactivate without deleting.
				_, err := d.sql.ExecContext(context.Background(),
					"UPDATE model_deployments SET is_active = 0 WHERE id = ?", inactive.ID)
				if err != nil {
					t.Fatalf("deactivate deployment: %v", err)
				}
			},
			wantCount: 1,
		},
		{
			name: "excludes soft-deleted deployments",
			setup: func(t *testing.T, d *DB, modelID string) {
				t.Helper()
				mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID:  modelID,
					Name:     "live-dep-2",
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Weight:   1,
				})
				gone := mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID:  modelID,
					Name:     "gone-dep-2",
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Weight:   1,
				})
				if err := d.DeleteDeployment(context.Background(), gone.ID); err != nil {
					t.Fatalf("DeleteDeployment(): %v", err)
				}
			},
			wantCount: 1,
		},
		{
			name: "empty model returns empty slice",
			setup: func(t *testing.T, d *DB, modelID string) {
				// no deployments
			},
			wantCount: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)
			m := mustCreateModel(t, d, "gpt-4-active-"+tc.name)

			tc.setup(t, d, m.ID)

			deps, err := d.ListActiveDeployments(context.Background(), m.ID)
			if err != nil {
				t.Fatalf("ListActiveDeployments() error = %v", err)
			}
			if len(deps) != tc.wantCount {
				t.Errorf("ListActiveDeployments() len = %d, want %d", len(deps), tc.wantCount)
			}
			for _, dep := range deps {
				if !dep.IsActive {
					t.Errorf("ListActiveDeployments() returned inactive deployment %q", dep.ID)
				}
			}
		})
	}
}

// ---- UpdateDeployment -------------------------------------------------------

func TestUpdateDeployment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, d *DB) (id string, original *Deployment)
		params    UpdateDeploymentParams
		wantErr   error
		checkFunc func(t *testing.T, original, got *Deployment)
	}{
		{
			name: "update name only leaves other fields unchanged",
			setup: func(t *testing.T, d *DB) (string, *Deployment) {
				t.Helper()
				m := mustCreateModel(t, d, "gpt-4-upd-name")
				dep := mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID:  m.ID,
					Name:     "original-name",
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Weight:   3,
					Priority: 2,
				})
				return dep.ID, dep
			},
			params:  UpdateDeploymentParams{Name: ptr("updated-name")},
			wantErr: nil,
			checkFunc: func(t *testing.T, original, got *Deployment) {
				t.Helper()
				if got.Name != "updated-name" {
					t.Errorf("Name = %q, want %q", got.Name, "updated-name")
				}
				if got.Provider != original.Provider {
					t.Errorf("Provider = %q, want %q (unchanged)", got.Provider, original.Provider)
				}
				if got.BaseURL != original.BaseURL {
					t.Errorf("BaseURL = %q, want %q (unchanged)", got.BaseURL, original.BaseURL)
				}
				if got.Weight != original.Weight {
					t.Errorf("Weight = %d, want %d (unchanged)", got.Weight, original.Weight)
				}
				if got.Priority != original.Priority {
					t.Errorf("Priority = %d, want %d (unchanged)", got.Priority, original.Priority)
				}
			},
		},
		{
			name: "update provider and base url",
			setup: func(t *testing.T, d *DB) (string, *Deployment) {
				t.Helper()
				m := mustCreateModel(t, d, "gpt-4-upd-provider")
				dep := mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID:  m.ID,
					Name:     "provider-dep",
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Weight:   1,
				})
				return dep.ID, dep
			},
			params:  UpdateDeploymentParams{Provider: ptr("anthropic"), BaseURL: ptr("https://api.anthropic.com/v1")},
			wantErr: nil,
			checkFunc: func(t *testing.T, original, got *Deployment) {
				t.Helper()
				if got.Provider != "anthropic" {
					t.Errorf("Provider = %q, want %q", got.Provider, "anthropic")
				}
				if got.BaseURL != "https://api.anthropic.com/v1" {
					t.Errorf("BaseURL = %q, want %q", got.BaseURL, "https://api.anthropic.com/v1")
				}
			},
		},
		{
			name: "update weight and priority",
			setup: func(t *testing.T, d *DB) (string, *Deployment) {
				t.Helper()
				m := mustCreateModel(t, d, "gpt-4-upd-weight")
				dep := mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID:  m.ID,
					Name:     "weight-dep",
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Weight:   1,
					Priority: 0,
				})
				return dep.ID, dep
			},
			params:  UpdateDeploymentParams{Weight: ptr(10), Priority: ptr(5)},
			wantErr: nil,
			checkFunc: func(t *testing.T, original, got *Deployment) {
				t.Helper()
				if got.Weight != 10 {
					t.Errorf("Weight = %d, want 10", got.Weight)
				}
				if got.Priority != 5 {
					t.Errorf("Priority = %d, want 5", got.Priority)
				}
			},
		},
		{
			name: "no fields set returns deployment unchanged",
			setup: func(t *testing.T, d *DB) (string, *Deployment) {
				t.Helper()
				m := mustCreateModel(t, d, "gpt-4-upd-noop")
				dep := mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID:  m.ID,
					Name:     "noop-dep",
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Weight:   7,
					Priority: 4,
				})
				return dep.ID, dep
			},
			params:  UpdateDeploymentParams{},
			wantErr: nil,
			checkFunc: func(t *testing.T, original, got *Deployment) {
				t.Helper()
				if got.Name != original.Name {
					t.Errorf("Name = %q, want %q", got.Name, original.Name)
				}
				if got.Weight != original.Weight {
					t.Errorf("Weight = %d, want %d", got.Weight, original.Weight)
				}
			},
		},
		{
			name: "non-existent ID returns ErrNotFound",
			setup: func(t *testing.T, d *DB) (string, *Deployment) {
				return "00000000-0000-0000-0000-000000000000", &Deployment{ID: "00000000-0000-0000-0000-000000000000"}
			},
			params:  UpdateDeploymentParams{Name: ptr("ghost")},
			wantErr: ErrNotFound,
		},
		{
			name: "soft-deleted deployment returns ErrNotFound",
			setup: func(t *testing.T, d *DB) (string, *Deployment) {
				t.Helper()
				m := mustCreateModel(t, d, "gpt-4-upd-deleted")
				dep := mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID:  m.ID,
					Name:     "deleted-upd-dep",
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Weight:   1,
				})
				if err := d.DeleteDeployment(context.Background(), dep.ID); err != nil {
					t.Fatalf("DeleteDeployment(): %v", err)
				}
				return dep.ID, dep
			},
			params:  UpdateDeploymentParams{Name: ptr("still-gone")},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			id, original := tc.setup(t, d)
			got, err := d.UpdateDeployment(context.Background(), id, tc.params)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("UpdateDeployment() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, original, got)
			}
		})
	}
}

func TestUpdateDeployment_NameConflict(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	m := mustCreateModel(t, d, "gpt-4-upd-conflict")
	mustCreateDeployment(t, d, CreateDeploymentParams{
		ModelID:  m.ID,
		Name:     "existing-dep",
		Provider: "openai",
		BaseURL:  "https://api.openai.com/v1",
		Weight:   1,
	})
	second := mustCreateDeployment(t, d, CreateDeploymentParams{
		ModelID:  m.ID,
		Name:     "second-dep",
		Provider: "openai",
		BaseURL:  "https://api.openai.com/v1",
		Weight:   1,
	})

	_, err := d.UpdateDeployment(ctx, second.ID, UpdateDeploymentParams{Name: ptr("existing-dep")})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("UpdateDeployment() duplicate name error = %v, want ErrConflict", err)
	}
}

// ---- DeleteDeployment -------------------------------------------------------

func TestDeleteDeployment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, d *DB) string
		wantErr error
	}{
		{
			name: "delete existing deployment sets deleted_at and GetDeployment returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				m := mustCreateModel(t, d, "gpt-4-del")
				dep := mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID:  m.ID,
					Name:     "to-delete-dep",
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Weight:   1,
				})
				return dep.ID
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
			name: "delete already-deleted deployment returns ErrNotFound",
			setup: func(t *testing.T, d *DB) string {
				t.Helper()
				m := mustCreateModel(t, d, "gpt-4-double-del")
				dep := mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID:  m.ID,
					Name:     "double-delete-dep",
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Weight:   1,
				})
				if err := d.DeleteDeployment(context.Background(), dep.ID); err != nil {
					t.Fatalf("first DeleteDeployment(): %v", err)
				}
				return dep.ID
			},
			wantErr: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			id := tc.setup(t, d)
			err := d.DeleteDeployment(context.Background(), id)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("DeleteDeployment() error = %v, wantErr %v", err, tc.wantErr)
			}

			// On success, verify GetDeployment returns ErrNotFound and that the
			// row still exists in the DB with deleted_at set (soft delete).
			if tc.wantErr == nil {
				_, getErr := d.GetDeployment(context.Background(), id)
				if !errors.Is(getErr, ErrNotFound) {
					t.Errorf("GetDeployment() after DeleteDeployment() error = %v, want ErrNotFound", getErr)
				}

				var deletedAt *string
				row := d.sql.QueryRowContext(context.Background(),
					"SELECT deleted_at FROM model_deployments WHERE id = ?", id)
				if scanErr := row.Scan(&deletedAt); scanErr != nil {
					t.Fatalf("scan deleted_at: %v", scanErr)
				}
				if deletedAt == nil {
					t.Error("deleted_at is nil after DeleteDeployment(), want a timestamp")
				}
			}
		})
	}
}

// ---- ListDeploymentsByModelIDs ----------------------------------------------

func TestListDeploymentsByModelIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, d *DB) ([]string, map[string]int) // returns model IDs and expected count per model
		wantErr   error
		checkFunc func(t *testing.T, result map[string][]Deployment, modelIDs []string, expectedCounts map[string]int)
	}{
		{
			name: "batch fetch across multiple models",
			setup: func(t *testing.T, d *DB) ([]string, map[string]int) {
				t.Helper()
				m1 := mustCreateModel(t, d, "gpt-4-batch-1")
				m2 := mustCreateModel(t, d, "gpt-4-batch-2")
				m3 := mustCreateModel(t, d, "gpt-4-batch-3")

				mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID: m1.ID, Name: "m1-dep-a", Provider: "openai",
					BaseURL: "https://api.openai.com/v1", Weight: 1,
				})
				mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID: m1.ID, Name: "m1-dep-b", Provider: "openai",
					BaseURL: "https://api.openai.com/v1", Weight: 1,
				})
				mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID: m2.ID, Name: "m2-dep-a", Provider: "openai",
					BaseURL: "https://api.openai.com/v1", Weight: 1,
				})
				// m3 has no deployments.

				return []string{m1.ID, m2.ID, m3.ID}, map[string]int{
					m1.ID: 2,
					m2.ID: 1,
					m3.ID: 0,
				}
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, result map[string][]Deployment, modelIDs []string, expectedCounts map[string]int) {
				t.Helper()
				for modelID, wantCount := range expectedCounts {
					got := result[modelID]
					if len(got) != wantCount {
						t.Errorf("model %q: len(deployments) = %d, want %d", modelID, len(got), wantCount)
					}
				}
			},
		},
		{
			name: "excludes soft-deleted deployments",
			setup: func(t *testing.T, d *DB) ([]string, map[string]int) {
				t.Helper()
				m := mustCreateModel(t, d, "gpt-4-batch-del")
				mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID: m.ID, Name: "live-batch-dep", Provider: "openai",
					BaseURL: "https://api.openai.com/v1", Weight: 1,
				})
				gone := mustCreateDeployment(t, d, CreateDeploymentParams{
					ModelID: m.ID, Name: "gone-batch-dep", Provider: "openai",
					BaseURL: "https://api.openai.com/v1", Weight: 1,
				})
				if err := d.DeleteDeployment(context.Background(), gone.ID); err != nil {
					t.Fatalf("DeleteDeployment(): %v", err)
				}
				return []string{m.ID}, map[string]int{m.ID: 1}
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, result map[string][]Deployment, modelIDs []string, expectedCounts map[string]int) {
				t.Helper()
				for modelID, wantCount := range expectedCounts {
					got := result[modelID]
					if len(got) != wantCount {
						t.Errorf("model %q: len(deployments) = %d, want %d", modelID, len(got), wantCount)
					}
				}
			},
		},
		{
			name: "empty model ID list returns nil map",
			setup: func(t *testing.T, d *DB) ([]string, map[string]int) {
				return []string{}, nil
			},
			wantErr: nil,
			checkFunc: func(t *testing.T, result map[string][]Deployment, modelIDs []string, _ map[string]int) {
				t.Helper()
				if result != nil {
					t.Errorf("ListDeploymentsByModelIDs([]) = %v, want nil", result)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			modelIDs, expectedCounts := tc.setup(t, d)
			result, err := d.ListDeploymentsByModelIDs(context.Background(), modelIDs)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ListDeploymentsByModelIDs() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && tc.checkFunc != nil {
				tc.checkFunc(t, result, modelIDs, expectedCounts)
			}
		})
	}
}

func TestListDeploymentsByModelIDs_PriorityOrder(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	m := mustCreateModel(t, d, "gpt-4-batch-order")

	// Insert in reverse priority order so that ordering by priority ASC gives a
	// different result than insertion order.
	mustCreateDeployment(t, d, CreateDeploymentParams{
		ModelID: m.ID, Name: "batch-low", Provider: "openai",
		BaseURL: "https://api.openai.com/v1", Weight: 1, Priority: 20,
	})
	mustCreateDeployment(t, d, CreateDeploymentParams{
		ModelID: m.ID, Name: "batch-high", Provider: "openai",
		BaseURL: "https://api.openai.com/v1", Weight: 1, Priority: 0,
	})

	result, err := d.ListDeploymentsByModelIDs(ctx, []string{m.ID})
	if err != nil {
		t.Fatalf("ListDeploymentsByModelIDs() error = %v", err)
	}

	deps := result[m.ID]
	if len(deps) != 2 {
		t.Fatalf("len(deps) = %d, want 2", len(deps))
	}
	if deps[0].Priority > deps[1].Priority {
		t.Errorf("deployments not in priority ASC order: deps[0].Priority=%d > deps[1].Priority=%d",
			deps[0].Priority, deps[1].Priority)
	}
}
