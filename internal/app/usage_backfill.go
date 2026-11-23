package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/voidmind-io/voidllm/internal/db"
)

// usageHourlyBackfillTimeout bounds a single backfill attempt. Progress is
// persisted month by month, so a run that exceeds it resumes on the next start.
const usageHourlyBackfillTimeout = 30 * time.Minute

// startUsageHourlyBackfill runs the one-time usage_hourly backfill in a
// background goroutine so startup is never blocked by it. Only hours before the
// hour in which the process started are backfilled. The returned stop function
// cancels the run and waits for the goroutine to exit.
func startUsageHourlyBackfill(database *db.DB, log *slog.Logger, processStart time.Time) func() {
	ctx, cancel := context.WithTimeout(context.Background(), usageHourlyBackfillTimeout)
	done := make(chan struct{})

	go func() {
		defer close(done)
		began := time.Now()
		rows, ran, err := database.BackfillUsageHourlyOnce(ctx, processStart)
		switch {
		case err != nil:
			log.LogAttrs(ctx, slog.LevelWarn, "usage hourly backfill failed, will resume on next start",
				slog.String("error", err.Error()),
				slog.Duration("elapsed", time.Since(began)),
			)
		case ran:
			log.LogAttrs(ctx, slog.LevelInfo, "usage hourly backfill completed",
				slog.Int64("buckets_inserted", rows),
				slog.Duration("elapsed", time.Since(began)),
			)
		}
	}()

	return func() {
		cancel()
		<-done
	}
}
