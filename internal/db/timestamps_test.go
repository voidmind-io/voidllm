package db

import (
	"testing"
	"time"
)

// ---- FormatTimestamp ---------------------------------------------------------

func TestFormatTimestamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{
			name: "already UTC",
			in:   time.Date(2026, 9, 26, 21, 23, 17, 0, time.UTC),
			want: "2026-09-26T21:23:17Z",
		},
		{
			name: "non-UTC input is converted to UTC",
			in:   time.Date(2026, 9, 26, 23, 23, 17, 0, time.FixedZone("CEST", 2*60*60)),
			want: "2026-09-26T21:23:17Z",
		},
		{
			name: "negative offset input is converted to UTC",
			in:   time.Date(2026, 9, 26, 16, 23, 17, 0, time.FixedZone("EST", -5*60*60)),
			want: "2026-09-26T21:23:17Z",
		},
		{
			name: "fractional seconds are truncated, not rounded",
			in:   time.Date(2026, 9, 26, 21, 23, 17, 999_000_000, time.UTC),
			want: "2026-09-26T21:23:17Z",
		},
		{
			name: "zero time",
			in:   time.Time{},
			want: "0001-01-01T00:00:00Z",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := FormatTimestamp(tc.in)
			if got != tc.want {
				t.Errorf("FormatTimestamp(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ---- ParseStoredTimestamp -----------------------------------------------------

func TestParseStoredTimestamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    time.Time
		wantErr bool
	}{
		{
			name: "canonical RFC3339 with Z",
			raw:  "2026-09-26T21:23:17Z",
			want: time.Date(2026, 9, 26, 21, 23, 17, 0, time.UTC),
		},
		{
			name: "RFC3339 with positive offset",
			raw:  "2026-09-26T23:23:17+02:00",
			want: time.Date(2026, 9, 26, 21, 23, 17, 0, time.UTC),
		},
		{
			name: "RFC3339 with negative offset",
			raw:  "2026-09-26T16:23:17-05:00",
			want: time.Date(2026, 9, 26, 21, 23, 17, 0, time.UTC),
		},
		{
			name: "RFC3339Nano with fractional seconds and Z",
			raw:  "2026-09-26T21:23:17.123456789Z",
			want: time.Date(2026, 9, 26, 21, 23, 17, 123456789, time.UTC),
		},
		{
			name: "SQLite CURRENT_TIMESTAMP space-separated form",
			raw:  "2026-09-26 21:23:17",
			want: time.Date(2026, 9, 26, 21, 23, 17, 0, time.UTC),
		},
		{
			name: "PostgreSQL timestamptz text, fractional seconds, +00 offset",
			raw:  "2026-09-26 21:23:17.123456+00",
			want: time.Date(2026, 9, 26, 21, 23, 17, 123456000, time.UTC),
		},
		{
			name: "PostgreSQL timestamptz text, fractional seconds, +00:00 offset",
			raw:  "2026-09-26 21:23:17.123456+00:00",
			want: time.Date(2026, 9, 26, 21, 23, 17, 123456000, time.UTC),
		},
		{
			name: "PostgreSQL timestamptz text, non-UTC +02 offset",
			raw:  "2026-09-26 23:23:17.123456+02",
			want: time.Date(2026, 9, 26, 21, 23, 17, 123456000, time.UTC),
		},
		{
			name: "PostgreSQL timestamptz text, no fractional seconds, +02 offset",
			raw:  "2026-09-26 23:23:17+02",
			want: time.Date(2026, 9, 26, 21, 23, 17, 0, time.UTC),
		},
		{
			name: "PostgreSQL timestamptz text, no fractional seconds, negative offset",
			raw:  "2026-09-26 16:23:17-05",
			want: time.Date(2026, 9, 26, 21, 23, 17, 0, time.UTC),
		},
		{
			name:    "empty string is not parseable",
			raw:     "",
			wantErr: true,
		},
		{
			name:    "garbage is not parseable",
			raw:     "not-a-timestamp",
			wantErr: true,
		},
		{
			name:    "date only, no time component, is not parseable",
			raw:     "2026-09-26",
			wantErr: true,
		},
		{
			name:    "natural language is not parseable",
			raw:     "tomorrow",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseStoredTimestamp(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseStoredTimestamp(%q) error = nil, want error", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseStoredTimestamp(%q) error = %v, want nil", tc.raw, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("ParseStoredTimestamp(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			if got.Location() != time.UTC {
				t.Errorf("ParseStoredTimestamp(%q) location = %v, want UTC", tc.raw, got.Location())
			}
		})
	}
}
