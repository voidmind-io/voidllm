package mcp_test

import (
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// ---- Valid -------------------------------------------------------------------

func TestVersion_Valid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		v    mcp.Version
		want bool
	}{
		{"2025-03-26 is valid", mcp.V20250326, true},
		{"2025-06-18 is valid", mcp.V20250618, true},
		{"2025-11-25 is valid", mcp.V20251125, true},
		{"2026-07-28 is valid", mcp.V20260728, true},
		{"empty string is invalid", mcp.Version(""), false},
		{"well-formed but unknown date is invalid", mcp.Version("2025-03-27"), false},
		{"a revision VoidLLM has not been built against is invalid", mcp.Version("2027-01-01"), false},
		{"non-date garbage is invalid", mcp.Version("not-a-version"), false},
		{"case-sensitive: uppercase does not match", mcp.Version("2026-07-28X"), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.v.Valid(); got != tc.want {
				t.Errorf("Version(%q).Valid() = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

// ---- Compare -------------------------------------------------------------------

func TestVersion_Compare(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a    mcp.Version
		b    mcp.Version
		want int // sign only: -1, 0, or 1
	}{
		{"equal versions compare zero", mcp.V20260728, mcp.V20260728, 0},
		{"older sorts before newer", mcp.V20250326, mcp.V20260728, -1},
		{"newer sorts after older", mcp.V20260728, mcp.V20250326, 1},
		{"adjacent revisions: 2025-06-18 before 2025-11-25", mcp.V20250618, mcp.V20251125, -1},
		{"adjacent revisions: 2025-11-25 after 2025-06-18", mcp.V20251125, mcp.V20250618, 1},
		{"unknown versions still compare lexicographically", mcp.Version("2020-01-01"), mcp.V20250326, -1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tc.a.Compare(tc.b)
			gotSign := sign(got)
			if gotSign != tc.want {
				t.Errorf("Version(%q).Compare(%q) sign = %d, want %d (raw=%d)", tc.a, tc.b, gotSign, tc.want, got)
			}
		})
	}
}

// sign normalizes an integer comparison result to -1, 0, or 1.
func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// ---- Era -------------------------------------------------------------------

func TestVersion_Era(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		v    mcp.Version
		want mcp.Era
	}{
		{"2025-03-26 is legacy", mcp.V20250326, mcp.EraLegacy},
		{"2025-06-18 is legacy", mcp.V20250618, mcp.EraLegacy},
		{"2025-11-25 is legacy", mcp.V20251125, mcp.EraLegacy},
		{"2026-07-28 is modern", mcp.V20260728, mcp.EraModern},
		{
			name: "an unrecognized but chronologically later date is still classified modern",
			v:    mcp.Version("2027-01-01"),
			want: mcp.EraModern,
		},
		{
			name: "an unrecognized but chronologically earlier date is still classified legacy",
			v:    mcp.Version("2020-01-01"),
			want: mcp.EraLegacy,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.v.Era(); got != tc.want {
				t.Errorf("Version(%q).Era() = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

// ---- SupportedVersions -------------------------------------------------------

func TestSupportedVersions(t *testing.T) {
	t.Parallel()

	got := mcp.SupportedVersions()

	want := []mcp.Version{mcp.V20260728, mcp.V20251125, mcp.V20250618, mcp.V20250326}
	if len(got) != len(want) {
		t.Fatalf("SupportedVersions() len = %d, want %d", len(got), len(want))
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("SupportedVersions()[%d] = %q, want %q (must be newest-first)", i, got[i], v)
		}
	}

	// Every supported version must itself be Valid.
	for _, v := range got {
		if !v.Valid() {
			t.Errorf("SupportedVersions() contains %q which is not Valid()", v)
		}
	}
}

func TestSupportedVersions_MutationDoesNotAffectSubsequentCalls(t *testing.T) {
	t.Parallel()

	first := mcp.SupportedVersions()
	first[0] = mcp.Version("mutated")

	second := mcp.SupportedVersions()
	if second[0] != mcp.V20260728 {
		t.Errorf("SupportedVersions()[0] after mutating a previous call's slice = %q, want %q (each call must return an independent slice)", second[0], mcp.V20260728)
	}
}
