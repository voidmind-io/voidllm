package admin

import (
	"testing"

	"github.com/voidmind-io/voidllm/internal/license"
	"github.com/voidmind-io/voidllm/internal/money"
)

func microsPtr(v money.Micros) *money.Micros { return &v }

func TestRejectUnlicensedCostLimits_StripsEchoedValues(t *testing.T) {
	t.Parallel()

	stored := func() (money.Micros, money.Micros, error) { return 5_000_000, 8_000_000, nil }

	tests := []struct {
		name        string
		licensed    bool
		ch          costLimitChange
		wantDaily   *money.Micros
		wantMonthly *money.Micros
	}{
		{"unlicensed echoed non-zero is dropped", false,
			costLimitChange{Daily: microsPtr(5_000_000), Monthly: microsPtr(8_000_000)}, nil, nil},
		{"unlicensed echoed daily with zero monthly keeps the clear", false,
			costLimitChange{Daily: microsPtr(5_000_000), Monthly: microsPtr(0)}, nil, microsPtr(0)},
		{"unlicensed zero is written", false,
			costLimitChange{Daily: microsPtr(0)}, microsPtr(0), nil},
		{"licensed value is written", true,
			costLimitChange{Daily: microsPtr(5_000_000)}, microsPtr(5_000_000), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &Handler{License: license.NewHolder(license.Verify("", tc.licensed))}
			// A zero-only change returns before the stored values are read, so a
			// nil fiber context is never touched on these accepting paths.
			got, sent, err := h.rejectUnlicensedCostLimits(nil, tc.ch, stored, "not found")
			if sent || err != nil {
				t.Fatalf("sent = %v, err = %v, want accepted", sent, err)
			}
			if !samePtr(got.Daily, tc.wantDaily) || !samePtr(got.Monthly, tc.wantMonthly) {
				t.Errorf("got daily %v monthly %v, want %v / %v", got.Daily, got.Monthly, tc.wantDaily, tc.wantMonthly)
			}
		})
	}
}

func samePtr(a, b *money.Micros) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
