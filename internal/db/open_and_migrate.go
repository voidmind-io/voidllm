package db

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/voidmind-io/voidllm/internal/config"
)

// OpenAndMigrate opens the database and runs pending schema migrations.
// If migrations fail, the database connection is closed before returning the error.
func OpenAndMigrate(ctx context.Context, cfg config.DatabaseConfig, log *slog.Logger) (*DB, error) {
	database, err := Open(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := RunMigrations(ctx, database.SQL(), database.Dialect(), log); err != nil {
		database.Close() //nolint:errcheck // best-effort cleanup on failure path
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	return database, nil
}
