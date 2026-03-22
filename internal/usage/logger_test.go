package usage

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/ratelimit"
)

// openTestDB opens an in-memory SQLite database using the given unique URI,
// runs all migrations, and registers cleanup via t.Cleanup.
func openTestDB(t *testing.T, dsn string) *db.DB {
	t.Helper()

	cfg := config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             dsn,
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	}

	ctx := context.Background()
	d, err := db.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("openTestDB: Open() error = %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if err := db.RunMigrations(ctx, d.SQL(), db.SQLiteDialect{}); err != nil {
		t.Fatalf("openTestDB: RunMigrations() error = %v", err)
	}

	return d
}

// newTestLogger builds a Logger backed by d and starts it. The caller is
// responsible for calling Stop(). The logger discards all internal log output.
// No TokenCounter is wired; token counting is tested separately.
func newTestLogger(d *db.DB, cfg config.UsageConfig) *Logger {
	return NewLogger(d, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

// stopAndWait signals the Logger to stop and then polls usage_events until the
// expected number of rows appear or the deadline passes. It fails the test if
// the count never reaches want within the timeout.
func stopAndWait(t *testing.T, l *Logger, d *db.DB, want int) {
	t.Helper()

	l.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countRows(t, d) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	got := countRows(t, d)
	if got != want {
		t.Errorf("usage_events row count after Stop = %d, want %d", got, want)
	}
}

// countRows returns the total number of rows in usage_events.
func countRows(t *testing.T, d *db.DB) int {
	t.Helper()

	var n int
	err := d.SQL().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM usage_events").Scan(&n)
	if err != nil {
		t.Fatalf("countRows: %v", err)
	}
	return n
}

// waitForRows polls usage_events until count reaches want or the deadline
// passes. It is used by tests that trigger a flush via mechanism other than
// Stop (e.g. batch-full or timer).
func waitForRows(t *testing.T, d *db.DB, want int) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countRows(t, d) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := countRows(t, d)
	if got != want {
		t.Errorf("usage_events row count = %d, want %d (timed out waiting)", got, want)
	}
}

// ptr returns a pointer to v — a convenience for constructing *T fields inline.
func ptr[T any](v T) *T { return &v }

// sanitizeDSNName replaces any character that is not alphanumeric or underscore
// with an underscore, making a test name safe for use inside a SQLite file URI.
func sanitizeDSNName(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, s)
}

// defaultCfg returns a UsageConfig with a long flush interval (timer will not
// fire during short tests) and a buffer of 100.
func defaultCfg() config.UsageConfig {
	return config.UsageConfig{
		BufferSize:    100,
		FlushInterval: 30 * time.Second,
		DropOnFull:    ptr(false),
	}
}

// makeEvent returns a fully populated test Event with all optional fields set.
func makeEvent() Event {
	return Event{
		KeyID:            "key-abc",
		KeyType:          "user_key",
		OrgID:            "org-123",
		TeamID:           "team-456",
		UserID:           "user-789",
		ServiceAccountID: "",
		ModelName:        "gpt-oss-120b",
		PromptTokens:     10,
		CompletionTokens: 20,
		TotalTokens:      30,
		CostEstimate:     ptr(0.0042),
		DurationMS:       150,
		TTFT_MS:          ptr(45),
		TokensPerSecond:  ptr(133.3),
		StatusCode:       200,
	}
}

// TestLog_SingleEvent verifies that a single event is persisted after Stop().
func TestLog_SingleEvent(t *testing.T) {
	t.Parallel()

	d := openTestDB(t, "file:TestLog_SingleEvent?mode=memory&cache=private")
	l := newTestLogger(d, defaultCfg())
	l.Start()

	l.Log(makeEvent())
	stopAndWait(t, l, d, 1)
}

// TestLog_AllFieldsStored verifies that every column written by flush() matches
// the corresponding Event field, including all 16 columns from the INSERT.
func TestLog_AllFieldsStored(t *testing.T) {
	t.Parallel()

	d := openTestDB(t, "file:TestLog_AllFieldsStored?mode=memory&cache=private")
	l := newTestLogger(d, defaultCfg())
	l.Start()

	ev := makeEvent()
	l.Log(ev)
	stopAndWait(t, l, d, 1)

	ctx := context.Background()
	row := d.SQL().QueryRowContext(ctx, `
		SELECT key_id, key_type, org_id, team_id, user_id, service_account_id,
		       model_name, prompt_tokens, completion_tokens, total_tokens,
		       cost_estimate, request_duration_ms, ttft_ms, tokens_per_second,
		       status_code
		FROM usage_events LIMIT 1`,
	)

	var (
		keyID, keyType, orgID string
		teamID, userID, saID  sql.NullString
		modelName             string
		prompt, completion, total int
		costEstimate          sql.NullFloat64
		durationMS            int
		ttftMS                sql.NullInt64
		tokensPerSec          sql.NullFloat64
		statusCode            int
	)

	if err := row.Scan(
		&keyID, &keyType, &orgID,
		&teamID, &userID, &saID,
		&modelName,
		&prompt, &completion, &total,
		&costEstimate,
		&durationMS,
		&ttftMS,
		&tokensPerSec,
		&statusCode,
	); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if keyID != ev.KeyID {
		t.Errorf("key_id = %q, want %q", keyID, ev.KeyID)
	}
	if keyType != ev.KeyType {
		t.Errorf("key_type = %q, want %q", keyType, ev.KeyType)
	}
	if orgID != ev.OrgID {
		t.Errorf("org_id = %q, want %q", orgID, ev.OrgID)
	}
	if !teamID.Valid || teamID.String != ev.TeamID {
		t.Errorf("team_id = valid=%v %q, want %q", teamID.Valid, teamID.String, ev.TeamID)
	}
	if !userID.Valid || userID.String != ev.UserID {
		t.Errorf("user_id = valid=%v %q, want %q", userID.Valid, userID.String, ev.UserID)
	}
	if saID.Valid {
		t.Errorf("service_account_id = %q, want NULL (empty string becomes NULL)", saID.String)
	}
	if modelName != ev.ModelName {
		t.Errorf("model_name = %q, want %q", modelName, ev.ModelName)
	}
	if prompt != ev.PromptTokens {
		t.Errorf("prompt_tokens = %d, want %d", prompt, ev.PromptTokens)
	}
	if completion != ev.CompletionTokens {
		t.Errorf("completion_tokens = %d, want %d", completion, ev.CompletionTokens)
	}
	if total != ev.TotalTokens {
		t.Errorf("total_tokens = %d, want %d", total, ev.TotalTokens)
	}
	if !costEstimate.Valid {
		t.Error("cost_estimate = NULL, want non-NULL")
	} else if costEstimate.Float64 != *ev.CostEstimate {
		t.Errorf("cost_estimate = %v, want %v", costEstimate.Float64, *ev.CostEstimate)
	}
	if durationMS != ev.DurationMS {
		t.Errorf("request_duration_ms = %d, want %d", durationMS, ev.DurationMS)
	}
	if !ttftMS.Valid {
		t.Error("ttft_ms = NULL, want non-NULL")
	} else if int(ttftMS.Int64) != *ev.TTFT_MS {
		t.Errorf("ttft_ms = %d, want %d", ttftMS.Int64, *ev.TTFT_MS)
	}
	if !tokensPerSec.Valid {
		t.Error("tokens_per_second = NULL, want non-NULL")
	} else if tokensPerSec.Float64 != *ev.TokensPerSecond {
		t.Errorf("tokens_per_second = %v, want %v", tokensPerSec.Float64, *ev.TokensPerSecond)
	}
	if statusCode != ev.StatusCode {
		t.Errorf("status_code = %d, want %d", statusCode, ev.StatusCode)
	}
}

// TestLog_NullableFields verifies NULL vs. non-NULL storage for the six
// optional columns: team_id, user_id, service_account_id, cost_estimate,
// ttft_ms, and tokens_per_second.
func TestLog_NullableFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		event        Event
		wantTeamNull bool
		wantUserNull bool
		wantSANull   bool
		wantCostNull bool
		wantTTFTNull bool
		wantTPSNull  bool
	}{
		{
			name: "empty optional fields stored as NULL",
			event: Event{
				KeyID:     "k1",
				KeyType:   "team_key",
				OrgID:     "o1",
				ModelName: "m1",
				StatusCode: 200,
				// TeamID, UserID, ServiceAccountID: zero value → NULL
				// CostEstimate, TTFT_MS, TokensPerSecond: nil → NULL
			},
			wantTeamNull: true,
			wantUserNull: true,
			wantSANull:   true,
			wantCostNull: true,
			wantTTFTNull: true,
			wantTPSNull:  true,
		},
		{
			name: "populated optional fields stored as non-NULL",
			event: Event{
				KeyID:            "k2",
				KeyType:          "user_key",
				OrgID:            "o2",
				TeamID:           "t2",
				UserID:           "u2",
				ServiceAccountID: "sa2",
				ModelName:        "m2",
				CostEstimate:     ptr(0.001),
				TTFT_MS:          ptr(30),
				TokensPerSecond:  ptr(200.0),
				StatusCode:       200,
			},
			wantTeamNull: false,
			wantUserNull: false,
			wantSANull:   false,
			wantCostNull: false,
			wantTTFTNull: false,
			wantTPSNull:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := "file:TestLog_NullableFields_" + sanitizeDSNName(tc.name) + "?mode=memory&cache=private"
			d := openTestDB(t, dsn)
			l := newTestLogger(d, defaultCfg())
			l.Start()

			l.Log(tc.event)
			stopAndWait(t, l, d, 1)

			ctx := context.Background()
			row := d.SQL().QueryRowContext(ctx, `
				SELECT team_id, user_id, service_account_id,
				       cost_estimate, ttft_ms, tokens_per_second
				FROM usage_events LIMIT 1`,
			)

			var teamID, userID, saID sql.NullString
			var cost sql.NullFloat64
			var ttft sql.NullInt64
			var tps sql.NullFloat64

			if err := row.Scan(&teamID, &userID, &saID, &cost, &ttft, &tps); err != nil {
				t.Fatalf("Scan: %v", err)
			}

			if teamID.Valid == tc.wantTeamNull {
				t.Errorf("team_id.Valid = %v, want valid=%v", teamID.Valid, !tc.wantTeamNull)
			}
			if userID.Valid == tc.wantUserNull {
				t.Errorf("user_id.Valid = %v, want valid=%v", userID.Valid, !tc.wantUserNull)
			}
			if saID.Valid == tc.wantSANull {
				t.Errorf("service_account_id.Valid = %v, want valid=%v", saID.Valid, !tc.wantSANull)
			}
			if cost.Valid == tc.wantCostNull {
				t.Errorf("cost_estimate.Valid = %v, want valid=%v", cost.Valid, !tc.wantCostNull)
			}
			if ttft.Valid == tc.wantTTFTNull {
				t.Errorf("ttft_ms.Valid = %v, want valid=%v", ttft.Valid, !tc.wantTTFTNull)
			}
			if tps.Valid == tc.wantTPSNull {
				t.Errorf("tokens_per_second.Valid = %v, want valid=%v", tps.Valid, !tc.wantTPSNull)
			}
		})
	}
}

// TestLog_BatchFlush verifies that logging exactly batchSize events causes an
// immediate flush without waiting for the timer or Stop().
func TestLog_BatchFlush(t *testing.T) {
	t.Parallel()

	// BufferSize sets both the channel capacity and the batchSize threshold.
	// Use a long flush interval so only the batch-full path is exercised here.
	cfg := config.UsageConfig{
		BufferSize:    5,
		FlushInterval: 30 * time.Second,
		DropOnFull:    ptr(false),
	}

	d := openTestDB(t, "file:TestLog_BatchFlush?mode=memory&cache=private")
	l := newTestLogger(d, cfg)
	l.Start()
	defer l.Stop()

	for i := range 5 {
		ev := makeEvent()
		ev.StatusCode = 200 + i
		l.Log(ev)
	}

	// The fifth event fills the batch and triggers a flush inside run().
	// Poll until the flush lands in the DB.
	waitForRows(t, d, 5)
}

// TestLog_TimerFlush verifies that events are persisted when the flush
// interval fires even when the batch threshold has not been reached.
func TestLog_TimerFlush(t *testing.T) {
	t.Parallel()

	cfg := config.UsageConfig{
		BufferSize:    100,
		FlushInterval: 50 * time.Millisecond,
		DropOnFull:    ptr(false),
	}

	d := openTestDB(t, "file:TestLog_TimerFlush?mode=memory&cache=private")
	l := newTestLogger(d, cfg)
	l.Start()
	defer l.Stop()

	l.Log(makeEvent())

	// Wait up to 500ms for the 50ms timer to fire and flush the event.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if countRows(t, d) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := countRows(t, d); got != 1 {
		t.Errorf("usage_events row count after timer flush = %d, want 1", got)
	}
}

// TestLog_DropOnFull verifies that when the buffer is full and dropOnFull is
// true, excess Log() calls return immediately without blocking, and the dropped
// events are never persisted.
func TestLog_DropOnFull(t *testing.T) {
	t.Parallel()

	// Build the logger manually without starting it so the channel fills up
	// and the third Log() call has nowhere to send.
	d := openTestDB(t, "file:TestLog_DropOnFull?mode=memory&cache=private")
	cfg := config.UsageConfig{
		BufferSize:    2,
		FlushInterval: 30 * time.Second,
		DropOnFull:    ptr(true),
	}
	l := newTestLogger(d, cfg)
	// Do NOT call l.Start() yet — the run goroutine is not consuming events,
	// so the 2-slot channel fills immediately.

	ev := makeEvent()
	l.Log(ev) // slot 1
	l.Log(ev) // slot 2

	// This third call must return immediately (dropped), not block.
	dropDone := make(chan struct{})
	go func() {
		l.Log(ev)
		close(dropDone)
	}()

	select {
	case <-dropDone:
		// good: returned without blocking
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Log() blocked on full buffer with dropOnFull=true")
	}

	// Start and stop to drain the 2 buffered events.
	l.Start()
	stopAndWait(t, l, d, 2)

	// Exactly 2 rows — the third was dropped.
	if got := countRows(t, d); got != 2 {
		t.Errorf("usage_events row count = %d, want 2 (third event should have been dropped)", got)
	}
}

// TestStop_FlushesRemaining verifies that Stop() drains all in-flight events
// and persists them before the goroutine exits.
func TestStop_FlushesRemaining(t *testing.T) {
	t.Parallel()

	d := openTestDB(t, "file:TestStop_FlushesRemaining?mode=memory&cache=private")
	l := newTestLogger(d, defaultCfg())
	l.Start()

	const n = 10
	for range n {
		l.Log(makeEvent())
	}

	// Stop() signals the goroutine to drain; we poll until all rows appear.
	stopAndWait(t, l, d, n)
}

// TestLog_NoContentColumns asserts that the usage_events table contains no
// columns that could store prompt or response body text. This is a privacy
// invariant: VoidLLM must never persist request/response content.
func TestLog_NoContentColumns(t *testing.T) {
	t.Parallel()

	d := openTestDB(t, "file:TestLog_NoContentColumns?mode=memory&cache=private")

	ctx := context.Background()
	rows, err := d.SQL().QueryContext(ctx, "PRAGMA table_info(usage_events)")
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()

	// Substrings whose presence in a column name would indicate a privacy breach.
	forbidden := []string{"content", "prompt_text", "response_text", "body", "message"}

	for rows.Next() {
		var (
			cid       int
			name      string
			colType   string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			t.Fatalf("Scan column info: %v", err)
		}
		lower := strings.ToLower(name)
		for _, f := range forbidden {
			if strings.Contains(lower, f) {
				t.Errorf("usage_events column %q contains forbidden substring %q — prompt/response content must never be stored", name, f)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
}

// TestNullableString exercises the package-private nullableString helper that
// controls the SQL NULL-vs-value decision made in flush().
func TestNullableString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  any
	}{
		{
			name:  "empty string returns nil",
			input: "",
			want:  nil,
		},
		{
			name:  "non-empty string returns itself",
			input: "hello",
			want:  "hello",
		},
		{
			name:  "whitespace only string returns itself",
			input: "  ",
			want:  "  ",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := nullableString(tc.input)
			if got != tc.want {
				t.Errorf("nullableString(%q) = %v (%T), want %v (%T)", tc.input, got, got, tc.want, tc.want)
			}
		})
	}
}

// TestLog_TokenCounterUpdatedBeforeFlush verifies that when a Logger is wired
// with a TokenCounter, Log() increments the counter synchronously — before the
// event reaches the database — so that a subsequent CheckTokens call sees the
// current usage without waiting for the async flush.
func TestLog_TokenCounterUpdatedBeforeFlush(t *testing.T) {
	t.Parallel()

	d := openTestDB(t, "file:TestLog_TokenCounterUpdatedBeforeFlush?mode=memory&cache=private")

	counter := ratelimit.NewTokenCounter()
	cfg := defaultCfg() // long flush interval — DB write will not happen during the test
	l := NewLogger(d, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), counter)
	l.Start()
	defer l.Stop()

	ev := Event{
		KeyID:        "tc-key",
		KeyType:      "user_key",
		OrgID:        "tc-org",
		TeamID:       "tc-team",
		ModelName:    "test-model",
		PromptTokens: 60,
		CompletionTokens: 40,
		TotalTokens:  100,
		StatusCode:   200,
	}

	// DB has no rows yet — the flush interval is 30 s so the event is still queued.
	l.Log(ev)

	// The counter must already reflect the 100 tokens.
	keyLimits := ratelimit.Limits{DailyTokenLimit: 200}
	noLimits := ratelimit.Limits{}

	if err := counter.CheckTokens("tc-key", "tc-team", "tc-org", keyLimits, noLimits, noLimits); err != nil {
		t.Errorf("CheckTokens() immediately after Log() = %v, want nil (100 tokens < limit 200)", err)
	}

	// Now verify the counter blocks a request that would exceed the budget.
	keyLimitsExceeded := ratelimit.Limits{DailyTokenLimit: 100}
	if err := counter.CheckTokens("tc-key", "tc-team", "tc-org", keyLimitsExceeded, noLimits, noLimits); err == nil {
		t.Error("CheckTokens() = nil, want ErrTokenBudgetExceeded (100 tokens >= limit 100)")
	}

	// Confirm the DB row is not yet written (flush has not fired).
	if got := countRows(t, d); got != 0 {
		t.Errorf("usage_events row count before flush = %d, want 0 (counter updated before DB write)", got)
	}
}
