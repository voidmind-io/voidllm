package redis

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// newBudgetTestClient returns a Client with prefix "t:" on a fresh miniredis
// whose clock is fixed at now.
func newBudgetTestClient(t *testing.T, now time.Time) (*Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	mr.SetTime(now)
	c, err := New("redis://"+mr.Addr(), "t")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

func TestBudgetKeyLayout(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 3, 23, 30, 0, 0, time.FixedZone("west", -5*3600)) // Nov 4th 04:30 UTC
	if got := BudgetDayBucket(at); got != "d:2026-11-04" {
		t.Errorf("BudgetDayBucket() = %q, want d:2026-11-04 (UTC day)", got)
	}
	if got := BudgetMonthBucket(at); got != "m:2026-11" {
		t.Errorf("BudgetMonthBucket() = %q, want m:2026-11", got)
	}

	c := &Client{keyPrefix: "voidllm:"}
	tests := []struct {
		metric, bucket, scope, want string
	}{
		{BudgetMetricTokens, "d:2026-11-04", "key:k1", "voidllm:budget:tok:d:2026-11-04:key:k1"},
		{BudgetMetricCost, "m:2026-11", "user:org1:user1", "voidllm:budget:cost:m:2026-11:user:org1:user1"},
		{BudgetMetricTokens, "m:2026-11", "team:t1", "voidllm:budget:tok:m:2026-11:team:t1"},
		{BudgetMetricCost, "d:2026-11-04", "org:o1", "voidllm:budget:cost:d:2026-11-04:org:o1"},
	}
	for _, tc := range tests {
		if got := c.BudgetKey(tc.metric, tc.bucket, tc.scope); got != tc.want {
			t.Errorf("BudgetKey(%q, %q, %q) = %q, want %q", tc.metric, tc.bucket, tc.scope, got, tc.want)
		}
	}
}

func TestSetAndSumBudgets(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	c, mr := newBudgetTestClient(t, now)
	ctx := context.Background()

	a := c.BudgetKey(BudgetMetricTokens, "d:2026-03-10", "org:o")
	b := c.BudgetKey(BudgetMetricCost, "m:2026-03", "org:o")
	missing := c.BudgetKey(BudgetMetricTokens, "d:2026-03-10", "org:none")
	expire := now.Add(36 * time.Hour)

	if err := c.SetBudgets(ctx, "inst-1", nil); err != nil {
		t.Fatalf("SetBudgets(nil) error = %v", err)
	}
	if sums, err := c.SumBudgets(ctx, nil); err != nil || sums != nil {
		t.Errorf("SumBudgets(nil) = %v, %v, want nil, nil", sums, err)
	}

	set := func(instance string, tokens, cost int64) {
		t.Helper()
		err := c.SetBudgets(ctx, instance, []BudgetValue{
			{Key: a, Value: tokens, ExpireAt: expire},
			{Key: b, Value: cost, ExpireAt: expire},
		})
		if err != nil {
			t.Fatalf("SetBudgets(%s) error = %v", instance, err)
		}
	}
	assertSums := func(stage string, wantTokens, wantCost int64) {
		t.Helper()
		got, err := c.SumBudgets(ctx, []string{missing, a, b})
		if err != nil {
			t.Fatalf("%s: SumBudgets() error = %v", stage, err)
		}
		want := []int64{0, wantTokens, wantCost}
		if len(got) != len(want) {
			t.Fatalf("%s: SumBudgets() = %v, want %v", stage, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: SumBudgets()[%d] = %d, want %d", stage, i, got[i], want[i])
			}
		}
	}

	set("inst-1", 40, 1_000_000_000_000)
	assertSums("one instance", 40, 1_000_000_000_000)

	// Writing the same absolute value again changes nothing.
	set("inst-1", 40, 1_000_000_000_000)
	assertSums("repeated write", 40, 1_000_000_000_000)

	// A newer value replaces the instance's field; another instance adds up.
	set("inst-1", 55, 1_000_000_000_500)
	set("inst-2", 10, 2_000_000_000_000)
	assertSums("two instances", 65, 3_000_000_000_500)

	if got := mr.HGet(a, "inst-1"); got != "55" {
		t.Errorf("field inst-1 = %q, want 55", got)
	}
	if ttl := mr.TTL(a); ttl != 36*time.Hour {
		t.Errorf("TTL = %v, want 36h", ttl)
	}

	// A value that is not a number counts as 0, the other fields still add up.
	mr.HSet(a, "broken", "not-a-number")
	assertSums("non-numeric field", 65, 3_000_000_000_500)
}

func TestSumBudgetsMalformedCounterReadsAsZero(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	c, mr := newBudgetTestClient(t, now)
	ctx := context.Background()

	good := c.BudgetKey(BudgetMetricTokens, "d:2026-03-10", "org:o")
	wrongType := c.BudgetKey(BudgetMetricCost, "d:2026-03-10", "org:o")
	badField := c.BudgetKey(BudgetMetricCost, "m:2026-03", "org:o")

	expire := now.Add(36 * time.Hour)
	if err := c.SetBudgets(ctx, "inst-1", []BudgetValue{{Key: good, Value: 42, ExpireAt: expire}}); err != nil {
		t.Fatalf("SetBudgets: %v", err)
	}
	if err := mr.Set(wrongType, "not a hash"); err != nil {
		t.Fatalf("miniredis Set: %v", err)
	}
	mr.HSet(badField, "inst-1", "oops", "inst-2", "7")

	sums, err := c.SumBudgets(ctx, []string{wrongType, good, badField})
	if err != nil {
		t.Fatalf("SumBudgets error = %v, want nil", err)
	}
	want := []int64{0, 42, 7}
	for i := range want {
		if sums[i] != want[i] {
			t.Errorf("sums[%d] = %d, want %d (all: %v)", i, sums[i], want[i], sums)
		}
	}
}
