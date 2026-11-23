package db

import (
	"context"
	"testing"
	"time"
)

type backfillEvent struct {
	id, key, model, created string
	team, user              any
	cost                    any
	ttft                    any
	tokens                  int
}

func insertBackfillEvent(t *testing.T, d *DB, e backfillEvent) {
	t.Helper()
	p := d.Dialect().Placeholder
	_, err := d.sql.ExecContext(context.Background(),
		"INSERT INTO usage_events (id, key_id, key_type, org_id, team_id, user_id, model_name, "+
			"prompt_tokens, completion_tokens, total_tokens, cached_read_tokens, cache_write_tokens, "+
			"cost_estimate, request_duration_ms, ttft_ms, status_code, created_at) VALUES ("+
			p(1)+", "+p(2)+", 'user_key', 'org-bf', "+p(3)+", "+p(4)+", "+p(5)+", "+
			p(6)+", 1, "+p(7)+", 2, 3, "+p(8)+", 100, "+p(9)+", 200, "+p(10)+")",
		e.id, e.key, e.team, e.user, e.model, e.tokens, e.tokens+1, e.cost, e.ttft, e.created)
	if err != nil {
		t.Fatalf("insert usage event %s: %v", e.id, err)
	}
}

func openBackfillPostgres(t *testing.T) *DB {
	t.Helper()
	dsn := postgresTestDSN()
	if dsn == "" {
		t.Skip("VOIDLLM_TEST_POSTGRES_DSN not set")
	}
	return openMigratedPostgresDB(t, dsn)
}

func backfillMarker(t *testing.T, d *DB) string {
	t.Helper()
	v, err := d.GetSetting(context.Background(), UsageHourlyBackfillMarker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	return v
}

func countBuckets(t *testing.T, d *DB, where string) int {
	t.Helper()
	var n int
	if err := d.sql.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM usage_hourly WHERE "+where).Scan(&n); err != nil {
		t.Fatalf("count buckets: %v", err)
	}
	return n
}

// Advisory locks are database-wide, so the PostgreSQL backfill tests below
// must not run in parallel with each other.

func TestBackfillUsageHourly_SQLiteNoOp(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	insertBackfillEvent(t, d, backfillEvent{id: "e1", key: "k1", model: "m", created: "2026-03-20T10:05:00Z", tokens: 1})

	rows, ran, err := d.BackfillUsageHourlyOnce(ctx, time.Date(2026, 3, 20, 14, 30, 0, 0, time.UTC))
	if err != nil || ran || rows != 0 {
		t.Fatalf("rows=%d ran=%v err=%v, want no-op", rows, ran, err)
	}
	if n := countBuckets(t, d, "1=1"); n != 0 {
		t.Errorf("buckets = %d, want 0 on sqlite", n)
	}
	if got := backfillMarker(t, d); got != usageHourlyBackfillDone {
		t.Errorf("marker = %q, want %q", got, usageHourlyBackfillDone)
	}
}

func TestBackfillUsageHourly_Postgres(t *testing.T) {
	d := openBackfillPostgres(t)
	ctx := context.Background()
	start := time.Date(2026, 3, 20, 14, 30, 0, 0, time.UTC)

	// Hour 10: earliest event (10:05) carries t1/u1, later events differ; one
	// NULL cost, one NULL ttft. Inserted out of order.
	insertBackfillEvent(t, d, backfillEvent{id: "e2", key: "k1", model: "m", created: "2026-03-20T10:59:59Z", team: "t9", user: nil, cost: nil, ttft: nil, tokens: 20})
	insertBackfillEvent(t, d, backfillEvent{id: "e3", key: "k1", model: "m", created: "2026-03-20T10:30:00Z", team: nil, user: "u9", cost: 0.25, ttft: 60, tokens: 5})
	insertBackfillEvent(t, d, backfillEvent{id: "e1", key: "k1", model: "m", created: "2026-03-20T10:05:00Z", team: "t1", user: "u1", cost: 0.5, ttft: 40, tokens: 10})
	// Hour 11 without team/user, previous month event as well.
	insertBackfillEvent(t, d, backfillEvent{id: "e4", key: "k1", model: "m", created: "2026-03-20T11:00:00Z", team: nil, user: nil, cost: nil, ttft: nil, tokens: 7})
	insertBackfillEvent(t, d, backfillEvent{id: "e7", key: "k1", model: "m", created: "2026-02-03T08:00:00Z", team: "t2", user: "u2", cost: nil, ttft: nil, tokens: 4})
	// Existing live bucket stays untouched.
	insertBackfillEvent(t, d, backfillEvent{id: "e5", key: "k2", model: "m", created: "2026-03-20T12:10:00Z", team: nil, user: nil, cost: 1, ttft: 1, tokens: 99})
	if err := d.UpsertUsageHourly(ctx, HourlyRollup{OrgID: "org-bf", KeyID: "k2", ModelName: "m", BucketHour: "2026-03-20T12:00:00Z", RequestCount: 1, TotalTokens: 1}); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
	// Start hour and later: untouched.
	insertBackfillEvent(t, d, backfillEvent{id: "e6", key: "k1", model: "m", created: "2026-03-20T14:01:00Z", tokens: 3})
	insertBackfillEvent(t, d, backfillEvent{id: "e8", key: "k1", model: "m", created: "2026-03-20T15:01:00Z", tokens: 3})
	// Malformed timestamps: skipped.
	insertBackfillEvent(t, d, backfillEvent{id: "x1", key: "k1", model: "m", created: "2026-03-20 09:00:00", tokens: 3})
	insertBackfillEvent(t, d, backfillEvent{id: "x2", key: "k1", model: "m", created: "2026-03-20T09:00:00.123456Z", tokens: 3})
	insertBackfillEvent(t, d, backfillEvent{id: "x3", key: "k1", model: "m", created: "garbage", tokens: 3})

	rows, ran, err := d.BackfillUsageHourlyOnce(ctx, start)
	if err != nil || !ran {
		t.Fatalf("first run: rows=%d ran=%v err=%v", rows, ran, err)
	}
	if rows != 3 {
		t.Errorf("rows = %d, want 3 (feb hour 8, hours 10 and 11)", rows)
	}
	if got := backfillMarker(t, d); got != usageHourlyBackfillDone {
		t.Errorf("marker = %q, want done", got)
	}

	var (
		team, user               string
		req, prompt, comp, total int
		cached, cw, ttftCount    int
		cost, dur, ttftSum       float64
	)
	err = d.sql.QueryRowContext(ctx, "SELECT team_id, user_id, request_count, prompt_tokens, completion_tokens, total_tokens, "+
		"cached_read_tokens, cache_write_tokens, cost_sum, duration_sum_ms, ttft_sum_ms, ttft_count FROM usage_hourly "+
		"WHERE key_id = 'k1' AND bucket_hour = '2026-03-20T10:00:00Z'").
		Scan(&team, &user, &req, &prompt, &comp, &total, &cached, &cw, &cost, &dur, &ttftSum, &ttftCount)
	if err != nil {
		t.Fatalf("read hour 10: %v", err)
	}
	if team != "t1" || user != "u1" {
		t.Errorf("attribution = %q/%q, want t1/u1 from the earliest event", team, user)
	}
	if req != 3 || prompt != 35 || comp != 3 || total != 38 || cached != 6 || cw != 9 ||
		cost != 0.75 || dur != 300 || ttftSum != 100 || ttftCount != 2 {
		t.Errorf("hour 10 = req=%d p=%d c=%d t=%d cr=%d cw=%d cost=%v dur=%v ttft=%v/%d",
			req, prompt, comp, total, cached, cw, cost, dur, ttftSum, ttftCount)
	}

	err = d.sql.QueryRowContext(ctx, "SELECT team_id, user_id, cost_sum, ttft_count FROM usage_hourly "+
		"WHERE key_id = 'k1' AND bucket_hour = '2026-03-20T11:00:00Z'").Scan(&team, &user, &cost, &ttftCount)
	if err != nil {
		t.Fatalf("read hour 11: %v", err)
	}
	if team != "" || user != "" || cost != 0 || ttftCount != 0 {
		t.Errorf("hour 11 = %q %q cost=%v ttft=%d, want empty/0", team, user, cost, ttftCount)
	}

	if n := countBuckets(t, d, "bucket_hour >= '2026-03-20T13:00:00Z' AND key_id = 'k1'"); n != 0 {
		t.Errorf("buckets at/after start hour = %d, want 0", n)
	}
	if n := countBuckets(t, d, "key_id = 'k1' AND bucket_hour < '2026-03-20T10:00:00Z'"); n != 1 {
		t.Errorf("buckets before hour 10 = %d, want 1 (feb) and no malformed ones", n)
	}
	var existing int
	if err := d.sql.QueryRowContext(ctx, "SELECT total_tokens FROM usage_hourly WHERE key_id = 'k2'").Scan(&existing); err != nil || existing != 1 {
		t.Errorf("existing bucket total_tokens = %d (err %v), want 1", existing, err)
	}

	agg, err := d.GetHourlyUsageTotals(ctx, UsageFilter{OrgID: "org-bf"}, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("GetHourlyUsageTotals with malformed events present: %v", err)
	}
	if agg.TotalRequests != 5 {
		t.Errorf("TotalRequests = %d, want 5", agg.TotalRequests)
	}

	if rows, ran, err = d.BackfillUsageHourlyOnce(ctx, start); err != nil || ran || rows != 0 {
		t.Errorf("second run: rows=%d ran=%v err=%v, want no-op", rows, ran, err)
	}
}

func TestBackfillUsageHourly_ResumesMonthByMonth(t *testing.T) {
	d := openBackfillPostgres(t)
	ctx := context.Background()
	start := time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC)

	insertBackfillEvent(t, d, backfillEvent{id: "a", key: "k1", model: "m", created: "2026-01-05T01:00:00Z", tokens: 1})
	insertBackfillEvent(t, d, backfillEvent{id: "b", key: "k1", model: "m", created: "2026-02-05T01:00:00Z", tokens: 1})
	insertBackfillEvent(t, d, backfillEvent{id: "c", key: "k1", model: "m", created: "2026-03-05T01:00:00Z", tokens: 1})

	// Fail while writing the February bucket.
	for _, stmt := range []string{
		`CREATE FUNCTION vl_fail_feb() RETURNS trigger AS $$ BEGIN
			IF NEW.bucket_hour LIKE '2026-02%' THEN RAISE EXCEPTION 'simulated failure'; END IF;
			RETURN NEW; END; $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER vl_fail_feb BEFORE INSERT ON usage_hourly FOR EACH ROW EXECUTE FUNCTION vl_fail_feb()`,
	} {
		if _, err := d.sql.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup trigger: %v", err)
		}
	}

	if _, ran, err := d.BackfillUsageHourlyOnce(ctx, start); err == nil || !ran {
		t.Fatalf("first run: ran=%v err=%v, want failure", ran, err)
	}
	if got := backfillMarker(t, d); got != `{"done_until":"2026-01"}` {
		t.Fatalf("marker after failure = %q, want done_until 2026-01", got)
	}
	if n := countBuckets(t, d, "bucket_hour LIKE '2026-01%'"); n != 1 {
		t.Errorf("january buckets = %d, want 1", n)
	}

	if _, err := d.sql.ExecContext(ctx, "DROP TRIGGER vl_fail_feb ON usage_hourly"); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	// Make a January re-scan observable: remove its bucket. A resume must not redo it.
	if _, err := d.sql.ExecContext(ctx, "DELETE FROM usage_hourly WHERE bucket_hour LIKE '2026-01%'"); err != nil {
		t.Fatalf("delete january: %v", err)
	}

	rows, ran, err := d.BackfillUsageHourlyOnce(ctx, start)
	if err != nil || !ran {
		t.Fatalf("resume: rows=%d ran=%v err=%v", rows, ran, err)
	}
	if rows != 2 {
		t.Errorf("resume rows = %d, want 2 (feb, mar)", rows)
	}
	if n := countBuckets(t, d, "bucket_hour LIKE '2026-01%'"); n != 0 {
		t.Errorf("january re-processed on resume: %d buckets", n)
	}
	if got := backfillMarker(t, d); got != usageHourlyBackfillDone {
		t.Errorf("marker = %q, want done", got)
	}
}

func TestBackfillUsageHourly_SecondInstanceSkipsWhileLocked(t *testing.T) {
	d := openBackfillPostgres(t)
	ctx := context.Background()
	insertBackfillEvent(t, d, backfillEvent{id: "a", key: "k1", model: "m", created: "2026-03-05T01:00:00Z", tokens: 1})
	start := time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC)

	holder, err := d.sql.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	defer holder.Close() //nolint:errcheck // test cleanup
	if _, err := holder.ExecContext(ctx, "SELECT pg_advisory_lock($1)", usageHourlyBackfillLockKey); err != nil {
		t.Fatalf("hold lock: %v", err)
	}

	rows, ran, err := d.BackfillUsageHourlyOnce(ctx, start)
	if err != nil || ran || rows != 0 {
		t.Fatalf("while locked: rows=%d ran=%v err=%v, want skip", rows, ran, err)
	}
	if got := backfillMarker(t, d); got != "" {
		t.Errorf("marker = %q, want unset", got)
	}
	if n := countBuckets(t, d, "1=1"); n != 0 {
		t.Errorf("buckets = %d, want 0", n)
	}

	if _, err := holder.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", usageHourlyBackfillLockKey); err != nil {
		t.Fatalf("release lock: %v", err)
	}
	if rows, ran, err = d.BackfillUsageHourlyOnce(ctx, start); err != nil || !ran || rows != 1 {
		t.Errorf("after release: rows=%d ran=%v err=%v, want 1 row", rows, ran, err)
	}
}

func TestBackfillUsageHourly_FailureLeavesMarkerUnset(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := d.BackfillUsageHourlyOnce(ctx, time.Now()); err == nil {
		t.Fatal("expected error with canceled context")
	}
	if v := backfillMarker(t, d); v != "" {
		t.Errorf("marker = %q, want unset", v)
	}
}
