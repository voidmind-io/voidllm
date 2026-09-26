package usage

// End-to-end regressions for #226 through the real Logger write path: stored
// created_at must equal the event's own timestamp (not the time the batch
// happened to flush), the daily token counter must see today's events after a
// fresh Seed (the "budget resets on restart" regression), and events that
// land in different hours within a single flush must produce two separate
// usage_hourly buckets.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/ratelimit"
)

// dbUsageSeeder adapts *db.DB to ratelimit.UsageSeeder for this test file,
// mirroring the identical adapter in internal/app/app.go (dbUsageSeeder) —
// duplicated here because that one is unexported in a different package.
type dbUsageSeeder struct{ d *db.DB }

func (s dbUsageSeeder) QueryUsageSeed(ctx context.Context, since time.Time) (ratelimit.RowScanner, error) {
	return s.d.QueryUsageSeed(ctx, since)
}

// TestLogger_StoredCreatedAtEqualsEventTime verifies that usage_events.created_at
// reflects each event's own CreatedAt — set well before the flush actually
// happens — rather than the wall-clock time of the flush itself. One event is
// stamped today at 00:00:05 UTC and another yesterday at 23:59:59 UTC, dates
// deliberately chosen to sit right at a day boundary.
func TestLogger_StoredCreatedAtEqualsEventTime(t *testing.T) {
	t.Parallel()

	d := openTestDB(t, "file:TestLogger_StoredCreatedAtEqualsEventTime?mode=memory&cache=private")
	l := newTestLogger(d, defaultCfg())
	l.Start()

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 5, 0, time.UTC)
	yesterdayDate := now.AddDate(0, 0, -1)
	yesterday := time.Date(yesterdayDate.Year(), yesterdayDate.Month(), yesterdayDate.Day(), 23, 59, 59, 0, time.UTC)

	todayEvent := makeEvent()
	todayEvent.KeyID = "time-today"
	todayEvent.CreatedAt = today

	yesterdayEvent := makeEvent()
	yesterdayEvent.KeyID = "time-yesterday"
	yesterdayEvent.CreatedAt = yesterday

	l.Log(todayEvent)
	l.Log(yesterdayEvent)
	stopAndWait(t, l, d, 2)

	ctx := context.Background()
	var gotToday, gotYesterday string
	if err := d.SQL().QueryRowContext(ctx, "SELECT created_at FROM usage_events WHERE key_id = ?", "time-today").Scan(&gotToday); err != nil {
		t.Fatalf("read today's created_at: %v", err)
	}
	if err := d.SQL().QueryRowContext(ctx, "SELECT created_at FROM usage_events WHERE key_id = ?", "time-yesterday").Scan(&gotYesterday); err != nil {
		t.Fatalf("read yesterday's created_at: %v", err)
	}

	if want := db.FormatTimestamp(today); gotToday != want {
		t.Errorf("today's created_at = %q, want %q (event time, not flush time)", gotToday, want)
	}
	if want := db.FormatTimestamp(yesterday); gotYesterday != want {
		t.Errorf("yesterday's created_at = %q, want %q (event time, not flush time)", gotYesterday, want)
	}
}

// TestLogger_TokenCounterSeed_CountsTodayEventAfterRestart reproduces the
// #226 "daily token budget resets on restart" regression end-to-end: an event
// is logged and flushed to a real database with an explicit CreatedAt set to
// today (a date chosen relative to the actual time.Now() so the test is not
// sensitive to when it happens to run, including near a calendar boundary),
// then a brand new TokenCounter — standing in for a freshly restarted
// process — is seeded from that same database. The daily counter must
// already reflect the persisted event's tokens.
func TestLogger_TokenCounterSeed_CountsTodayEventAfterRestart(t *testing.T) {
	t.Parallel()

	d := openTestDB(t, "file:TestLogger_TokenCounterSeed_CountsTodayEventAfterRestart?mode=memory&cache=private")
	l := newTestLogger(d, defaultCfg())
	l.Start()

	now := time.Now().UTC()
	todayEarly := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 5, 0, time.UTC)

	ev := makeEvent()
	ev.KeyID = "seed-restart-key"
	ev.OrgID = "seed-restart-org"
	ev.TotalTokens = 500
	ev.CreatedAt = todayEarly

	l.Log(ev)
	stopAndWait(t, l, d, 1)

	// Simulate a process restart: a brand new TokenCounter with no in-memory
	// state, seeded fresh from the database Logger just wrote to.
	restarted := ratelimit.NewTokenCounter()
	if err := restarted.Seed(context.Background(), dbUsageSeeder{d}); err != nil {
		t.Fatalf("Seed() error = %v, want nil", err)
	}

	scopes := ratelimit.Scopes{KeyID: "seed-restart-key", OrgID: "seed-restart-org"}
	limits := ratelimit.ScopeLimits{
		Key: ratelimit.Limits{DailyTokenLimit: 500},
	}
	if err := restarted.CheckTokens(scopes, limits); err == nil {
		t.Error("CheckTokens() after Seed() = nil, want ErrTokenBudgetExceeded (500 tokens already spent today, budget is 500)")
	}

	// A budget just above the already-spent total must still have headroom,
	// confirming the counter counts exactly the persisted total rather than
	// e.g. treating every key as perpetually over budget.
	roomyLimits := ratelimit.ScopeLimits{Key: ratelimit.Limits{DailyTokenLimit: 1000}}
	if err := restarted.CheckTokens(scopes, roomyLimits); err != nil {
		t.Errorf("CheckTokens() with headroom after Seed() = %v, want nil", err)
	}
}

// TestLogger_TwoEventsDifferentHours_TwoHourlyBuckets verifies that two
// events logged in the same flush batch, but stamped an hour apart, produce
// two separate usage_hourly rollup rows rather than being collapsed into a
// single bucket keyed by the flush's own wall-clock time.
func TestLogger_TwoEventsDifferentHours_TwoHourlyBuckets(t *testing.T) {
	t.Parallel()

	d := openTestDB(t, "file:TestLogger_TwoEventsDifferentHours_TwoHourlyBuckets?mode=memory&cache=private")
	l := newTestLogger(d, defaultCfg()) // long flush interval: both events sit in the buffer until Stop()
	l.Start()

	base := time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC)

	first := makeEvent()
	first.KeyID = "bucket-key"
	first.CreatedAt = base

	second := makeEvent()
	second.KeyID = "bucket-key"
	second.CreatedAt = base.Add(2 * time.Hour)

	l.Log(first)
	l.Log(second)
	stopAndWait(t, l, d, 2) // both flushed together by Stop()'s drain path

	var bucketCount int
	if err := d.SQL().QueryRowContext(context.Background(),
		"SELECT COUNT(DISTINCT bucket_hour) FROM usage_hourly WHERE key_id = ?", "bucket-key",
	).Scan(&bucketCount); err != nil {
		t.Fatalf("count usage_hourly buckets: %v", err)
	}
	if bucketCount != 2 {
		t.Errorf("distinct usage_hourly buckets for key = %d, want 2", bucketCount)
	}

	var gotFirst, gotSecond string
	if err := d.SQL().QueryRowContext(context.Background(),
		"SELECT bucket_hour FROM usage_hourly WHERE key_id = ? AND bucket_hour = ?",
		"bucket-key", db.FormatTimestamp(base.Truncate(time.Hour)),
	).Scan(&gotFirst); err != nil {
		t.Errorf("bucket for first event not found at %q: %v", db.FormatTimestamp(base.Truncate(time.Hour)), err)
	}
	if err := d.SQL().QueryRowContext(context.Background(),
		"SELECT bucket_hour FROM usage_hourly WHERE key_id = ? AND bucket_hour = ?",
		"bucket-key", db.FormatTimestamp(second.CreatedAt.Truncate(time.Hour)),
	).Scan(&gotSecond); err != nil {
		t.Errorf("bucket for second event not found at %q: %v", db.FormatTimestamp(second.CreatedAt.Truncate(time.Hour)), err)
	}
}

// TestMCPLogger_StoredCreatedAtCanonicalAndEqualsEventTime verifies that
// MCPLogger.Log, which stamps CreatedAt when left zero, persists a canonical
// created_at that matches the event's own time — not merely "canonical
// shaped", but the exact value FormatTimestamp would produce for the
// CreatedAt actually used.
func TestMCPLogger_StoredCreatedAtCanonicalAndEqualsEventTime(t *testing.T) {
	t.Parallel()

	d := openTestDB(t, "file:TestMCPLogger_StoredCreatedAtCanonicalAndEqualsEventTime?mode=memory&cache=private")
	l := NewMCPLogger(d, 100, slog.New(slog.NewTextHandler(io.Discard, nil)))

	eventTime := time.Date(2026, 9, 26, 21, 23, 17, 0, time.UTC)
	l.Log(MCPToolCallEvent{
		KeyID:       "mcp-time-key",
		KeyType:     "user_key",
		OrgID:       "mcp-time-org",
		ServerAlias: "server-a",
		ToolName:    "tool-a",
		Status:      "success",
		CreatedAt:   eventTime,
	})
	l.Stop()

	var got string
	if err := d.SQL().QueryRowContext(context.Background(),
		"SELECT created_at FROM mcp_tool_calls WHERE key_id = ?", "mcp-time-key",
	).Scan(&got); err != nil {
		t.Fatalf("read created_at: %v", err)
	}
	if want := db.FormatTimestamp(eventTime); got != want {
		t.Errorf("mcp_tool_calls.created_at = %q, want %q", got, want)
	}
}
