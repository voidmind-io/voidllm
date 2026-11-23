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

func TestBackfillUsageHourly(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		now := time.Date(2026, 3, 20, 14, 30, 0, 0, time.UTC)

		// Hour 10: three events, one NULL cost, one NULL ttft.
		insertBackfillEvent(t, d, backfillEvent{id: "e1", key: "k1", model: "m", created: "2026-03-20T10:05:00Z", team: "t1", user: "u1", cost: 0.5, ttft: 40, tokens: 10})
		insertBackfillEvent(t, d, backfillEvent{id: "e2", key: "k1", model: "m", created: "2026-03-20T10:59:59Z", team: "t1", user: "u1", cost: nil, ttft: nil, tokens: 20})
		insertBackfillEvent(t, d, backfillEvent{id: "e3", key: "k1", model: "m", created: "2026-03-20T10:30:00Z", team: "t1", user: "u1", cost: 0.25, ttft: 60, tokens: 5})
		// Hour 11 without team/user.
		insertBackfillEvent(t, d, backfillEvent{id: "e4", key: "k1", model: "m", created: "2026-03-20T11:00:00Z", team: nil, user: nil, cost: nil, ttft: nil, tokens: 7})
		// Hour 12 already has a live bucket that must stay untouched.
		insertBackfillEvent(t, d, backfillEvent{id: "e5", key: "k2", model: "m", created: "2026-03-20T12:10:00Z", team: nil, user: nil, cost: 1, ttft: 1, tokens: 99})
		if err := d.UpsertUsageHourly(ctx, HourlyRollup{OrgID: "org-bf", KeyID: "k2", ModelName: "m", BucketHour: "2026-03-20T12:00:00Z", RequestCount: 1, TotalTokens: 1}); err != nil {
			t.Fatalf("seed bucket: %v", err)
		}
		// Current hour: must be skipped.
		insertBackfillEvent(t, d, backfillEvent{id: "e6", key: "k1", model: "m", created: "2026-03-20T14:01:00Z", team: nil, user: nil, cost: nil, ttft: nil, tokens: 3})

		rows, ran, err := d.BackfillUsageHourlyOnce(ctx, now)
		if err != nil || !ran {
			t.Fatalf("first run: rows=%d ran=%v err=%v", rows, ran, err)
		}
		if rows != 2 {
			t.Errorf("rows = %d, want 2 (hours 10 and 11)", rows)
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
		if team != "t1" || user != "u1" || req != 3 || prompt != 35 || comp != 3 || total != 38 ||
			cached != 6 || cw != 9 || cost != 0.75 || dur != 300 || ttftSum != 100 || ttftCount != 2 {
			t.Errorf("hour 10 = %s %s req=%d p=%d c=%d t=%d cr=%d cw=%d cost=%v dur=%v ttft=%v/%d",
				team, user, req, prompt, comp, total, cached, cw, cost, dur, ttftSum, ttftCount)
		}

		err = d.sql.QueryRowContext(ctx, "SELECT team_id, user_id, cost_sum, ttft_count FROM usage_hourly "+
			"WHERE key_id = 'k1' AND bucket_hour = '2026-03-20T11:00:00Z'").Scan(&team, &user, &cost, &ttftCount)
		if err != nil {
			t.Fatalf("read hour 11: %v", err)
		}
		if team != "" || user != "" || cost != 0 || ttftCount != 0 {
			t.Errorf("hour 11 = %q %q cost=%v ttft=%d, want empty/0", team, user, cost, ttftCount)
		}

		var n, existing int
		if err := d.sql.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_hourly WHERE bucket_hour = '2026-03-20T14:00:00Z'").Scan(&n); err != nil || n != 0 {
			t.Errorf("current hour buckets = %d (err %v), want 0", n, err)
		}
		if err := d.sql.QueryRowContext(ctx, "SELECT total_tokens FROM usage_hourly WHERE key_id = 'k2'").Scan(&existing); err != nil || existing != 1 {
			t.Errorf("existing bucket total_tokens = %d (err %v), want 1 (untouched)", existing, err)
		}

		// Second run: marker short-circuits.
		if rows, ran, err = d.BackfillUsageHourlyOnce(ctx, now); err != nil || ran || rows != 0 {
			t.Errorf("second run: rows=%d ran=%v err=%v, want no-op", rows, ran, err)
		}
		// Direct re-run: ON CONFLICT leaves everything as is.
		if rows, err = d.BackfillUsageHourly(ctx, now); err != nil || rows != 0 {
			t.Errorf("direct rerun: rows=%d err=%v, want 0", rows, err)
		}
		if err := d.sql.QueryRowContext(ctx, "SELECT request_count FROM usage_hourly WHERE key_id = 'k1' AND bucket_hour = '2026-03-20T10:00:00Z'").Scan(&req); err != nil || req != 3 {
			t.Errorf("request_count after rerun = %d (err %v), want 3", req, err)
		}
	})
}

func TestBackfillUsageHourly_FailureLeavesMarkerUnset(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := d.BackfillUsageHourlyOnce(ctx, time.Now()); err == nil {
		t.Fatal("expected error with canceled context")
	}
	v, err := d.GetSetting(context.Background(), UsageHourlyBackfillMarker)
	if err != nil || v != "" {
		t.Errorf("marker = %q (err %v), want unset", v, err)
	}
}
