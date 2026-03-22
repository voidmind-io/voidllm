package usage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/ratelimit"
)

// rollupKey identifies a unique (key, model, hour) aggregation bucket.
type rollupKey struct {
	KeyID      string
	ModelName  string
	BucketHour string
}

// Logger collects usage events from the proxy hot path and writes them to the
// database in batches. It is safe for concurrent use. The zero value is not
// usable; construct via NewLogger.
type Logger struct {
	events       chan Event
	database     *db.DB
	log          *slog.Logger
	batchSize    int
	interval     time.Duration
	dropOnFull   bool
	done         chan struct{}
	stopped      chan struct{} // closed by run() when it exits
	tokenCounter *ratelimit.TokenCounter // nil disables in-memory token counting
}

// NewLogger constructs a Logger wired to the given database connection.
// cfg controls the channel buffer size, flush interval, and drop-on-full
// behaviour. log is used to report flush errors without propagating them.
// tokenCounter, when non-nil, is incremented immediately in Log() before the
// event is enqueued so that CheckTokens sees up-to-date counts without waiting
// for the async DB flush.
func NewLogger(database *db.DB, cfg config.UsageConfig, log *slog.Logger, tokenCounter *ratelimit.TokenCounter) *Logger {
	bufferSize := cfg.BufferSize
	if bufferSize <= 0 {
		bufferSize = 1000
	}
	interval := cfg.FlushInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}

	return &Logger{
		events:       make(chan Event, bufferSize),
		database:     database,
		log:          log,
		batchSize:    bufferSize,
		interval:     interval,
		dropOnFull:   cfg.ShouldDropOnFull(),
		done:         make(chan struct{}),
		stopped:      make(chan struct{}),
		tokenCounter: tokenCounter,
	}
}

// Start launches the background flush goroutine. It must be called exactly once
// before Log is used. Stop must be called to shut the goroutine down cleanly.
func (l *Logger) Start() {
	go l.run()
}

// Stop signals the background goroutine to stop, flushes any remaining events,
// and blocks until the goroutine has fully exited. This guarantees all buffered
// events have been flushed to the database before the caller proceeds.
func (l *Logger) Stop() {
	close(l.done)
	<-l.stopped
}

// BufferLen returns the number of events currently pending in the internal
// buffer channel. It is intended for instrumentation and does not block.
func (l *Logger) BufferLen() int {
	return len(l.events)
}

// Log enqueues an event for async persistence. When a TokenCounter is wired it
// is incremented immediately — before the channel send — so that in-flight
// token budget checks see the most current totals without waiting for the DB
// flush. When the internal buffer is full and dropOnFull is true, the event is
// silently dropped rather than blocking the caller. This ensures the proxy hot
// path is never delayed by a slow DB.
func (l *Logger) Log(event Event) {
	// Increment the in-memory token counter immediately so that subsequent
	// CheckTokens calls reflect this request even before it reaches the DB.
	if l.tokenCounter != nil && event.TotalTokens > 0 {
		l.tokenCounter.Add(event.KeyID, event.TeamID, event.OrgID, int64(event.TotalTokens))
	}

	if l.dropOnFull {
		select {
		case l.events <- event:
		default:
			l.log.LogAttrs(context.Background(), slog.LevelWarn,
				"usage logger buffer full, dropping event",
				slog.String("model", event.ModelName),
				slog.String("key_id", event.KeyID),
			)
		}
		return
	}
	l.events <- event
}

// run is the background flush loop. It accumulates events and flushes them
// either when the batch is full or when the ticker fires, whichever comes first.
// When done is closed it drains any remaining events before returning.
func (l *Logger) run() {
	defer close(l.stopped)

	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()

	batch := make([]Event, 0, l.batchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := l.flush(batch); err != nil {
			l.log.LogAttrs(context.Background(), slog.LevelError,
				"usage flush failed",
				slog.Int("count", len(batch)),
				slog.String("error", err.Error()),
			)
		}
		batch = batch[:0]
	}

	for {
		select {
		case ev := <-l.events:
			batch = append(batch, ev)
			if len(batch) >= l.batchSize {
				flush()
			}

		case <-ticker.C:
			flush()

		case <-l.done:
			// Drain any events already in the channel before exiting.
			for {
				select {
				case ev := <-l.events:
					batch = append(batch, ev)
				default:
					flush()
					return
				}
			}
		}
	}
}

// flush writes a batch of events to the database inside a single transaction.
// Each event receives a new UUIDv7 ID. Nullable fields (TeamID, UserID,
// ServiceAccountID, CostEstimate, TTFT_MS, TokensPerSecond) are stored as SQL
// NULL when empty or nil.
func (l *Logger) flush(events []Event) error {
	p := l.database.Dialect().Placeholder

	query := "INSERT INTO usage_events " +
		"(id, key_id, key_type, org_id, team_id, user_id, service_account_id, " +
		"model_name, prompt_tokens, completion_tokens, total_tokens, " +
		"cost_estimate, request_duration_ms, ttft_ms, tokens_per_second, status_code, request_id) " +
		"VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " +
		p(5) + ", " + p(6) + ", " + p(7) + ", " + p(8) + ", " +
		p(9) + ", " + p(10) + ", " + p(11) + ", " +
		p(12) + ", " + p(13) + ", " + p(14) + ", " + p(15) + ", " + p(16) + ", " + p(17) + ")"

	ctx := context.Background()

	if err := l.database.WithTx(ctx, func(q db.Querier) error {
		for _, ev := range events {
			id, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("usage flush: generate id: %w", err)
			}

			teamID := nullableString(ev.TeamID)
			userID := nullableString(ev.UserID)
			saID := nullableString(ev.ServiceAccountID)

			_, err = q.ExecContext(ctx, query,
				id.String(),
				ev.KeyID,
				ev.KeyType,
				ev.OrgID,
				teamID,
				userID,
				saID,
				ev.ModelName,
				ev.PromptTokens,
				ev.CompletionTokens,
				ev.TotalTokens,
				ev.CostEstimate,
				ev.DurationMS,
				ev.TTFT_MS,
				ev.TokensPerSecond,
				ev.StatusCode,
				ev.RequestID,
			)
			if err != nil {
				return fmt.Errorf("usage flush: insert event: %w", err)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// Aggregate the flushed events into hourly rollup buckets. The bucket hour
	// is derived from the current wall clock time, which is accurate to within
	// the flush interval (default 5s) — sufficient precision for hour granularity.
	// Rollup failures are logged but do not fail the flush; raw usage_events
	// remain the source of truth and rollups can be recomputed from them.
	rollups := make(map[rollupKey]*db.HourlyRollup)
	bucketHour := time.Now().UTC().Truncate(time.Hour).Format("2006-01-02T15:00:00Z")

	for _, ev := range events {
		key := rollupKey{KeyID: ev.KeyID, ModelName: ev.ModelName, BucketHour: bucketHour}
		r, exists := rollups[key]
		if !exists {
			r = &db.HourlyRollup{
				OrgID:      ev.OrgID,
				TeamID:     ev.TeamID,
				UserID:     ev.UserID,
				KeyID:      ev.KeyID,
				ModelName:  ev.ModelName,
				BucketHour: bucketHour,
			}
			rollups[key] = r
		}
		r.RequestCount++
		r.PromptTokens += ev.PromptTokens
		r.CompletionTokens += ev.CompletionTokens
		r.TotalTokens += ev.TotalTokens
		if ev.CostEstimate != nil {
			r.CostSum += *ev.CostEstimate
		}
		r.DurationSumMS += float64(ev.DurationMS)
		if ev.TTFT_MS != nil {
			r.TTFTSumMS += float64(*ev.TTFT_MS)
			r.TTFTCount++
		}
	}

	for _, r := range rollups {
		if err := l.database.UpsertUsageHourly(ctx, *r); err != nil {
			l.log.LogAttrs(ctx, slog.LevelError, "upsert hourly rollup failed",
				slog.String("key_id", r.KeyID),
				slog.String("model", r.ModelName),
				slog.String("bucket_hour", r.BucketHour),
				slog.String("error", err.Error()),
			)
		}
	}

	return nil
}

// nullableString converts an empty string to nil so it is stored as SQL NULL.
// A non-empty string is returned as-is.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
