package db

// Regression test for the #172 follow-up: the ":deleted:" tombstone marker
// must be rejected by the DB layer itself — the single choke point — rather
// than relying solely on Admin API handler validation. SyncYAMLModels and
// OIDC auto-provisioning call CreateModel/UpdateModel/CreateUser directly,
// bypassing the handler-level checks, so the store functions themselves must
// reject the marker before executing any SQL. See tombstone.go and errors.go.

import (
	"context"
	"errors"
	"testing"
)

// markerName is a value containing the reserved soft-delete tombstone marker.
// It is shared read-only across the table-driven subtests below.
const markerName = "gpt-4:deleted:someid"

// TestCreateAndUpdate_RejectTombstoneMarker is a table-driven test proving
// that every Create* function for the five entities with a tombstone-eligible
// unique column (models.name, users.email, organizations.slug, teams.slug,
// model_deployments.name), plus a representative Update* function, return
// ErrReservedValue when the relevant field contains the marker — without
// going through any Admin API handler.
func TestCreateAndUpdate_RejectTombstoneMarker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T, d *DB) error
	}{
		{
			name: "CreateModel rejects a name containing the marker",
			run: func(t *testing.T, d *DB) error {
				_, err := d.CreateModel(context.Background(), CreateModelParams{
					Name:     markerName,
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Source:   "api",
				})
				return err
			},
		},
		{
			name: "CreateUser rejects an email containing the marker",
			run: func(t *testing.T, d *DB) error {
				_, err := d.CreateUser(context.Background(), CreateUserParams{
					Email:        markerName + "@example.com",
					DisplayName:  "Marker User",
					PasswordHash: testPasswordHash(t),
				})
				return err
			},
		},
		{
			name: "CreateOrg rejects a slug containing the marker",
			run: func(t *testing.T, d *DB) error {
				_, err := d.CreateOrg(context.Background(), CreateOrgParams{
					Name: "Marker Org",
					Slug: markerName,
				})
				return err
			},
		},
		{
			name: "CreateTeam rejects a slug containing the marker",
			run: func(t *testing.T, d *DB) error {
				org := mustCreateOrg(t, d, CreateOrgParams{Name: "Team Marker Org", Slug: "team-marker-org"})
				_, err := d.CreateTeam(context.Background(), CreateTeamParams{
					OrgID: org.ID,
					Name:  "Marker Team",
					Slug:  markerName,
				})
				return err
			},
		},
		{
			name: "CreateDeployment rejects a name containing the marker",
			run: func(t *testing.T, d *DB) error {
				m := mustCreateModel(t, d, "deployment-marker-model")
				_, err := d.CreateDeployment(context.Background(), CreateDeploymentParams{
					ModelID:  m.ID,
					Name:     markerName,
					Provider: "openai",
					BaseURL:  "https://api.openai.com/v1",
					Weight:   1,
				})
				return err
			},
		},
		{
			name: "UpdateModel rejects a new name containing the marker",
			run: func(t *testing.T, d *DB) error {
				m := mustCreateModel(t, d, "update-marker-model")
				_, err := d.UpdateModel(context.Background(), m.ID, UpdateModelParams{
					Name: ptr(markerName),
				})
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)

			err := tc.run(t, d)
			if !errors.Is(err, ErrReservedValue) {
				t.Fatalf("error = %v, want ErrReservedValue", err)
			}
		})
	}
}
