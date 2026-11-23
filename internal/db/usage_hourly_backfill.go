package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// UsageHourlyBackfillMarker is the settings key recording the progress of the
// one-time usage_hourly backfill from usage_events. Its value is either
// usageHourlyBackfillDone or a JSON object {"done_until":"YYYY-MM"} naming the
// last fully processed month.
const UsageHourlyBackfillMarker = "usage_hourly_backfill_v1"

// usageHourlyBackfillDone is the final marker value once the backfill finished.
const usageHourlyBackfillDone = "done"

// usageHourlyBackfillLockKey is the PostgreSQL advisory lock key that ensures
// only one instance runs the backfill at a time.
const usageHourlyBackfillLockKey int64 = 0x766f6964686272

// usageHourlyBackfillUnlockTimeout bounds the advisory unlock call.
const usageHourlyBackfillUnlockTimeout = 5 * time.Second

// canonicalTimestampRegex matches the canonical TimestampLayout shape with
// in-range fields, including days per month (Feb 29 in non-leap years still
// matches and is skipped when parsing).
const canonicalTimestampRegex = `^[0-9]{4}-((0[13578]|1[02])-(0[1-9]|[12][0-9]|3[01])|(0[469]|11)-(0[1-9]|[12][0-9]|30)|02-(0[1-9]|1[0-9]|2[0-9]))T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]Z$`

type usageHourlyBackfillProgress struct {
	// Cutoff is the exclusive upper bound of the run (canonical timestamp),
	// fixed on the first attempt so a resume never recomputes it from buckets
	// the backfill itself created.
	Cutoff    string `json:"cutoff,omitempty"`
	DoneUntil string `json:"done_until,omitempty"`
}

// BackfillUsageHourlyOnce rebuilds missing usage_hourly buckets from
// usage_events, once per database, on PostgreSQL only. SQLite never suffered
// the upsert defect the backfill repairs, so there the marker is set and the
// call returns immediately.
//
// Only hours strictly before the cutoff are backfilled, where cutoff is the
// earlier of the hour containing processStart and the hour of the earliest
// existing usage_hourly bucket. A freshly started process holds no buffered
// events from before its start, and the table was empty on PostgreSQL before
// the upsert fix, so the earliest bucket marks when fixed loggers (including
// other replicas of a rolling deploy) started writing. No live upsert can
// therefore interleave with the backfill. The first live hour may stay
// partial: buckets the logger already created there are kept (ON CONFLICT DO
// NOTHING). The cutoff is persisted on the first attempt and reused on resume.
//
// The work is done month by month from the oldest event to the newest, and the
// last finished month is persisted in the settings marker, so an interrupted
// run resumes where it stopped. A session-level advisory lock ensures a single
// instance runs it; when another instance holds the lock the call returns
// ran=false and the backfill is retried on the next start. The marker is set to
// its final value only after every month succeeded.
func (d *DB) BackfillUsageHourlyOnce(ctx context.Context, processStart time.Time) (rows int64, ran bool, err error) {
	if _, ok := d.dialect.(PostgresDialect); !ok {
		done, err := d.GetSetting(ctx, UsageHourlyBackfillMarker)
		if err != nil {
			return 0, false, fmt.Errorf("backfill usage hourly: read marker: %w", err)
		}
		if done == usageHourlyBackfillDone {
			return 0, false, nil
		}
		if err := d.SetSetting(ctx, UsageHourlyBackfillMarker, usageHourlyBackfillDone); err != nil {
			return 0, false, fmt.Errorf("backfill usage hourly: set marker: %w", err)
		}
		return 0, false, nil
	}

	conn, err := d.sql.Conn(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("backfill usage hourly: acquire connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup

	var locked bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", usageHourlyBackfillLockKey).Scan(&locked); err != nil {
		return 0, false, fmt.Errorf("backfill usage hourly: acquire lock: %w", err)
	}
	if !locked {
		return 0, false, nil
	}
	defer func() {
		uctx, ucancel := context.WithTimeout(context.Background(), usageHourlyBackfillUnlockTimeout)
		defer ucancel()
		if _, uerr := conn.ExecContext(uctx, "SELECT pg_advisory_unlock($1)", usageHourlyBackfillLockKey); uerr != nil {
			// The lock is session-scoped: discard the connection so it cannot
			// return to the pool still holding the lock.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, fmt.Errorf("backfill usage hourly: release lock: %w", uerr))
		}
	}()

	marker, err := d.readBackfillMarker(ctx, conn)
	if err != nil {
		return 0, false, err
	}
	if marker == usageHourlyBackfillDone {
		return 0, false, nil
	}
	var progress usageHourlyBackfillProgress
	if marker != "" {
		if err := json.Unmarshal([]byte(marker), &progress); err != nil {
			return 0, true, fmt.Errorf("backfill usage hourly: parse marker: %w", err)
		}
	}

	if progress.Cutoff == "" {
		cutoff, err := d.backfillCutoff(ctx, conn, processStart)
		if err != nil {
			return 0, true, err
		}
		progress.Cutoff = FormatTimestamp(cutoff)
		if err := d.saveBackfillProgress(ctx, conn, progress); err != nil {
			return 0, true, err
		}
	}

	rows, err = d.backfillMonths(ctx, conn, progress)
	if err != nil {
		return rows, true, err
	}
	if err := d.writeBackfillMarker(ctx, conn, usageHourlyBackfillDone); err != nil {
		return rows, true, err
	}
	return rows, true, nil
}

// backfillCutoff returns the earlier of the hour containing processStart and
// the hour of the earliest existing usage_hourly bucket.
func (d *DB) backfillCutoff(ctx context.Context, conn *sql.Conn, processStart time.Time) (time.Time, error) {
	cutoff := processStart.UTC().Truncate(time.Hour)
	var earliest sql.NullString
	if err := conn.QueryRowContext(ctx, "SELECT MIN(bucket_hour) FROM usage_hourly").Scan(&earliest); err != nil {
		return time.Time{}, fmt.Errorf("backfill usage hourly: find earliest bucket: %w", err)
	}
	if earliest.Valid {
		if t, err := time.Parse(TimestampLayout, earliest.String); err == nil && t.Before(cutoff) {
			cutoff = t.UTC().Truncate(time.Hour)
		}
	}
	return cutoff, nil
}

func (d *DB) saveBackfillProgress(ctx context.Context, conn *sql.Conn, p usageHourlyBackfillProgress) error {
	b, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("backfill usage hourly: encode progress: %w", err)
	}
	return d.writeBackfillMarker(ctx, conn, string(b))
}

// firstValidEvent returns the oldest parseable canonical created_at before
// cutoff, skipping calendar-invalid values. ok is false when there is none.
func (d *DB) firstValidEvent(ctx context.Context, conn *sql.Conn, cutoff time.Time) (time.Time, bool, error) {
	p := d.dialect.Placeholder
	lower := ""
	for {
		var oldest sql.NullString
		err := conn.QueryRowContext(ctx,
			"SELECT MIN(created_at) FROM usage_events WHERE created_at ~ '"+canonicalTimestampRegex+"' AND created_at > "+p(1)+" AND created_at < "+p(2),
			lower, FormatTimestamp(cutoff)).Scan(&oldest)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("backfill usage hourly: find oldest event: %w", err)
		}
		if !oldest.Valid {
			return time.Time{}, false, nil
		}
		if t, perr := time.Parse(TimestampLayout, oldest.String); perr == nil {
			return t, true, nil
		}
		lower = oldest.String
	}
}

// backfillMonths processes every month from the first one not yet recorded in
// progress up to the month containing the cutoff, persisting progress after
// each fully processed month.
func (d *DB) backfillMonths(ctx context.Context, conn *sql.Conn, progress usageHourlyBackfillProgress) (int64, error) {
	cutoff, err := time.Parse(TimestampLayout, progress.Cutoff)
	if err != nil {
		return 0, fmt.Errorf("backfill usage hourly: parse cutoff %q: %w", progress.Cutoff, err)
	}

	first, ok, err := d.firstValidEvent(ctx, conn, cutoff)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	month := time.Date(first.Year(), first.Month(), 1, 0, 0, 0, 0, time.UTC)
	if progress.DoneUntil != "" {
		done, err := time.Parse("2006-01", progress.DoneUntil)
		if err != nil {
			return 0, fmt.Errorf("backfill usage hourly: parse progress %q: %w", progress.DoneUntil, err)
		}
		if next := done.AddDate(0, 1, 0); next.After(month) {
			month = next
		}
	}

	var total int64
	for ; month.Before(cutoff); month = month.AddDate(0, 1, 0) {
		end := month.AddDate(0, 1, 0)
		complete := !end.After(cutoff)
		if !complete {
			end = cutoff
		}
		n, err := d.backfillRange(ctx, conn, month, end)
		if err != nil {
			return total, err
		}
		total += n
		if complete {
			progress.DoneUntil = month.Format("2006-01")
			if err := d.saveBackfillProgress(ctx, conn, progress); err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

// backfillRange inserts one usage_hourly bucket per (key_id, model_name, hour)
// found in usage_events with from <= created_at < to. Existing buckets are
// left untouched (ON CONFLICT DO NOTHING), so the statement is idempotent.
//
// org_id, team_id and user_id of a bucket are taken together from the earliest
// event of that bucket (ties broken by id), never from independent
// aggregates; NULL team/user become ”. NULL cost and duration count as 0 and
// ttft_count counts only events with a recorded TTFT, matching the usage
// logger. Rows whose created_at is not in the canonical shape are ignored.
func (d *DB) backfillRange(ctx context.Context, conn *sql.Conn, from, to time.Time) (int64, error) {
	p := d.dialect.Placeholder
	query := "WITH ev AS (" +
		"SELECT *, substr(created_at, 1, 13) || ':00:00Z' AS bucket_hour FROM usage_events " +
		"WHERE created_at ~ '" + canonicalTimestampRegex + "' AND created_at >= " + p(1) + " AND created_at < " + p(2) + "), " +
		"first_ev AS (SELECT DISTINCT ON (key_id, model_name, bucket_hour) key_id, model_name, bucket_hour, org_id, team_id, user_id " +
		"FROM ev ORDER BY key_id, model_name, bucket_hour, created_at, id), " +
		"agg AS (SELECT key_id, model_name, bucket_hour, " +
		"COUNT(*) AS request_count, COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens, " +
		"COALESCE(SUM(completion_tokens), 0) AS completion_tokens, COALESCE(SUM(total_tokens), 0) AS total_tokens, " +
		"COALESCE(SUM(cached_read_tokens), 0) AS cached_read_tokens, COALESCE(SUM(cache_write_tokens), 0) AS cache_write_tokens, " +
		"COALESCE(SUM(cost_estimate), 0) AS cost_sum, COALESCE(SUM(request_duration_ms), 0) AS duration_sum_ms, " +
		"COALESCE(SUM(ttft_ms), 0) AS ttft_sum_ms, COUNT(ttft_ms) AS ttft_count " +
		"FROM ev GROUP BY key_id, model_name, bucket_hour) " +
		"INSERT INTO usage_hourly (" +
		"org_id, team_id, user_id, key_id, model_name, bucket_hour, " +
		"request_count, prompt_tokens, completion_tokens, total_tokens, " +
		"cached_read_tokens, cache_write_tokens, cost_sum, duration_sum_ms, ttft_sum_ms, ttft_count) " +
		"SELECT f.org_id, COALESCE(f.team_id, ''), COALESCE(f.user_id, ''), a.key_id, a.model_name, a.bucket_hour, " +
		"a.request_count, a.prompt_tokens, a.completion_tokens, a.total_tokens, " +
		"a.cached_read_tokens, a.cache_write_tokens, a.cost_sum, a.duration_sum_ms, a.ttft_sum_ms, a.ttft_count " +
		"FROM agg a JOIN first_ev f ON f.key_id = a.key_id AND f.model_name = a.model_name AND f.bucket_hour = a.bucket_hour " +
		"ON CONFLICT (key_id, model_name, bucket_hour) DO NOTHING"

	res, err := conn.ExecContext(ctx, query, FormatTimestamp(from), FormatTimestamp(to))
	if err != nil {
		return 0, fmt.Errorf("backfill usage hourly %s: %w", from.Format("2006-01"), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("backfill usage hourly: rows affected: %w", err)
	}
	return n, nil
}

func (d *DB) readBackfillMarker(ctx context.Context, conn *sql.Conn) (string, error) {
	var v string
	err := conn.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = "+d.dialect.Placeholder(1),
		UsageHourlyBackfillMarker).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("backfill usage hourly: read marker: %w", err)
	}
	return v, nil
}

func (d *DB) writeBackfillMarker(ctx context.Context, conn *sql.Conn, value string) error {
	p := d.dialect.Placeholder
	_, err := conn.ExecContext(ctx,
		"INSERT INTO settings (key, value, updated_at) VALUES ("+p(1)+", "+p(2)+", CURRENT_TIMESTAMP) "+
			"ON CONFLICT(key) DO UPDATE SET value = "+p(3)+", updated_at = CURRENT_TIMESTAMP",
		UsageHourlyBackfillMarker, value, value)
	if err != nil {
		return fmt.Errorf("backfill usage hourly: write marker: %w", err)
	}
	return nil
}
