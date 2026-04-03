package license

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// SettingsReadWriter is the subset of the database interface needed to
// read and write settings. Using an interface avoids circular imports.
type SettingsReadWriter interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
	SetSettingIfNotExists(ctx context.Context, key, value string) error
}

// GetOrCreateInstanceID reads the instance UUID from the settings table.
// If none exists, generates a new UUIDv7 and persists it atomically using
// INSERT ... ON CONFLICT DO NOTHING so concurrent pods converge on a single
// winner without a TOCTOU race. The instance ID is stable across restarts
// and shared by all pods using the same database.
func GetOrCreateInstanceID(ctx context.Context, db SettingsReadWriter) (string, error) {
	// Try reading existing ID first.
	id, err := db.GetSetting(ctx, "instance_id")
	if err != nil {
		return "", fmt.Errorf("get instance ID: %w", err)
	}
	if id != "" {
		return id, nil
	}

	// Generate new UUIDv7.
	v7, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate instance ID: %w", err)
	}

	// Write only if not yet set (another pod may have written concurrently).
	if err := db.SetSettingIfNotExists(ctx, "instance_id", v7.String()); err != nil {
		return "", fmt.Errorf("persist instance ID: %w", err)
	}

	// Re-read to get the winning value (may differ from ours if another pod won).
	id, err = db.GetSetting(ctx, "instance_id")
	if err != nil {
		return "", fmt.Errorf("re-read instance ID: %w", err)
	}
	return id, nil
}
