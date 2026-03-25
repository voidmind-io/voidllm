package ratelimit

import (
	"testing"
)

// TestMinNonZero exercises the unexported minNonZero helper through
// EffectiveLimits, but also directly since the test is in the same package.
func TestMinNonZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b int
		want int
	}{
		{name: "both zero returns zero", a: 0, b: 0, want: 0},
		{name: "a non-zero b zero returns a", a: 5, b: 0, want: 5},
		{name: "a zero b non-zero returns b", a: 0, b: 3, want: 3},
		{name: "a smaller than b returns a", a: 5, b: 3, want: 3},
		{name: "b smaller than a returns b", a: 3, b: 5, want: 3},
		{name: "equal values returns either", a: 4, b: 4, want: 4},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := minNonZero(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("minNonZero(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestMinNonZero64(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b int64
		want int64
	}{
		{name: "both zero returns zero", a: 0, b: 0, want: 0},
		{name: "a non-zero b zero returns a", a: 5, b: 0, want: 5},
		{name: "a zero b non-zero returns b", a: 0, b: 3, want: 3},
		{name: "a smaller than b returns a", a: 5, b: 3, want: 3},
		{name: "b smaller than a returns b", a: 3, b: 5, want: 3},
		{name: "equal values returns either", a: 7, b: 7, want: 7},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := minNonZero64(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("minNonZero64(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestEffectiveLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                             string
		keyLimits, teamLimits, orgLimits Limits
		want                             Limits
	}{
		{
			name:       "all zeros means unlimited across all fields",
			keyLimits:  Limits{},
			teamLimits: Limits{},
			orgLimits:  Limits{},
			want:       Limits{},
		},
		{
			name: "key has limits team and org zero, key limits apply",
			keyLimits: Limits{
				RequestsPerMinute: 10,
				RequestsPerDay:    100,
				DailyTokenLimit:   500,
				MonthlyTokenLimit: 5000,
			},
			teamLimits: Limits{},
			orgLimits:  Limits{},
			want: Limits{
				RequestsPerMinute: 10,
				RequestsPerDay:    100,
				DailyTokenLimit:   500,
				MonthlyTokenLimit: 5000,
			},
		},
		{
			name:      "team has limits key and org zero, team limits apply",
			keyLimits: Limits{},
			teamLimits: Limits{
				RequestsPerMinute: 20,
				RequestsPerDay:    200,
				DailyTokenLimit:   1000,
				MonthlyTokenLimit: 10000,
			},
			orgLimits: Limits{},
			want: Limits{
				RequestsPerMinute: 20,
				RequestsPerDay:    200,
				DailyTokenLimit:   1000,
				MonthlyTokenLimit: 10000,
			},
		},
		{
			name:       "org has limits key and team zero, org limits apply",
			keyLimits:  Limits{},
			teamLimits: Limits{},
			orgLimits: Limits{
				RequestsPerMinute: 30,
				RequestsPerDay:    300,
				DailyTokenLimit:   2000,
				MonthlyTokenLimit: 20000,
			},
			want: Limits{
				RequestsPerMinute: 30,
				RequestsPerDay:    300,
				DailyTokenLimit:   2000,
				MonthlyTokenLimit: 20000,
			},
		},
		{
			name:       "key RPM 10 team RPM 5 org RPM 20, effective is 5",
			keyLimits:  Limits{RequestsPerMinute: 10},
			teamLimits: Limits{RequestsPerMinute: 5},
			orgLimits:  Limits{RequestsPerMinute: 20},
			want:       Limits{RequestsPerMinute: 5},
		},
		{
			name:       "key RPM less than team and org, key wins",
			keyLimits:  Limits{RequestsPerMinute: 3},
			teamLimits: Limits{RequestsPerMinute: 7},
			orgLimits:  Limits{RequestsPerMinute: 15},
			want:       Limits{RequestsPerMinute: 3},
		},
		{
			name:       "one field restricted others unlimited",
			keyLimits:  Limits{RequestsPerMinute: 5},
			teamLimits: Limits{},
			orgLimits:  Limits{},
			want: Limits{
				RequestsPerMinute: 5,
				RequestsPerDay:    0,
				DailyTokenLimit:   0,
				MonthlyTokenLimit: 0,
			},
		},
		{
			name:       "mix: key daily=1000 team daily=0 unlimited org daily=500, effective=500",
			keyLimits:  Limits{DailyTokenLimit: 1000},
			teamLimits: Limits{DailyTokenLimit: 0},
			orgLimits:  Limits{DailyTokenLimit: 500},
			want:       Limits{DailyTokenLimit: 500},
		},
		{
			name:       "mix: key daily=300 team daily=0 unlimited org daily=500, key wins",
			keyLimits:  Limits{DailyTokenLimit: 300},
			teamLimits: Limits{DailyTokenLimit: 0},
			orgLimits:  Limits{DailyTokenLimit: 500},
			want:       Limits{DailyTokenLimit: 300},
		},
		{
			name:       "key and team both restrict RPD, smaller wins",
			keyLimits:  Limits{RequestsPerDay: 50},
			teamLimits: Limits{RequestsPerDay: 80},
			orgLimits:  Limits{},
			want:       Limits{RequestsPerDay: 50},
		},
		{
			name:       "monthly token limit: org most restrictive",
			keyLimits:  Limits{MonthlyTokenLimit: 100000},
			teamLimits: Limits{MonthlyTokenLimit: 50000},
			orgLimits:  Limits{MonthlyTokenLimit: 25000},
			want:       Limits{MonthlyTokenLimit: 25000},
		},
		{
			name: "all scopes set on all fields, smallest per field wins",
			keyLimits: Limits{
				RequestsPerMinute: 100,
				RequestsPerDay:    500,
				DailyTokenLimit:   10000,
				MonthlyTokenLimit: 100000,
			},
			teamLimits: Limits{
				RequestsPerMinute: 50,
				RequestsPerDay:    1000,
				DailyTokenLimit:   5000,
				MonthlyTokenLimit: 200000,
			},
			orgLimits: Limits{
				RequestsPerMinute: 200,
				RequestsPerDay:    200,
				DailyTokenLimit:   20000,
				MonthlyTokenLimit: 50000,
			},
			want: Limits{
				RequestsPerMinute: 50,
				RequestsPerDay:    200,
				DailyTokenLimit:   5000,
				MonthlyTokenLimit: 50000,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := EffectiveLimits(tc.keyLimits, tc.teamLimits, tc.orgLimits)
			if got != tc.want {
				t.Errorf("EffectiveLimits() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
