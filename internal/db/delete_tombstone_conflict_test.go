package db

// Regression test for the pathological soft-delete/tombstone collision noted
// in the #172 follow-up review: a legacy ACTIVE row whose unique column value
// is exactly "<value>:deleted:<victim row's id>" (only possible pre-upgrade,
// since the Admin API now rejects the ":deleted:" marker in user input) makes
// the tombstoning UPDATE in Delete* collide with that legacy row's UNIQUE
// constraint. This must surface as ErrConflict, not a generic/opaque error.
//
// Only the models entity is covered here; DeleteUser/DeleteOrg/DeleteTeam/
// DeleteDeployment share the exact same translateError-based mapping, so one
// representative case is sufficient to prove the mapping works end to end.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestDeleteModel_LegacyTombstoneCollision_ReturnsErrConflict seeds an active
// row whose name already occupies the tombstone value that DeleteModel would
// write for the victim row, then asserts that deleting the victim returns
// ErrConflict rather than a generic/internal error.
func TestDeleteModel_LegacyTombstoneCollision_ReturnsErrConflict(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	ctx := context.Background()

	victim := mustCreateModel(t, d, "tombstone-collision-model")
	tombstoneValue := "tombstone-collision-model:deleted:" + victim.ID

	// Seed a legacy active row occupying the exact tombstone value the
	// pending delete will try to write. This shape bypasses the API's guard
	// against ":deleted:"-suffixed names by inserting directly via raw SQL,
	// simulating data left over from before that guard existed.
	legacyID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	if _, err := d.sql.ExecContext(ctx,
		"INSERT INTO models (id, name, provider, base_url, is_active, source, created_at, updated_at) "+
			"VALUES (?, ?, 'openai', 'https://api.openai.com/v1', 1, 'api', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)",
		legacyID.String(), tombstoneValue,
	); err != nil {
		t.Fatalf("seed legacy active row occupying tombstone value: %v", err)
	}

	err = d.DeleteModel(ctx, victim.ID)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("DeleteModel() with a colliding legacy tombstone row = %v, want ErrConflict", err)
	}

	// The victim row must remain untouched (still active, original name)
	// since the UPDATE failed and was not partially applied.
	got := rawColumnValue(t, d, "models", "name", victim.ID)
	if got != "tombstone-collision-model" {
		t.Errorf("victim row name after failed delete = %q, want unchanged %q", got, "tombstone-collision-model")
	}
}
