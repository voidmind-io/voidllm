package db

import (
	"fmt"
	"time"
)

// TimestampLayout is the canonical on-disk format for every stored event
// timestamp (usage_events.created_at, mcp_tool_calls.created_at,
// api_keys.expires_at): UTC, second precision, ISO-8601 with a literal "Z"
// suffix, e.g. "2026-09-26T21:23:17Z". Every writer in this codebase produces
// this exact shape via FormatTimestamp, and every reader binds query range
// values through the same function, so plain TEXT comparison (<, >=, <=) is
// always correct and index-friendly regardless of the underlying dialect.
const TimestampLayout = "2006-01-02T15:04:05Z"

// FormatTimestamp renders t in the canonical storage format defined by
// TimestampLayout. t is converted to UTC first, so callers never need to call
// t.UTC() themselves. This is the single source of truth for both column
// writes and query bound values (from/to, since, etc.).
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format(TimestampLayout)
}

// storedTimestampLayouts lists every shape that has been written to
// created_at/expires_at columns, tried in order until one parses
// successfully:
//   - RFC3339Nano: the canonical shape itself, and any RFC3339 value with or
//     without a fractional second and with any UTC offset.
//   - "2006-01-02 15:04:05": SQLite's CURRENT_TIMESTAMP output — space
//     separated, always UTC, no offset.
//   - "2006-01-02 15:04:05.999999999-07": PostgreSQL's text rendering of a
//     timestamptz CURRENT_TIMESTAMP default — space separated, fractional
//     seconds, two-digit zone offset without a colon (e.g. "+00").
//   - "2006-01-02 15:04:05.999999999-07:00": the same, with a colon in the
//     zone offset.
//   - "2006-01-02 15:04:05-07": the PostgreSQL form without a fractional
//     second component.
var storedTimestampLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05",
	"2006-01-02 15:04:05.999999999-07",
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05-07",
}

// ParseStoredTimestamp parses raw against every historically observed
// timestamp shape (see storedTimestampLayouts) and returns the result
// normalized to UTC. It exists to make sense of data written before
// FormatTimestamp became the single write path; new writes never produce a
// value ParseStoredTimestamp cannot parse.
func ParseStoredTimestamp(raw string) (time.Time, error) {
	for _, layout := range storedTimestampLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("parse stored timestamp %q: no matching layout", raw)
}

// dayBucketExpr is a portable SQL expression, valid against a TEXT column
// storing the canonical format on both SQLite and PostgreSQL, that extracts
// the calendar day (e.g. "2026-09-26") from a created_at value. Substring
// extraction works because every stored value produced by FormatTimestamp has
// the same fixed width.
const dayBucketExpr = "substr(created_at, 1, 10)"

// hourBucketExpr is the hour-granularity counterpart of dayBucketExpr,
// producing e.g. "2026-09-26T21:00:00Z" from a created_at value.
const hourBucketExpr = "substr(created_at, 1, 13) || ':00:00Z'"

// canonicalTimestampLike is a SQL LIKE pattern (portable across SQLite and
// PostgreSQL — "_" matches exactly one character in both) that matches a TEXT
// value already in the canonical TimestampLayout shape. Used to select rows
// that still need normalization without needing a dialect-specific check.
const canonicalTimestampLike = "____-__-__T__:__:__Z"
