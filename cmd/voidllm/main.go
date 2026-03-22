package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/app"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/logger"
)

func main() {
	// Subcommand detection: handle subcommands before flag.Parse so that
	// subcommand-specific flags are not parsed by the top-level flag set.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			runMigrate(os.Args[2:])
			return
		case "license":
			runLicense(os.Args[2:])
			return
		}
	}

	configPath := flag.String("config", "", "path to voidllm.yaml config file")
	devMode := flag.Bool("dev", false, "enable development mode (pprof, debug logging, CORS *)")
	flag.Parse()

	if ok, _ := strconv.ParseBool(os.Getenv("VOIDLLM_DEV")); ok {
		*devMode = true
	}

	// Load configuration. Errors here use fmt.Fprintf because the logger has
	// not yet been initialized.
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "voidllm: failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Dev mode forces debug log level so all diagnostic output is visible.
	if *devMode {
		cfg.Logging.Level = "debug"
	}

	// Initialize the structured logger, wrap it with RequestIDHandler for
	// automatic request_id propagation, and install it as the global default
	// so that any code calling slog.Default() gets request correlation for free.
	log := slog.New(logger.NewRequestIDHandler(logger.New(cfg.Logging, os.Stdout).Handler(), apierror.RequestIDFromGoCtx))
	slog.SetDefault(log)

	if *devMode {
		log.Warn("========================================")
		log.Warn("DEVELOPMENT MODE ENABLED")
		log.Warn("CORS *, pprof :6060, debug logging active")
		log.Warn("Do NOT use in production")
		log.Warn("========================================")
	}

	application, err := app.New(cfg, log, *devMode)
	if err != nil {
		log.Error("startup failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	if err := application.Start(); err != nil {
		log.Error("server start failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	application.WaitForShutdown(context.Background())
}
