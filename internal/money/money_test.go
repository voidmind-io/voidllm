package money

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    Micros
		wantErr bool
	}{
		{"0", 0, false},
		{"0.0", 0, false},
		{"0.5", 500_000, false},
		{".5", 500_000, false},
		{"25", 25_000_000, false},
		{"25.00", 25_000_000, false},
		{"025", 25_000_000, false},
		{"0.000001", 1, false},
		{"25.123456", 25_123_456, false},
		{"1000000000", MaxMicros, false},
		{"1000000000.000000", MaxMicros, false},
		{"", 0, true},
		{".", 0, true},
		{"25.", 0, true},
		{"1e3", 0, true},
		{"1E3", 0, true},
		{"-1", 0, true},
		{"-0", 0, true},
		{"+1", 0, true},
		{" 1", 0, true},
		{"1 ", 0, true},
		{"1,5", 0, true},
		{"1,000", 0, true},
		{"1.2.3", 0, true},
		{"abc", 0, true},
		{"0.0000001", 0, true},
		{"1000000000.000001", 0, true},
		{"1000000001", 0, true},
		{"99999999999999999999", 0, true},
		{"NaN", 0, true},
		{"Inf", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Parse(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalid) {
				t.Errorf("Parse(%q) error %v does not wrap ErrInvalid", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Parse(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   Micros
		want string
	}{
		{0, "0.00"},
		{1, "0.000001"},
		{500_000, "0.50"},
		{25_000_000, "25.00"},
		{25_100_000, "25.10"},
		{25_123_456, "25.123456"},
		{123, "0.000123"},
		{MaxMicros, "1000000000.00"},
		{-1_500_000, "-1.50"},
	}
	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Micros(%d).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFromCost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   float64
		want Micros
	}{
		{"zero", 0, 0},
		{"negative", -1.5, 0},
		{"negative zero", math.Copysign(0, -1), 0},
		{"nan", math.NaN(), 0},
		{"positive infinity", math.Inf(1), MaxMicros},
		{"negative infinity", math.Inf(-1), 0},
		{"one micro", 0.000001, 1},
		{"rounds up", 0.0000016, 2},
		{"rounds down", 0.0000014, 1},
		{"major units", 25, 25_000_000},
		{"float noise", 0.000123, 123},
		{"beyond int64", 1e300, MaxMicros},
		{"just above max", float64(MaxUnits) + 1, MaxMicros},
		{"exactly max", float64(MaxUnits), MaxMicros},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := FromCost(tc.in); got != tc.want {
				t.Errorf("FromCost(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestFromCostRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()

	for _, in := range []float64{1e300, math.Inf(1), float64(MaxUnits) * 2, 25.5} {
		out, err := json.Marshal(FromCost(in))
		if err != nil {
			t.Fatalf("marshal FromCost(%v): %v", in, err)
		}
		var back Micros
		if err := json.Unmarshal(out, &back); err != nil {
			t.Fatalf("FromCost(%v) = %s does not round-trip: %v", in, out, err)
		}
		if back != FromCost(in) {
			t.Errorf("FromCost(%v) round-tripped to %d, want %d", in, back, FromCost(in))
		}
	}
}

func TestJSON(t *testing.T) {
	t.Parallel()

	out, err := json.Marshal(struct {
		A Micros `json:"a"`
	}{A: 25_500_000})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(out) != `{"a":"25.50"}` {
		t.Errorf("Marshal = %s, want {\"a\":\"25.50\"}", out)
	}

	tests := []struct {
		in      string
		want    Micros
		wantErr bool
	}{
		{`"25.00"`, 25_000_000, false},
		{`"0"`, 0, false},
		{`25`, 25_000_000, false},
		{`0.5`, 500_000, false},
		{`25.000001`, 25_000_001, false},
		{`1e3`, 0, true},
		{`-1`, 0, true},
		{`"-1"`, 0, true},
		{`"abc"`, 0, true},
		{`""`, 0, true},
		{`true`, 0, true},
		{`[]`, 0, true},
		{`{}`, 0, true},
		{`0.0000001`, 0, true},
	}
	for _, tc := range tests {
		var got Micros
		err := json.Unmarshal([]byte(tc.in), &got)
		if (err != nil) != tc.wantErr {
			t.Errorf("Unmarshal(%s) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("Unmarshal(%s) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func FuzzParseRoundTrip(f *testing.F) {
	for _, seed := range []string{"0", "0.5", "25.00", "1e3", "-1", "1000000000", ".5", "0.000001", "9999999999999999999"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		m, err := Parse(s)
		if err != nil {
			return
		}
		if m < 0 || m > MaxMicros {
			t.Fatalf("Parse(%q) = %d, out of range", s, m)
		}
		again, err := Parse(m.String())
		if err != nil {
			t.Fatalf("Parse(%q) = %d but String() %q does not parse: %v", s, m, m.String(), err)
		}
		if again != m {
			t.Fatalf("round trip of %q: %d -> %q -> %d", s, m, m.String(), again)
		}
	})
}
