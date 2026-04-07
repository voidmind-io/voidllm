// Package retention implements periodic deletion of old usage and audit records
// based on configurable per-table retention durations. See issue #46.
package retention

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
)

// defaultBatchSize is the maximum number of rows deleted in a single
// statement. Small enough to avoid holding a write lock on SQLite for long,
// large enough to make sustained cleanup efficient.
const defaultBatchSize = 10000

// defaultBetweenBatch is the pause between consecutive batches within a
// single cleanup pass. Yields to other writers on SQLite.
const defaultBetweenBatch = 100 * time.Millisecond

// runTimeout caps how long a single cleanup pass may run end-to-end.
const runTimeout = 10 * time.Minute

// Cleaner periodically deletes old rows from usage_events and audit_logs.
// Disabled configurations produce a zero-cost no-op: Start logs once and
// returns without launching any background goroutine.
type Cleaner struct {
	db  *db.DB
	cfg config.RetentionConfig
	log *slog.Logger

	// batchSize and betweenBatch are package-private so tests can override them
	// without requiring exported option functions.
	batchSize    int
	betweenBatch time.Duration

	done     chan struct{}
	stopOnce sync.Once
}

// New constructs a Cleaner. It does not start the background loop - call Start.
func New(database *db.DB, cfg config.RetentionConfig, log *slog.Logger) *Cleaner {
	if log == nil {
		log = slog.Default()
	}
	return &Cleaner{
		db:           database,
		cfg:          cfg,
		log:          log.With(slog.String("component", "retention")),
		batchSize:    defaultBatchSize,
		betweenBatch: defaultBetweenBatch,
	}
}

// Start begins the background cleanup ticker. If retention is disabled for
// all tables, Start logs a single info message and returns. Safe to call once.
func (c *Cleaner) Start() {
	if !c.cfg.Enabled() {
		c.log.Info("data retention disabled (all retention durations are zero)")
		return
	}

	c.log.Info("data retention enabled",
		slog.Duration("usage_events", c.cfg.UsageEvents),
		slog.Duration("audit_logs", c.cfg.AuditLogs),
		slog.Duration("interval", c.cfg.Interval),
	)

	c.done = make(chan struct{})

	go func() {
		// Bail immediately if Stop was called before the goroutine started.
		select {
		case <-c.done:
			return
		default:
		}

		// Initial run: gives operators immediate feedback and trims any
		// existing backlog on a newly-enabled deployment.
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
			defer cancel()
			c.runOnce(ctx)
		}()

		ticker := time.NewTicker(c.cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
				c.runOnce(ctx)
				cancel()
			case <-c.done:
				return
			}
		}
	}()
}

// Stop halts the cleanup ticker. Safe to call multiple times or without Start.
func (c *Cleaner) Stop() {
	if c.done == nil {
		return
	}
	c.stopOnce.Do(func() { close(c.done) })
}

// runOnce performs a single cleanup pass across all enabled tables.
// Errors for one table do not abort the other.
func (c *Cleaner) runOnce(ctx context.Context) {
	if c.cfg.UsageEvents > 0 {
		c.runTable(ctx, "usage_events", "created_at", c.cfg.UsageEvents)
	}
	if c.cfg.AuditLogs > 0 {
		c.runTable(ctx, "audit_logs", "timestamp", c.cfg.AuditLogs)
	}
}

func (c *Cleaner) runTable(ctx context.Context, table, tsColumn string, maxAge time.Duration) {
	start := time.Now()
	cutoff := start.UTC().Add(-maxAge)
	deleted, err := c.cleanupTable(ctx, table, tsColumn, cutoff)
	attrs := []slog.Attr{
		slog.String("table", table),
		slog.Time("cutoff", cutoff),
		slog.Int64("rows_deleted", deleted),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
		c.log.LogAttrs(ctx, slog.LevelError, "retention cleanup failed", attrs...)
		return
	}
	level := slog.LevelDebug
	if deleted > 0 {
		level = slog.LevelInfo
	}
	c.log.LogAttrs(ctx, level, "retention cleanup complete", attrs...)
}

// cleanupTable deletes rows from table where tsColumn is older than cutoff,
// in batches of c.batchSize. Returns the total number of rows deleted, plus
// any error that aborted the loop. A successful partial deletion is reported
// as (partial_count, nil) when ctx is cancelled between batches.
//
// The subselect form "DELETE FROM t WHERE id IN (SELECT id FROM t WHERE ... LIMIT n)"
// is used because a bare DELETE ... LIMIT requires a SQLite build flag that is
// not guaranteed to be present.
func (c *Cleaner) cleanupTable(ctx context.Context, table, tsColumn string, cutoff time.Time) (int64, error) {
	ph := c.db.Dialect().Placeholder(1)
	predicate := c.db.Dialect().TimestampLessThan(tsColumn, ph)
	query := fmt.Sprintf(
		"DELETE FROM %s WHERE id IN (SELECT id FROM %s WHERE %s LIMIT %d)",
		table, table, predicate, c.batchSize,
	)

	cutoffStr := cutoff.UTC().Format(time.RFC3339Nano)

	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, nil // partial success, context cancelled
		}
		res, err := c.db.SQL().ExecContext(ctx, query, cutoffStr)
		if err != nil {
			if ctx.Err() != nil {
				// Context was cancelled or timed out during the statement; treat
				// as partial success — same semantics as the pre-loop check above.
				return total, nil
			}
			return total, fmt.Errorf("delete from %s: %w", table, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("rows affected for %s: %w", table, err)
		}
		total += affected
		if affected < int64(c.batchSize) {
			return total, nil
		}
		if c.betweenBatch > 0 {
			select {
			case <-time.After(c.betweenBatch):
			case <-ctx.Done():
				return total, nil
			}
		}
	}
}
