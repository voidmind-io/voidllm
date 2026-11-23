package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/voidmind-io/voidllm/internal/db"
)

// usageHourlyBackfillTimeout bounds a single backfill attempt. A run that
// exceeds it is abandoned without setting the completion marker and retried on
// the next start.
const usageHourlyBackfillTimeout = 30 * time.Minute

// startUsageHourlyBackfill runs the one-time usage_hourly backfill in a
// background goroutine so startup is never blocked by it. The returned stop
// function cancels the run and waits for the goroutine to exit.
func startUsageHourlyBackfill(database *db.DB, log *slog.Logger) func() {
	ctx, cancel := context.WithTimeout(context.Background(), usageHourlyBackfillTimeout)
	done := make(chan struct{})

	go func() {
		defer close(done)
		start := time.Now()
		if done, err := database.GetSetting(ctx, db.UsageHourlyBackfillMarker); err == nil && done != "" {
			return
		}
		log.LogAttrs(ctx, slog.LevelInfo, "usage hourly backfill starting")
		rows, ran, err := database.BackfillUsageHourlyOnce(ctx, start)
		switch {
		case err != nil:
			log.LogAttrs(ctx, slog.LevelWarn, "usage hourly backfill failed, will retry on next start",
				slog.String("error", err.Error()),
				slog.Duration("elapsed", time.Since(start)),
			)
		case ran:
			log.LogAttrs(ctx, slog.LevelInfo, "usage hourly backfill completed",
				slog.Int64("buckets_inserted", rows),
				slog.Duration("elapsed", time.Since(start)),
			)
		}
	}()

	return func() {
		cancel()
		<-done
	}
}
