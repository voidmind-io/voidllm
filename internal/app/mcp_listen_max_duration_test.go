package app

// Table-driven tests for mcpListenMaxDuration (routes.go): deriving a
// subscriptions/listen stream's max duration from the hosting app's own
// effective WriteTimeout — see that function's own doc for the exact
// constants (mcpListenMaxDurationCap, mcpListenSafetyMargin,
// mcpListenMinViableDuration) this table exercises.

import (
	"testing"
	"time"
)

func TestMCPListenMaxDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		writeTimeout time.Duration
		want         time.Duration
	}{
		{
			name:         "unlimited WriteTimeout (0) falls back to the 1h cap",
			writeTimeout: 0,
			want:         1 * time.Hour,
		},
		{
			name:         "negative WriteTimeout is treated the same as unlimited",
			writeTimeout: -1 * time.Second,
			want:         1 * time.Hour,
		},
		{
			name:         "120s WriteTimeout leaves a 115s budget after the 5s safety margin",
			writeTimeout: 120 * time.Second,
			want:         115 * time.Second,
		},
		{
			name:         "14s WriteTimeout: a 9s budget falls below the 10s minimum viable floor — refuse",
			writeTimeout: 14 * time.Second,
			want:         0,
		},
		{
			name:         "15s WriteTimeout: exactly the 10s minimum viable floor — the smallest viable budget",
			writeTimeout: 15 * time.Second,
			want:         10 * time.Second,
		},
		{
			name:         "5s WriteTimeout: budget is not even positive — refuse",
			writeTimeout: 5 * time.Second,
			want:         0,
		},
		{
			name:         "1h WriteTimeout leaves a correspondingly large budget, uncapped",
			writeTimeout: 1 * time.Hour,
			want:         1*time.Hour - 5*time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := mcpListenMaxDuration(tc.writeTimeout)
			if got != tc.want {
				t.Errorf("mcpListenMaxDuration(%v) = %v, want %v", tc.writeTimeout, got, tc.want)
			}
		})
	}
}
