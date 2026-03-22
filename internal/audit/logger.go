package audit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
)

// Logger collects audit events from admin API handlers and writes them to the
// database in batches. It is safe for concurrent use. The zero value is not
// usable; construct via NewLogger.
type Logger struct {
	events    chan Event
	database  *db.DB
	log       *slog.Logger
	batchSize int
	interval  time.Duration
	done      chan struct{}
	stopped   chan struct{} // closed by run() when it exits
}

// NewLogger constructs a Logger wired to the given database connection.
// cfg controls the channel buffer size and flush interval.
// log is used to report flush errors without propagating them to callers.
func NewLogger(database *db.DB, cfg config.AuditConfig, log *slog.Logger) *Logger {
	bufferSize := cfg.BufferSize
	if bufferSize <= 0 {
		bufferSize = 500
	}
	interval := cfg.FlushInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}

	return &Logger{
		events:    make(chan Event, bufferSize),
		database:  database,
		log:       log,
		batchSize: bufferSize,
		interval:  interval,
		done:      make(chan struct{}),
		stopped:   make(chan struct{}),
	}
}

// Start launches the background flush goroutine. It must be called exactly once
// before Log is used. Stop must be called to shut the goroutine down cleanly.
func (l *Logger) Start() {
	go l.run()
}

// Stop signals the background goroutine to stop, drains any remaining events,
// and blocks until the goroutine has fully exited. This guarantees all buffered
// events have been flushed to the database before the caller proceeds.
func (l *Logger) Stop() {
	close(l.done)
	<-l.stopped
}

// Log enqueues an event for async persistence. It is non-blocking: if the
// internal buffer is full the event is silently dropped rather than blocking
// the caller, keeping the admin API responsive under flush pressure.
func (l *Logger) Log(event Event) {
	select {
	case l.events <- event:
	default:
		l.log.LogAttrs(context.Background(), slog.LevelWarn,
			"audit logger buffer full, dropping event",
			slog.String("action", event.Action),
			slog.String("resource_type", event.ResourceType),
			slog.String("actor_id", event.ActorID),
		)
	}
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
				"audit flush failed",
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

// flush writes a batch of audit events to the database inside a single
// transaction. Each event receives a new UUIDv7 ID during flush; callers do
// not assign IDs. Timestamps are stored as RFC3339 UTC strings to match the
// audit_logs schema.
func (l *Logger) flush(events []Event) error {
	p := l.database.Dialect().Placeholder

	query := "INSERT INTO audit_logs " +
		"(id, timestamp, org_id, actor_id, actor_type, actor_key_id, " +
		"action, resource_type, resource_id, description, ip_address, status_code, request_id) " +
		"VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " +
		p(5) + ", " + p(6) + ", " + p(7) + ", " + p(8) + ", " +
		p(9) + ", " + p(10) + ", " + p(11) + ", " + p(12) + ", " + p(13) + ")"

	ctx := context.Background()

	return l.database.WithTx(ctx, func(q db.Querier) error {
		for _, ev := range events {
			id, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("audit flush: generate id: %w", err)
			}

			ts := ev.Timestamp.UTC().Format(time.RFC3339)

			_, err = q.ExecContext(ctx, query,
				id.String(),
				ts,
				ev.OrgID,
				ev.ActorID,
				ev.ActorType,
				ev.ActorKeyID,
				ev.Action,
				ev.ResourceType,
				ev.ResourceID,
				ev.Description,
				ev.IPAddress,
				ev.StatusCode,
				ev.RequestID,
			)
			if err != nil {
				return fmt.Errorf("audit flush: insert event: %w", err)
			}
		}
		return nil
	})
}
