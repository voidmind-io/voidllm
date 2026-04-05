package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/logger"
)

// runMigrateSchema is the entry point for the "migrate-schema" subcommand.
// It opens the configured database, acquires an advisory lock when running
// against PostgreSQL, applies all pending schema migrations, then exits.
// This is intended for use as a pre-upgrade Helm hook in multi-replica
// PostgreSQL deployments to ensure migrations run exactly once.
func runMigrateSchema(args []string) {
	fs := flag.NewFlagSet("migrate-schema", flag.ExitOnError)
	configPath := fs.String("config", "", "path to voidllm.yaml config file")
	fs.Parse(args) //nolint:errcheck // ExitOnError handles the error

	cfg, _, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "voidllm migrate-schema: failed to load config: %v\n", err)
		exitWithPause(1)
	}

	log := slog.New(logger.New(cfg.Logging, os.Stdout).Handler())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	database, err := db.OpenAndMigrate(ctx, cfg.Database, log)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "migration failed",
			slog.String("error", err.Error()))
		exitWithPause(1)
	}
	database.Close() //nolint:errcheck // exiting immediately after

	log.LogAttrs(ctx, slog.LevelInfo, "schema migrations complete")
}
